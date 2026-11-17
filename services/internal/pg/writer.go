package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/lib/pq"

	"github.com/bindle-ai/bindle/services/internal/model"
)

// Writer inserts normalized steps and maintains the per-trace rollup. It
// implements pipeline.Sink. Connect it as the writer role.
type Writer struct {
	DB  *sql.DB
	Log *slog.Logger
	// Retention bounds accepted step times: steps older than this, or more
	// than a day in the future, are dropped (no partition exists for them).
	Retention time.Duration
	Now       func() time.Time

	parts sync.Map // "2006-01-02" -> struct{}: partitions known to exist
}

const maxFutureSkew = 24 * time.Hour

// insertSQL inserts a batch of one tenant's steps from parallel arrays and
// folds the newly inserted rows (duplicates are skipped) into bindle.traces.
// Rollup rows are processed in trace_id order so concurrent writers take
// row locks in the same order.
const insertSQL = `
WITH ins AS (
    INSERT INTO bindle.steps (
        tenant_id, trace_id, step_id, parent_step_id, name, type, status, attempt,
        framework, service_name, agent_name, agent_version, session_id, environment,
        provider, model, tool_name, input_tokens, output_tokens, cached_tokens,
        cost_usd, cost_known, start_time, start_ns, duration_ms,
        error_message, input, output, reasoning, attributes, links)
    SELECT u.tenant_id, u.trace_id, u.step_id, u.parent_step_id, u.name, u.type, u.status, u.attempt,
        u.framework, u.service_name, u.agent_name, u.agent_version, u.session_id, u.environment,
        u.provider, u.model, u.tool_name, u.input_tokens, u.output_tokens, u.cached_tokens,
        u.cost_usd, u.cost_known,
        timestamptz 'epoch' + (u.start_ns / 1000) * interval '1 microsecond', u.start_ns, u.duration_ms,
        u.error_message, u.input, u.output, u.reasoning, u.attributes::jsonb, u.links::jsonb
    FROM unnest(
        $1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::bigint[],
        $9::text[], $10::text[], $11::text[], $12::text[], $13::text[], $14::text[],
        $15::text[], $16::text[], $17::text[], $18::bigint[], $19::bigint[], $20::bigint[],
        $21::float8[], $22::bool[], $23::bigint[], $24::float8[],
        $25::text[], $26::text[], $27::text[], $28::text[], $29::text[], $30::text[]
    ) AS u(tenant_id, trace_id, step_id, parent_step_id, name, type, status, attempt,
        framework, service_name, agent_name, agent_version, session_id, environment,
        provider, model, tool_name, input_tokens, output_tokens, cached_tokens,
        cost_usd, cost_known, start_ns, duration_ms,
        error_message, input, output, reasoning, attributes, links)
    ON CONFLICT DO NOTHING
    RETURNING tenant_id, trace_id, parent_step_id, type, status, model, cost_known, cost_usd,
        input_tokens, output_tokens, agent_name, agent_version, framework, environment,
        session_id, start_time, start_ns, duration_ms
)
INSERT INTO bindle.traces AS t (
    tenant_id, trace_id, first_start, first_start_ns, steps, failed_steps, cost_unknown_steps,
    input_tokens, output_tokens, cost_usd, agent_name, agent_version, framework, environment,
    session_id, root_status, root_duration_ms)
SELECT tenant_id, trace_id, min(start_time), min(start_ns), count(*),
    count(*) FILTER (WHERE status = 'failure'),
    count(*) FILTER (WHERE NOT cost_known AND (model <> '' OR type = 'llm_call')),
    sum(input_tokens), sum(output_tokens), sum(cost_usd),
    coalesce(max(agent_name) FILTER (WHERE agent_name <> ''), ''),
    coalesce(max(agent_version) FILTER (WHERE agent_version <> ''), ''),
    coalesce(max(framework) FILTER (WHERE framework NOT IN ('', 'unknown')), ''),
    coalesce(max(environment) FILTER (WHERE environment <> ''), ''),
    coalesce(max(session_id) FILTER (WHERE session_id <> ''), ''),
    coalesce(max(status) FILTER (WHERE parent_step_id = ''), ''),
    coalesce(max(duration_ms) FILTER (WHERE parent_step_id = ''), 0)
FROM ins
GROUP BY tenant_id, trace_id
ORDER BY trace_id
ON CONFLICT (tenant_id, trace_id) DO UPDATE SET
    first_start        = least(t.first_start, excluded.first_start),
    first_start_ns     = least(t.first_start_ns, excluded.first_start_ns),
    steps              = t.steps + excluded.steps,
    failed_steps       = t.failed_steps + excluded.failed_steps,
    cost_unknown_steps = t.cost_unknown_steps + excluded.cost_unknown_steps,
    input_tokens       = t.input_tokens + excluded.input_tokens,
    output_tokens      = t.output_tokens + excluded.output_tokens,
    cost_usd           = t.cost_usd + excluded.cost_usd,
    agent_name         = greatest(t.agent_name, excluded.agent_name),
    agent_version      = greatest(t.agent_version, excluded.agent_version),
    framework          = greatest(t.framework, excluded.framework),
    environment        = greatest(t.environment, excluded.environment),
    session_id         = greatest(t.session_id, excluded.session_id),
    root_status        = greatest(t.root_status, excluded.root_status),
    root_duration_ms   = greatest(t.root_duration_ms, excluded.root_duration_ms),
    updated_at         = now()`

// Write implements pipeline.Sink. Steps are written per tenant, each in its
// own transaction scoped to that tenant by row-level security. Retrying a
// whole batch after a partial failure is safe: inserts are idempotent.
func (w *Writer) Write(ctx context.Context, steps []model.Step) error {
	byTenant := map[string][]model.Step{}
	var order []string
	for _, s := range steps {
		if s.TenantID == "" {
			continue // never write a step without a tenant
		}
		if _, ok := byTenant[s.TenantID]; !ok {
			order = append(order, s.TenantID)
		}
		byTenant[s.TenantID] = append(byTenant[s.TenantID], s)
	}
	now := w.now()
	for _, tenant := range order {
		kept, days, dropped := w.window(byTenant[tenant], now)
		if dropped > 0 {
			w.logger().Warn("dropping steps outside the accepted time window",
				"tenant", tenant, "dropped", dropped, "retention", w.retention().String())
		}
		if len(kept) == 0 {
			continue
		}
		if err := w.ensurePartitions(ctx, days); err != nil {
			return err
		}
		if err := w.writeTenant(ctx, tenant, kept); err != nil {
			return fmt.Errorf("pg: write tenant %s: %w", tenant, err)
		}
	}
	return nil
}

func (w *Writer) writeTenant(ctx context.Context, tenant string, steps []model.Step) error {
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('bindle.tenant', $1, true), set_config('statement_timeout', '15000', true)`, tenant); err != nil {
		return err
	}
	cols, err := columns(steps)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, insertSQL, cols...); err != nil {
		return err
	}
	return tx.Commit()
}

// window keeps steps whose start time has a partition: within retention
// and at most a day ahead. Returns kept steps, their distinct UTC days, and
// the number dropped.
func (w *Writer) window(steps []model.Step, now time.Time) ([]model.Step, []string, int) {
	oldest := now.Add(-w.retention())
	newest := now.Add(maxFutureSkew)
	kept := steps[:0:0]
	seen := map[string]bool{}
	var days []string
	for _, s := range steps {
		t := time.Unix(0, int64(s.StartUnixNano)).UTC()
		if t.Before(oldest) || t.After(newest) {
			continue
		}
		kept = append(kept, s)
		d := t.Format("2006-01-02")
		if !seen[d] {
			seen[d] = true
			days = append(days, d)
		}
	}
	return kept, days, len(steps) - len(kept)
}

func (w *Writer) ensurePartitions(ctx context.Context, days []string) error {
	for _, d := range days {
		if _, ok := w.parts.Load(d); ok {
			continue
		}
		if _, err := w.DB.ExecContext(ctx, `SELECT bindle.ensure_steps_partition($1::date)`, d); err != nil {
			return fmt.Errorf("pg: ensure partition %s: %w", d, err)
		}
		w.parts.Store(d, struct{}{})
	}
	return nil
}

func (w *Writer) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *Writer) retention() time.Duration {
	if w.Retention > 0 {
		return w.Retention
	}
	return 30 * 24 * time.Hour
}

func (w *Writer) logger() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

// clean makes a string storable: PostgreSQL text rejects NUL bytes and
// invalid UTF-8, and one bad value would fail the whole batch.
func clean(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "�")
	}
	return s
}

// columns converts steps to the 30 parallel arrays insertSQL expects.
func columns(steps []model.Step) ([]any, error) {
	n := len(steps)
	text := func() []string { return make([]string, n) }
	var (
		tenant, trace, step, parent, name, typ, status                    = text(), text(), text(), text(), text(), text(), text()
		framework, service, agent, version, session, env                  = text(), text(), text(), text(), text(), text()
		provider, mdl, tool, errMsg, input, output, reasoning, attrs, lnk = text(), text(), text(), text(), text(), text(), text(), text(), text()
		attempt, inTok, outTok, cachedTok, startNs                        = make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
		cost, dur                                                         = make([]float64, n), make([]float64, n)
		costKnown                                                         = make([]bool, n)
	)
	for i, s := range steps {
		tenant[i], trace[i], step[i], parent[i] = clean(s.TenantID), clean(s.TraceID), clean(s.StepID), clean(s.ParentStepID)
		name[i], typ[i], status[i], attempt[i] = clean(s.Name), clean(s.Type), clean(s.Status), s.Attempt
		framework[i], service[i], agent[i], version[i] = clean(s.Framework), clean(s.ServiceName), clean(s.AgentName), clean(s.AgentVersion)
		session[i], env[i], provider[i], mdl[i], tool[i] = clean(s.SessionID), clean(s.Environment), clean(s.Provider), clean(s.Model), clean(s.ToolName)
		inTok[i], outTok[i], cachedTok[i] = s.InputTokens, s.OutputTokens, s.CachedTokens
		cost[i], costKnown[i], startNs[i], dur[i] = s.CostUSD, s.CostKnown, int64(s.StartUnixNano), s.DurationMs
		errMsg[i], input[i], output[i], reasoning[i] = clean(s.ErrorMessage), clean(s.Input), clean(s.Output), clean(s.Reasoning)

		a := make(map[string]string, len(s.Attributes))
		for k, v := range s.Attributes {
			a[clean(k)] = clean(v)
		}
		ab, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		links := make([]string, 0, len(s.Links))
		for _, l := range s.Links {
			links = append(links, clean(l))
		}
		lb, err := json.Marshal(links)
		if err != nil {
			return nil, err
		}
		attrs[i], lnk[i] = string(ab), string(lb)
	}
	return []any{
		pq.Array(tenant), pq.Array(trace), pq.Array(step), pq.Array(parent), pq.Array(name), pq.Array(typ), pq.Array(status), pq.Array(attempt),
		pq.Array(framework), pq.Array(service), pq.Array(agent), pq.Array(version), pq.Array(session), pq.Array(env),
		pq.Array(provider), pq.Array(mdl), pq.Array(tool), pq.Array(inTok), pq.Array(outTok), pq.Array(cachedTok),
		pq.Array(cost), pq.Array(costKnown), pq.Array(startNs), pq.Array(dur),
		pq.Array(errMsg), pq.Array(input), pq.Array(output), pq.Array(reasoning), pq.Array(attrs), pq.Array(lnk),
	}, nil
}

// Maintain keeps step partitions ahead of time and applies retention, now
// and then every interval, until ctx ends. Safe to run on every ingest task.
func (w *Writer) Maintain(ctx context.Context, interval time.Duration) {
	run := func() {
		now := w.now()
		for d := -1; d <= 2; d++ {
			day := now.AddDate(0, 0, d).Format("2006-01-02")
			if _, err := w.DB.ExecContext(ctx, `SELECT bindle.ensure_steps_partition($1::date)`, day); err != nil {
				w.logger().Error("partition maintenance failed", "day", day, "err", err)
				continue
			}
			w.parts.Store(day, struct{}{})
		}
		cutoff := now.Add(-w.retention()).Format("2006-01-02")
		var dropped int
		if err := w.DB.QueryRowContext(ctx, `SELECT bindle.apply_retention($1::date)`, cutoff).Scan(&dropped); err != nil {
			w.logger().Error("retention failed", "cutoff", cutoff, "err", err)
		} else if dropped > 0 {
			w.logger().Info("retention dropped step partitions", "count", dropped, "cutoff", cutoff)
		}
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
