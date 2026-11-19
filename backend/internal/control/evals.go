package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bindle-ai/bindle/backend/internal/evals"
	"github.com/lib/pq"
)

// ErrDuplicate means a name is already used in the tenant.
var ErrDuplicate = errors.New("control: name already used")

const (
	MaxDatasetItems = 500
	maxItemText     = 64 << 10
	maxFailures     = 500
)

const scorerCols = `id, name, kind, agent, value, number, case_sensitive, enabled, created_at`

func scanScorer(sc interface{ Scan(...any) error }) (evals.Scorer, error) {
	var s evals.Scorer
	err := sc.Scan(&s.ID, &s.Name, &s.Kind, &s.Agent, &s.Value, &s.Number, &s.CaseSensitive, &s.Enabled, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	s.CreatedAt = s.CreatedAt.UTC()
	return s, err
}

func dupe(err error) error {
	var pe *pq.Error
	if errors.As(err, &pe) && pe.Code == "23505" {
		return ErrDuplicate
	}
	return err
}

func optUser(userID string) any {
	if userID == "" {
		return nil
	}
	return userID
}

// ListScorers returns the tenant's checks, oldest first.
func (s *Store) ListScorers(ctx context.Context, tenant string) (out []evals.Scorer, err error) {
	out = []evals.Scorer{}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+scorerCols+` FROM control.scorers ORDER BY created_at, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sc, err := scanScorer(rows)
			if err != nil {
				return err
			}
			out = append(out, sc)
		}
		return rows.Err()
	})
	return out, err
}

// CreateScorer validates and adds a check.
func (s *Store) CreateScorer(ctx context.Context, tenant, userID string, sc evals.Scorer) (out evals.Scorer, err error) {
	if err := evals.Validate(sc); err != nil {
		return out, err
	}
	id, err := newID("sc_")
	if err != nil {
		return out, err
	}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM control.scorers`).Scan(&n); err != nil {
			return err
		}
		if n >= evals.MaxScorers {
			return evals.Invalid("at most %d checks", evals.MaxScorers)
		}
		out, err = scanScorer(tx.QueryRowContext(ctx, `INSERT INTO control.scorers (id, tenant_id, name, kind, agent, value, number, case_sensitive, enabled, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+scorerCols,
			id, tenant, sc.Name, sc.Kind, sc.Agent, sc.Value, sc.Number, sc.CaseSensitive, sc.Enabled, optUser(userID)))
		return dupe(err)
	})
	return out, err
}

// UpdateScorer changes a check (its kind is fixed).
func (s *Store) UpdateScorer(ctx context.Context, tenant, id string, sc evals.Scorer) (out evals.Scorer, err error) {
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		cur, err := scanScorer(tx.QueryRowContext(ctx, `SELECT `+scorerCols+` FROM control.scorers WHERE id = $1`, id))
		if err != nil {
			return err
		}
		sc.ID, sc.Kind = cur.ID, cur.Kind
		if err := evals.Validate(sc); err != nil {
			return err
		}
		out, err = scanScorer(tx.QueryRowContext(ctx, `UPDATE control.scorers SET name = $2, agent = $3, value = $4, number = $5, case_sensitive = $6, enabled = $7
			WHERE id = $1 RETURNING `+scorerCols, id, sc.Name, sc.Agent, sc.Value, sc.Number, sc.CaseSensitive, sc.Enabled))
		return dupe(err)
	})
	return out, err
}

// DeleteScorer removes a check. Past results keep its name.
func (s *Store) DeleteScorer(ctx context.Context, tenant, id string) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		return oneRow(tx.ExecContext(ctx, `DELETE FROM control.scorers WHERE id = $1`, id))
	})
}

// Dataset is a named set of runs.
type Dataset struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Items       int       `json:"items"`
	CreatedAt   time.Time `json:"created_at"`
	LastEval    *EvalRun  `json:"last_eval,omitempty"`
}

// EvalRun is one run of the checks over a dataset.
type EvalRun struct {
	ID        string        `json:"id"`
	DatasetID string        `json:"dataset_id"`
	Items     int           `json:"items"`
	Tallies   []evals.Tally `json:"tallies"`
	Failures  []Failure     `json:"failures"`
	CreatedAt time.Time     `json:"created_at"`
}

// Failure is one failed check on one dataset item.
type Failure struct {
	TraceID  string `json:"trace_id"`
	ScorerID string `json:"scorer_id"`
	Scorer   string `json:"scorer"`
	Detail   string `json:"detail"`
}

func cleanText(name string, max int) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > max || !utf8.ValidString(name) || strings.ContainsAny(name, "\x00\r\n") {
		return "", evals.Invalid("name must be 1-%d characters on one line", max)
	}
	return name, nil
}

// CreateDataset adds an empty dataset.
func (s *Store) CreateDataset(ctx context.Context, tenant, userID, name, description string) (out Dataset, err error) {
	if name, err = cleanText(name, 80); err != nil {
		return out, err
	}
	if len(description) > 500 {
		return out, evals.Invalid("description is longer than 500 characters")
	}
	id, err := newID("ds_")
	if err != nil {
		return out, err
	}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `INSERT INTO control.datasets (id, tenant_id, name, description, created_by) VALUES ($1, $2, $3, $4, $5)
			RETURNING id, name, description, created_at`, id, tenant, name, strings.TrimSpace(description), optUser(userID)).
			Scan(&out.ID, &out.Name, &out.Description, &out.CreatedAt)
		return dupe(err)
	})
	out.CreatedAt = out.CreatedAt.UTC()
	return out, err
}

// ListDatasets returns datasets with item counts and their latest eval.
func (s *Store) ListDatasets(ctx context.Context, tenant string) (out []Dataset, err error) {
	out = []Dataset{}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT d.id, d.name, d.description, d.created_at,
				(SELECT count(*) FROM control.dataset_items i WHERE i.dataset_id = d.id),
				e.id, e.items, e.tallies, e.created_at
			FROM control.datasets d
			LEFT JOIN LATERAL (SELECT id, items, tallies, created_at FROM control.eval_runs r WHERE r.dataset_id = d.id ORDER BY created_at DESC LIMIT 1) e ON true
			ORDER BY d.created_at DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d Dataset
			var eid sql.NullString
			var eitems sql.NullInt64
			var etallies []byte
			var eat sql.NullTime
			if err := rows.Scan(&d.ID, &d.Name, &d.Description, &d.CreatedAt, &d.Items, &eid, &eitems, &etallies, &eat); err != nil {
				return err
			}
			d.CreatedAt = d.CreatedAt.UTC()
			if eid.Valid {
				e := &EvalRun{ID: eid.String, DatasetID: d.ID, Items: int(eitems.Int64), CreatedAt: eat.Time.UTC(), Failures: []Failure{}}
				if err := json.Unmarshal(etallies, &e.Tallies); err != nil {
					return err
				}
				d.LastEval = e
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// GetDataset returns a dataset and its items.
func (s *Store) GetDataset(ctx context.Context, tenant, id string) (d Dataset, items []evals.Run, err error) {
	items = []evals.Run{}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT id, name, description, created_at FROM control.datasets WHERE id = $1`, id).
			Scan(&d.ID, &d.Name, &d.Description, &d.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		d.CreatedAt = d.CreatedAt.UTC()
		rows, err := tx.QueryContext(ctx, `SELECT trace_id, agent, version, input, output, tools, steps, cost_usd, cost_known, duration_ms, failed
			FROM control.dataset_items WHERE dataset_id = $1 ORDER BY added_at, trace_id`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r evals.Run
			var tools []byte
			if err := rows.Scan(&r.TraceID, &r.Agent, &r.Version, &r.Input, &r.Output, &tools, &r.Steps, &r.CostUSD, &r.CostKnown, &r.DurationMs, &r.Failed); err != nil {
				return err
			}
			if err := json.Unmarshal(tools, &r.Tools); err != nil {
				return err
			}
			items = append(items, r)
		}
		d.Items = len(items)
		return rows.Err()
	})
	return d, items, err
}

func clip(s string) string {
	if len(s) <= maxItemText {
		return s
	}
	cut := maxItemText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// AddDatasetItems copies runs into a dataset; runs already in it are kept
// as they were. It returns how many were added.
func (s *Store) AddDatasetItems(ctx context.Context, tenant, id string, runs []evals.Run) (added int, err error) {
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var n int
		err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM control.dataset_items WHERE dataset_id = $1)
			FROM control.datasets WHERE id = $1`, id).Scan(&n)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		for _, r := range runs {
			if n >= MaxDatasetItems {
				return evals.Invalid("a dataset holds at most %d runs", MaxDatasetItems)
			}
			tools, _ := json.Marshal(r.Tools)
			if r.Tools == nil {
				tools = []byte("[]")
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO control.dataset_items (dataset_id, tenant_id, trace_id, agent, version, input, output, tools, steps, cost_usd, cost_known, duration_ms, failed)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) ON CONFLICT DO NOTHING`,
				id, tenant, r.TraceID, r.Agent, r.Version, clip(r.Input), clip(r.Output), tools, r.Steps, r.CostUSD, r.CostKnown, r.DurationMs, r.Failed)
			if err != nil {
				return err
			}
			if k, _ := res.RowsAffected(); k == 1 {
				added++
				n++
			}
		}
		return nil
	})
	return added, err
}

// RemoveDatasetItem removes one run from a dataset.
func (s *Store) RemoveDatasetItem(ctx context.Context, tenant, id, traceID string) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		return oneRow(tx.ExecContext(ctx, `DELETE FROM control.dataset_items WHERE dataset_id = $1 AND trace_id = $2`, id, traceID))
	})
}

// DeleteDataset removes a dataset, its items and its eval results.
func (s *Store) DeleteDataset(ctx context.Context, tenant, id string) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		return oneRow(tx.ExecContext(ctx, `DELETE FROM control.datasets WHERE id = $1`, id))
	})
}

// RecordEvalRun stores the results of running the checks on a dataset.
func (s *Store) RecordEvalRun(ctx context.Context, tenant, userID, datasetID string, items int, tallies []evals.Tally, failures []Failure) (out EvalRun, err error) {
	if len(failures) > maxFailures {
		failures = failures[:maxFailures]
	}
	if failures == nil {
		failures = []Failure{}
	}
	if tallies == nil {
		tallies = []evals.Tally{}
	}
	id, err := newID("ev_")
	if err != nil {
		return out, err
	}
	t, _ := json.Marshal(tallies)
	f, _ := json.Marshal(failures)
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `INSERT INTO control.eval_runs (id, tenant_id, dataset_id, items, tallies, failures, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`, id, tenant, datasetID, items, t, f, optUser(userID)).Scan(&out.CreatedAt)
	})
	out = EvalRun{ID: id, DatasetID: datasetID, Items: items, Tallies: tallies, Failures: failures, CreatedAt: out.CreatedAt.UTC()}
	return out, err
}

// ListEvalRuns returns a dataset's eval runs, newest first (at most 20).
func (s *Store) ListEvalRuns(ctx context.Context, tenant, datasetID string) (out []EvalRun, err error) {
	out = []EvalRun{}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, items, tallies, failures, created_at FROM control.eval_runs
			WHERE dataset_id = $1 ORDER BY created_at DESC LIMIT 20`, datasetID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e := EvalRun{DatasetID: datasetID}
			var t, f []byte
			if err := rows.Scan(&e.ID, &e.Items, &t, &f, &e.CreatedAt); err != nil {
				return err
			}
			e.CreatedAt = e.CreatedAt.UTC()
			if err := json.Unmarshal(t, &e.Tallies); err != nil {
				return err
			}
			if err := json.Unmarshal(f, &e.Failures); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}
