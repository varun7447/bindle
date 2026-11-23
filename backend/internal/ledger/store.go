package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/lib/pq"
)

// Writer appends entries through the control-plane functions available to
// the ingest and processor roles (they have no table access).
type Writer struct {
	DB *sql.DB
}

// Append chains and stores the entries for one tenant and agent, skipping
// any already in the ledger, in one transaction. It is safe to retry and to
// call concurrently: writers for the same agent serialize on its head row.
// It returns how many entries were new.
func (w *Writer) Append(ctx context.Context, tenant, agent string, entries []Entry) (int, error) {
	if tenant == "" {
		return 0, errors.New("ledger: tenant is required")
	}
	if len(entries) == 0 {
		return 0, nil
	}
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('statement_timeout', '10000', true)`); err != nil {
		return 0, err
	}
	var headSeq int64
	var headHash []byte
	var day time.Time
	if err := tx.QueryRowContext(ctx, `SELECT seq, hash, day FROM control.ledger_lock_head($1, $2)`, tenant, agent).
		Scan(&headSeq, &headHash, &day); err != nil {
		return 0, fmt.Errorf("ledger: lock head: %w", err)
	}

	// Drop entries already stored (retries), and duplicates within the batch.
	kinds, traces, steps, ekeys := make([]string, len(entries)), make([]string, len(entries)), make([]string, len(entries)), make([]string, len(entries))
	for i, e := range entries {
		kinds[i], traces[i], steps[i], ekeys[i] = e.Kind, e.TraceID, e.StepID, e.EntryKey
	}
	rows, err := tx.QueryContext(ctx, `SELECT kind, trace_id, step_id, entry_key FROM control.ledger_known($1, $2, $3, $4, $5)`,
		tenant, pq.Array(kinds), pq.Array(traces), pq.Array(steps), pq.Array(ekeys))
	if err != nil {
		return 0, fmt.Errorf("ledger: known: %w", err)
	}
	seen := map[key]bool{}
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.kind, &k.trace, &k.step, &k.ekey); err != nil {
			rows.Close()
			return 0, err
		}
		seen[k] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	fresh := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if seen[e.key()] {
			continue
		}
		seen[e.key()] = true
		fresh = append(fresh, e)
	}
	if len(fresh) == 0 {
		return 0, tx.Commit()
	}
	sort.SliceStable(fresh, func(i, j int) bool {
		if !fresh[i].At.Equal(fresh[j].At) {
			return fresh[i].At.Before(fresh[j].At)
		}
		return fresh[i].StepID < fresh[j].StepID
	})

	dayStr := day.UTC().Format("2006-01-02")
	prev := headHash
	type row struct {
		Seq           int64     `json:"seq"`
		Day           string    `json:"day"`
		At            time.Time `json:"at"`
		Kind          string    `json:"kind"`
		TraceID       string    `json:"trace_id"`
		StepID        string    `json:"step_id"`
		EntryKey      string    `json:"entry_key"`
		StepType      string    `json:"step_type"`
		StepName      string    `json:"step_name"`
		ToolName      string    `json:"tool_name"`
		Status        string    `json:"status"`
		ErrorClass    string    `json:"error_class"`
		InputTokens   int64     `json:"input_tokens"`
		OutputTokens  int64     `json:"output_tokens"`
		CostUSD       string    `json:"cost_usd"`
		CostKnown     bool      `json:"cost_known"`
		SecurityFlags []string  `json:"security_flags"`
		Detail        string    `json:"detail"`
		PrevHash      string    `json:"prev_hash"`
		Hash          string    `json:"hash"`
	}
	out := make([]row, len(fresh))
	for i := range fresh {
		e := &fresh[i]
		e.TenantID, e.Agent, e.Seq, e.Day, e.PrevHash = tenant, agent, headSeq+int64(i)+1, dayStr, prev
		if e.SecurityFlags == nil {
			e.SecurityFlags = []string{}
		}
		sort.Strings(e.SecurityFlags)
		if e.Detail == "" {
			e.Detail = "{}"
		}
		if e.CostUSD == "" {
			e.CostUSD = "0"
		}
		e.Hash = Hash(*e)
		prev = e.Hash
		out[i] = row{e.Seq, e.Day, e.At, e.Kind, e.TraceID, e.StepID, e.EntryKey, e.StepType, e.StepName, e.ToolName,
			e.Status, e.ErrorClass, e.InputTokens, e.OutputTokens, e.CostUSD, e.CostKnown, e.SecurityFlags, e.Detail,
			hex.EncodeToString(e.PrevHash), hex.EncodeToString(e.Hash)}
	}
	body, err := json.Marshal(out)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT control.ledger_append($1, $2, $3, $4::jsonb)`, tenant, agent, headSeq, string(body)); err != nil {
		return 0, fmt.Errorf("ledger: append: %w", err)
	}
	return len(fresh), tx.Commit()
}

// Reader reads a tenant's ledger with the API role (row-level security).
type Reader struct {
	DB *sql.DB
}

func (r *Reader) inTenant(ctx context.Context, tenant string, fn func(*sql.Tx) error) error {
	if tenant == "" {
		return errors.New("ledger: tenant is required")
	}
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('bindle.tenant', $1, true), set_config('statement_timeout', '15000', true)`, tenant); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

const entryColumns = `agent, seq, day, at, kind, trace_id, step_id, entry_key, step_type, step_name, tool_name, status,
	error_class, input_tokens, output_tokens, cost_usd::text, cost_known, security_flags, detail, prev_hash, hash`

func scanEntry(sc interface{ Scan(...any) error }, tenant string) (Entry, error) {
	var e Entry
	var day time.Time
	var flags pq.StringArray
	if err := sc.Scan(&e.Agent, &e.Seq, &day, &e.At, &e.Kind, &e.TraceID, &e.StepID, &e.EntryKey, &e.StepType,
		&e.StepName, &e.ToolName, &e.Status, &e.ErrorClass, &e.InputTokens, &e.OutputTokens, &e.CostUSD, &e.CostKnown,
		&flags, &e.Detail, &e.PrevHash, &e.Hash); err != nil {
		return Entry{}, err
	}
	e.TenantID, e.Day, e.At, e.SecurityFlags = tenant, day.UTC().Format("2006-01-02"), e.At.UTC(), []string(flags)
	if e.SecurityFlags == nil {
		e.SecurityFlags = []string{}
	}
	return e, nil
}

// List returns an agent's entries appended between from and to (UTC days,
// inclusive), newest first. beforeSeq > 0 continues a previous page.
func (r *Reader) List(ctx context.Context, tenant, agent string, from, to time.Time, beforeSeq int64, limit int) ([]Entry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []Entry
	err := r.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		q := `SELECT ` + entryColumns + ` FROM control.ledger_entries
			WHERE tenant_id = $1 AND agent = $2 AND day BETWEEN $3 AND $4 AND ($5 = 0 OR seq < $5)
			ORDER BY seq DESC LIMIT $6`
		rows, err := tx.QueryContext(ctx, q, tenant, agent, dayOf(from), dayOf(to), beforeSeq, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEntry(rows, tenant)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// Broken describes the first entry that failed verification.
type Broken struct {
	Seq    int64     `json:"seq"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// Result of verifying a chain over a range of days.
type Result struct {
	OK          bool    `json:"ok"`
	Entries     int64   `json:"entries"`
	From        string  `json:"from"`
	To          string  `json:"to"`
	FirstSeq    int64   `json:"first_seq,omitempty"`
	LastSeq     int64   `json:"last_seq,omitempty"`
	FirstBroken *Broken `json:"first_broken,omitempty"`
}

// Verify recomputes the chain for the entries appended between from and to
// (UTC days, inclusive). The first entry's prev_hash is checked against the
// entry before it or, when earlier days were removed by retention, against
// the recorded final hash of that day's segment. If the range reaches the
// newest entry, the chain's head must match it too, so removing the newest
// entries is detected.
func (r *Reader) Verify(ctx context.Context, tenant, agent string, from, to time.Time) (Result, error) {
	res := Result{OK: true, From: dayOf(from), To: dayOf(to)}
	err := r.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var minSeq, maxSeq sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT min(seq), max(seq) FROM control.ledger_entries
			WHERE tenant_id = $1 AND agent = $2 AND day BETWEEN $3 AND $4`, tenant, agent, res.From, res.To).Scan(&minSeq, &maxSeq); err != nil {
			return err
		}
		if !minSeq.Valid {
			return nil
		}
		res.FirstSeq, res.LastSeq = minSeq.Int64, maxSeq.Int64
		prev, err := expectedPrev(ctx, tx, tenant, agent, minSeq.Int64)
		if err != nil {
			return err
		}
		next := minSeq.Int64
		var last Entry
		const page = 2000
		for next <= maxSeq.Int64 && res.OK {
			rows, err := tx.QueryContext(ctx, `SELECT `+entryColumns+` FROM control.ledger_entries
				WHERE tenant_id = $1 AND agent = $2 AND seq >= $3 AND seq <= $4 ORDER BY seq LIMIT $5`,
				tenant, agent, next, maxSeq.Int64, page)
			if err != nil {
				return err
			}
			n := 0
			for rows.Next() {
				e, err := scanEntry(rows, tenant)
				if err != nil {
					rows.Close()
					return err
				}
				n++
				if b := check(e, next, prev); b != nil {
					res.OK, res.FirstBroken = false, b
					break
				}
				res.Entries++
				prev, next, last = e.Hash, e.Seq+1, e
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if n == 0 && res.OK { // the next seq is missing
				res.OK, res.FirstBroken = false, &Broken{Seq: next, Reason: "entry is missing from the chain"}
			}
		}
		if !res.OK {
			return nil
		}
		var headSeq int64
		var headHash []byte
		if err := tx.QueryRowContext(ctx, `SELECT seq, hash FROM control.ledger_heads WHERE tenant_id = $1 AND agent = $2`,
			tenant, agent).Scan(&headSeq, &headHash); err != nil {
			return err
		}
		if headSeq == last.Seq && !bytes.Equal(headHash, last.Hash) {
			res.OK, res.FirstBroken = false, &Broken{Seq: last.Seq, At: last.At, Reason: "the newest entry doesn't match the chain head"}
		}
		if headSeq < last.Seq {
			res.OK, res.FirstBroken = false, &Broken{Seq: headSeq + 1, Reason: "entries exist beyond the chain head"}
		}
		if res.To >= dayOf(time.Now()) && headSeq > last.Seq {
			res.OK, res.FirstBroken = false, &Broken{Seq: last.Seq + 1, Reason: "newer entries are missing (the chain head is further along)"}
		}
		return nil
	})
	return res, err
}

// check returns why e doesn't continue the chain, or nil.
func check(e Entry, wantSeq int64, prev []byte) *Broken {
	switch {
	case e.Seq != wantSeq:
		return &Broken{Seq: wantSeq, At: e.At, Reason: "entry is missing from the chain"}
	case !bytes.Equal(e.PrevHash, prev):
		return &Broken{Seq: e.Seq, At: e.At, Reason: "its previous-hash doesn't match the entry before it"}
	case !bytes.Equal(e.Hash, Hash(e)):
		return &Broken{Seq: e.Seq, At: e.At, Reason: "its hash doesn't match its contents"}
	}
	return nil
}

// expectedPrev is the hash the entry with seq should chain from.
func expectedPrev(ctx context.Context, tx *sql.Tx, tenant, agent string, seq int64) ([]byte, error) {
	if seq == 1 {
		return Genesis, nil
	}
	var h []byte
	err := tx.QueryRowContext(ctx, `SELECT hash FROM control.ledger_entries WHERE tenant_id = $1 AND agent = $2 AND seq = $3`,
		tenant, agent, seq-1).Scan(&h)
	if err == nil {
		return h, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	err = tx.QueryRowContext(ctx, `SELECT final_hash FROM control.ledger_segments WHERE tenant_id = $1 AND agent = $2 AND last_seq = $3`,
		tenant, agent, seq-1).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // nothing to chain from: check() reports the mismatch
	}
	return h, err
}

// Day returns every entry an agent appended on one UTC day, in chain order.
func (r *Reader) Day(ctx context.Context, tenant, agent string, day time.Time) ([]Entry, error) {
	var out []Entry
	err := r.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+entryColumns+` FROM control.ledger_entries
			WHERE tenant_id = $1 AND agent = $2 AND day = $3 ORDER BY seq LIMIT 200000`, tenant, agent, dayOf(day))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEntry(rows, tenant)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Maintain creates upcoming day partitions, records the segments of
// finished days and drops days older than retentionDays (alerts role).
func Maintain(ctx context.Context, db *sql.DB, retentionDays int) (closed, dropped int, err error) {
	err = db.QueryRowContext(ctx, `SELECT closed, dropped FROM control.ledger_maintain($1, 3)`, retentionDays).Scan(&closed, &dropped)
	return closed, dropped, err
}
