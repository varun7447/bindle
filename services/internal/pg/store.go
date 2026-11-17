package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/bindle-ai/bindle/services/internal/query"
)

// Store implements query.Store. Connect it as the reader role.
type Store struct {
	DB *sql.DB
	// StatementTimeout bounds every query; default 10s.
	StatementTimeout time.Duration
}

var _ query.Store = (*Store)(nil)

// withTenant runs fn in a read-only transaction whose row-level security
// scope is tenant. Every query also filters tenant_id explicitly.
func (s *Store) withTenant(ctx context.Context, tenant string, fn func(*sql.Tx) error) error {
	if tenant == "" {
		return query.ErrNoTenant
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	timeout := s.StatementTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('bindle.tenant', $1, true), set_config('statement_timeout', $2, true)`,
		tenant, strconv.FormatInt(timeout.Milliseconds(), 10)); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func nsTime(ns int64) query.Time { return query.Time{Time: time.Unix(0, ns).UTC()} }

// args collects positional parameters; Add returns the placeholder.
type args []any

func (a *args) Add(v any) string {
	*a = append(*a, v)
	return "$" + strconv.Itoa(len(*a))
}

// ---- runs ----------------------------------------------------------------

const runCols = `trace_id, first_start_ns, steps, failed_steps, cost_usd, cost_unknown_steps,
    input_tokens, output_tokens, agent_name, agent_version, framework, environment,
    session_id, root_status, root_duration_ms`

func scanRun(sc interface{ Scan(...any) error }) (query.Run, error) {
	var r query.Run
	var ns int64
	err := sc.Scan(&r.TraceID, &ns, &r.Steps, &r.FailedSteps, &r.CostUSD, &r.CostUnknownSteps,
		&r.InputTokens, &r.OutputTokens, &r.Agent, &r.AgentVersion, &r.Framework, &r.Environment,
		&r.SessionID, &r.RootStatus, &r.DurationMs)
	r.Started = nsTime(ns)
	return r, err
}

// runsSQL builds the runs-list query from constant fragments. Only
// placeholders vary with the filter; values travel as parameters.
func runsSQL(tenant string, f query.RunFilter) (string, args) {
	var a args
	var b strings.Builder
	b.WriteString("SELECT " + runCols + "\nFROM bindle.traces\nWHERE tenant_id = " + a.Add(tenant))
	b.WriteString(" AND first_start_ns >= " + a.Add(f.From.UnixNano()) + " AND first_start_ns < " + a.Add(f.To.UnixNano()))
	if f.Agent != "" {
		b.WriteString(" AND agent_name = " + a.Add(f.Agent))
	}
	if f.Environment != "" {
		b.WriteString(" AND environment = " + a.Add(f.Environment))
	}
	switch f.Status {
	case "failed":
		b.WriteString(" AND (failed_steps > 0 OR root_status = 'failure')")
	case "ok":
		b.WriteString(" AND failed_steps = 0 AND root_status <> 'failure'")
	}
	if f.AfterTrace != "" {
		b.WriteString(" AND (first_start_ns, trace_id) < (" + a.Add(f.AfterStart.UnixNano()) + ", " + a.Add(f.AfterTrace) + ")")
	}
	b.WriteString("\nORDER BY first_start_ns DESC, trace_id DESC\nLIMIT " + a.Add(f.Limit))
	return b.String(), a
}

// ListRuns implements query.Store.
func (s *Store) ListRuns(ctx context.Context, tenant string, f query.RunFilter) ([]query.Run, error) {
	var out []query.Run
	err := s.withTenant(ctx, tenant, func(tx *sql.Tx) error {
		sqlText, a := runsSQL(tenant, f)
		rows, err := tx.QueryContext(ctx, sqlText, a...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRun(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// ---- trace ---------------------------------------------------------------

const stepSummaryCols = `step_id, parent_step_id, name, type, status, attempt, start_ns, duration_ms,
    provider, model, tool_name, input_tokens, output_tokens, cached_tokens, cost_usd, cost_known,
    error_message, octet_length(input), octet_length(output), octet_length(reasoning)`

func stepSummaryDest(st *query.StepSummary, ns *int64) []any {
	return []any{&st.StepID, &st.ParentStepID, &st.Name, &st.Type, &st.Status, &st.Attempt, ns, &st.DurationMs,
		&st.Provider, &st.Model, &st.ToolName, &st.InputTokens, &st.OutputTokens, &st.CachedTokens, &st.CostUSD, &st.CostKnown,
		&st.ErrorMessage, &st.InputBytes, &st.OutputBytes, &st.ReasoningBytes}
}

const traceStepsSQL = `SELECT ` + stepSummaryCols + `,
    strpos(input, '[REDACTED:') > 0 OR strpos(output, '[REDACTED:') > 0
      OR strpos(reasoning, '[REDACTED:') > 0 OR strpos(error_message, '[REDACTED:') > 0
FROM bindle.steps
WHERE tenant_id = $1 AND trace_id = $2
ORDER BY start_ns, step_id
LIMIT $3`

// GetTrace implements query.Store; nil, nil when the trace is not found.
func (s *Store) GetTrace(ctx context.Context, tenant, traceID string, maxSteps int) (*query.Trace, error) {
	var t *query.Trace
	err := s.withTenant(ctx, tenant, func(tx *sql.Tx) error {
		run, err := scanRun(tx.QueryRowContext(ctx, "SELECT "+runCols+" FROM bindle.traces WHERE tenant_id = $1 AND trace_id = $2", tenant, traceID))
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, traceStepsSQL, tenant, traceID, maxSteps+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		t = &query.Trace{Run: run}
		for rows.Next() {
			var st query.StepSummary
			var ns int64
			if err := rows.Scan(append(stepSummaryDest(&st, &ns), &st.Redacted)...); err != nil {
				return err
			}
			st.StartTime = nsTime(ns)
			t.Steps = append(t.Steps, st)
		}
		if len(t.Steps) > maxSteps {
			t.Steps, t.Truncated = t.Steps[:maxSteps], true
		}
		return rows.Err()
	})
	return t, err
}

const stepDetailSQL = `SELECT ` + stepSummaryCols + `,
    trace_id, framework, input, output, reasoning, attributes, links,
    ARRAY(SELECT DISTINCT m[1] FROM regexp_matches(
        input || E'\n' || output || E'\n' || reasoning || E'\n' || error_message,
        '\[REDACTED:([A-Z0-9_]+)\]', 'g') AS m ORDER BY 1)
FROM bindle.steps
WHERE tenant_id = $1 AND trace_id = $2 AND step_id = $3
LIMIT 1`

// GetStep implements query.Store; nil, nil when the step is not found.
func (s *Store) GetStep(ctx context.Context, tenant, traceID, stepID string) (*query.StepDetail, error) {
	var d *query.StepDetail
	err := s.withTenant(ctx, tenant, func(tx *sql.Tx) error {
		var sd query.StepDetail
		var ns int64
		var attrs, links []byte
		var kinds pq.StringArray
		dest := append(stepSummaryDest(&sd.StepSummary, &ns),
			&sd.TraceID, &sd.Framework, &sd.Input, &sd.Output, &sd.Reasoning, &attrs, &links, &kinds)
		err := tx.QueryRowContext(ctx, stepDetailSQL, tenant, traceID, stepID).Scan(dest...)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		sd.StartTime = nsTime(ns)
		if err := json.Unmarshal(attrs, &sd.Attributes); err != nil {
			return err
		}
		if err := json.Unmarshal(links, &sd.Links); err != nil {
			return err
		}
		sd.RedactedKinds = []string(kinds)
		sd.Redacted = len(sd.RedactedKinds) > 0
		d = &sd
		return nil
	})
	return d, err
}

// ---- overview ------------------------------------------------------------

const overviewKPISQL = `SELECT count(*),
    count(*) FILTER (WHERE failed_steps > 0 OR root_status = 'failure'),
    coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY root_duration_ms), 0),
    coalesce(percentile_cont(0.95) WITHIN GROUP (ORDER BY root_duration_ms), 0),
    coalesce(sum(cost_usd), 0), coalesce(sum(cost_unknown_steps), 0)::bigint,
    coalesce(sum(input_tokens), 0)::bigint, coalesce(sum(output_tokens), 0)::bigint,
    coalesce(sum(steps), 0)::bigint, coalesce(sum(failed_steps), 0)::bigint
FROM bindle.traces
WHERE tenant_id = $1 AND first_start_ns >= $2 AND first_start_ns < $3`

// Buckets align to multiples of the bucket size since the Unix epoch.
const overviewSeriesSQL = `SELECT (first_start_ns / $4) * $4 AS bucket_ns, count(*),
    count(*) FILTER (WHERE failed_steps > 0 OR root_status = 'failure'),
    coalesce(sum(cost_usd), 0)
FROM bindle.traces
WHERE tenant_id = $1 AND first_start_ns >= $2 AND first_start_ns < $3
GROUP BY 1
ORDER BY 1`

const topFailingToolsSQL = `SELECT tool_name, count(*) FILTER (WHERE status = 'failure') AS failures, count(*)
FROM bindle.steps
WHERE tenant_id = $1 AND type = 'tool_call' AND tool_name <> ''
  AND start_time >= $2 AND start_time < $3
GROUP BY tool_name
HAVING count(*) FILTER (WHERE status = 'failure') > 0
ORDER BY failures DESC, tool_name
LIMIT 10`

// Overview implements query.Store for runs started in [from, to).
func (s *Store) Overview(ctx context.Context, tenant string, from, to time.Time, bucket time.Duration) (*query.Overview, error) {
	o := &query.Overview{}
	err := s.withTenant(ctx, tenant, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, overviewKPISQL, tenant, from.UnixNano(), to.UnixNano()).Scan(
			&o.Runs, &o.FailedRuns, &o.P50RunMs, &o.P95RunMs, &o.CostUSD, &o.CostUnknownSteps,
			&o.InputTokens, &o.OutputTokens, &o.Steps, &o.FailedSteps); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, overviewSeriesSQL, tenant, from.UnixNano(), to.UnixNano(), bucket.Nanoseconds())
		if err != nil {
			return err
		}
		for rows.Next() {
			var p query.SeriesPoint
			var ns int64
			if err := rows.Scan(&ns, &p.Runs, &p.FailedRuns, &p.CostUSD); err != nil {
				rows.Close()
				return err
			}
			p.Bucket = nsTime(ns)
			o.Series = append(o.Series, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, topFailingToolsSQL, tenant, from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tf query.ToolFailure
			if err := rows.Scan(&tf.ToolName, &tf.Failures, &tf.Calls); err != nil {
				return err
			}
			o.TopFailingTools = append(o.TopFailingTools, tf)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return o, nil
}

// ---- loops ---------------------------------------------------------------

const loopsSQL = `SELECT trace_id, tool_name, count(*) AS repeats, min(start_ns), max(start_ns) AS last_ns
FROM bindle.steps
WHERE tenant_id = $1 AND type = 'tool_call' AND start_time >= $2 AND start_time < $3
GROUP BY trace_id, tool_name, md5(input)
HAVING count(*) >= $4
ORDER BY repeats DESC, last_ns DESC
LIMIT 100`

// Loops implements query.Store.
func (s *Store) Loops(ctx context.Context, tenant string, from, to time.Time, minRepeats int) ([]query.Loop, error) {
	var out []query.Loop
	err := s.withTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, loopsSQL, tenant, from, to, minRepeats)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l query.Loop
			var first, last int64
			if err := rows.Scan(&l.TraceID, &l.ToolName, &l.Repeats, &first, &last); err != nil {
				return err
			}
			l.FirstSeen, l.LastSeen = nsTime(first), nsTime(last)
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}
