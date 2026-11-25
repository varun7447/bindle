package control

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lib/pq"

	"github.com/bindle-ai/bindle/backend/internal/outcomes"
	"github.com/bindle-ai/bindle/backend/internal/secretbox"
)

// ---- workspace settings (api) ----------------------------------------------

// GetOutcomeSettings returns the workspace's outcome settings (defaults if
// it never saved any).
func (s *Store) GetOutcomeSettings(ctx context.Context, tenant string) (out outcomes.Settings, err error) {
	out = outcomes.DefaultSettings()
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var rev pq.StringArray
		e := tx.QueryRowContext(ctx, `SELECT reversal_outcomes, pending_hours, updated_at FROM control.outcome_settings WHERE tenant_id = $1`, tenant).
			Scan(&rev, &out.PendingHours, &out.UpdatedAt)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		out.ReversalOutcomes = append([]string{}, rev...)
		return e
	})
	return out, err
}

// PutOutcomeSettings validates and saves the workspace's outcome settings.
func (s *Store) PutOutcomeSettings(ctx context.Context, tenant, userID string, in outcomes.Settings) (outcomes.Settings, error) {
	clean, err := outcomes.NormalizeSettings(in)
	if err != nil {
		return outcomes.Settings{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `INSERT INTO control.outcome_settings (tenant_id, reversal_outcomes, pending_hours, updated_by)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant_id) DO UPDATE SET reversal_outcomes = EXCLUDED.reversal_outcomes, pending_hours = EXCLUDED.pending_hours,
				updated_by = EXCLUDED.updated_by, updated_at = now()
			RETURNING updated_at`, tenant, pq.Array(clean.ReversalOutcomes), clean.PendingHours, userID).Scan(&clean.UpdatedAt)
	})
	return clean, err
}

// ErrInvalidSettings wraps a settings validation error (HTTP 400).
var ErrInvalidSettings = errors.New("control: invalid settings")

// ---- webhook endpoints (api) -------------------------------------------------

// OutcomeEndpoint is a signed webhook for outcome events. Its secret is
// returned once, at creation.
type OutcomeEndpoint struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedBy string     `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

const endpointCols = `id, name, COALESCE(created_by, ''), created_at, revoked_at`

func scanEndpoint(sc interface{ Scan(...any) error }) (OutcomeEndpoint, error) {
	var e OutcomeEndpoint
	err := sc.Scan(&e.ID, &e.Name, &e.CreatedBy, &e.CreatedAt, &e.RevokedAt)
	return e, err
}

// CreateOutcomeEndpoint adds a webhook endpoint and returns its signing
// secret (shown once).
func (s *Store) CreateOutcomeEndpoint(ctx context.Context, tenant, userID, name string) (out OutcomeEndpoint, secret string, err error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 80 {
		return OutcomeEndpoint{}, "", fmt.Errorf("%w: name must be 1-80 characters", ErrInvalidSettings)
	}
	idb, sb := make([]byte, 8), make([]byte, 32)
	if _, err := rand.Read(idb); err != nil {
		return OutcomeEndpoint{}, "", err
	}
	if _, err := rand.Read(sb); err != nil {
		return OutcomeEndpoint{}, "", err
	}
	id, secret := "oep_"+hex.EncodeToString(idb), "obsec_"+hex.EncodeToString(sb)
	// Stored encrypted; without a secrets key the endpoint isn't created.
	enc, err := boxOrDisabled(s.Secrets).Encrypt(ctx, []byte(secret), secretbox.Context(tenant, secretbox.PurposeOutcomeSecret, id))
	if err != nil {
		return OutcomeEndpoint{}, "", err
	}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM control.outcome_endpoints WHERE tenant_id = $1 AND revoked_at IS NULL`, tenant).Scan(&n); err != nil {
			return err
		}
		if n >= 20 {
			return fmt.Errorf("%w: at most 20 active endpoints", ErrInvalidSettings)
		}
		out, err = scanEndpoint(tx.QueryRowContext(ctx, `INSERT INTO control.outcome_endpoints (id, tenant_id, name, secret_enc, created_by)
			VALUES ($1, $2, $3, $4, $5) RETURNING `+endpointCols, id, tenant, name, enc, userID))
		return err
	})
	if err != nil {
		return OutcomeEndpoint{}, "", err
	}
	return out, secret, nil
}

// ListOutcomeEndpoints returns the workspace's endpoints, newest first.
func (s *Store) ListOutcomeEndpoints(ctx context.Context, tenant string) (out []OutcomeEndpoint, err error) {
	out = []OutcomeEndpoint{}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+endpointCols+` FROM control.outcome_endpoints WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 100`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEndpoint(rows)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// RevokeOutcomeEndpoint stops an endpoint from accepting events.
func (s *Store) RevokeOutcomeEndpoint(ctx context.Context, tenant, id string) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		return oneRow(tx.ExecContext(ctx, `UPDATE control.outcome_endpoints SET revoked_at = now() WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL`, tenant, id))
	})
}

// ---- events -------------------------------------------------------------------

const eventCols = `id, correlations, name, success, value, COALESCE(comment, ''), source, COALESCE(endpoint_id, ''),
	occurred_at, received_at, status, attempts, next_attempt_at, expires_at, matched_traces, matched_at`

func scanEvent(sc interface{ Scan(...any) error }) (outcomes.Event, error) {
	var e outcomes.Event
	var corr, traces pq.StringArray
	var value sql.NullFloat64
	err := sc.Scan(&e.ID, &corr, &e.Name, &e.Success, &value, &e.Comment, &e.Source, &e.EndpointID,
		&e.OccurredAt, &e.ReceivedAt, &e.Status, &e.Attempts, &e.NextAttemptAt, &e.ExpiresAt, &traces, &e.MatchedAt)
	if value.Valid {
		v := value.Float64
		e.Value = &v
	}
	e.Correlations, e.MatchedTraces = append([]string{}, corr...), append([]string{}, traces...)
	return e, err
}

// ListOutcomeEvents returns recent events, newest first, optionally of one
// status (Settings → Outcomes).
func (s *Store) ListOutcomeEvents(ctx context.Context, tenant, status string, limit int) (out []outcomes.Event, err error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out = []outcomes.Event{}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+eventCols+` FROM control.outcome_events
			WHERE tenant_id = $1 AND ($2 = '' OR status = $2) ORDER BY received_at DESC, id LIMIT $3`, tenant, status, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEvent(rows)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// MatchedOutcomes returns, per run, the outcome events matched to it among
// events that occurred since since (for grading and economics).
func (s *Store) MatchedOutcomes(ctx context.Context, tenant string, since time.Time) (out map[string][]outcomes.OutcomeFact, err error) {
	out = map[string][]outcomes.OutcomeFact{}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT t, name, success FROM control.outcome_events, unnest(matched_traces) AS t
			WHERE tenant_id = $1 AND status = 'matched' AND occurred_at >= $2 LIMIT 50000`, tenant, since)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var trace string
			var f outcomes.OutcomeFact
			if err := rows.Scan(&trace, &f.Name, &f.Success); err != nil {
				return err
			}
			out[trace] = append(out[trace], f)
		}
		return rows.Err()
	})
	return out, err
}

// OutcomeIngest is ingest's view: record events, look up webhook endpoints
// and settings, and report an event's status. Ingest connects as its own
// role; events are written under row-level security for their tenant.
type OutcomeIngest struct {
	DB      *sql.DB
	Secrets secretbox.Box // decrypts endpoint signing secrets
}

func (o *OutcomeIngest) store() *Store { return &Store{DB: o.DB, Secrets: o.Secrets} }

// Settings returns the tenant's outcome settings (defaults if none).
func (o *OutcomeIngest) Settings(ctx context.Context, tenant string) (outcomes.Settings, error) {
	s := outcomes.DefaultSettings()
	var rev pq.StringArray
	err := o.DB.QueryRowContext(ctx, `SELECT reversal_outcomes, pending_hours FROM control.outcome_settings_for($1)`, tenant).Scan(&rev, &s.PendingHours)
	s.ReversalOutcomes = append([]string{}, rev...)
	return s, err
}

// EndpointSecret returns the tenant and signing secret of an active
// endpoint; ok is false for unknown or revoked endpoints.
func (o *OutcomeIngest) EndpointSecret(ctx context.Context, id string) (tenant, secret string, ok bool, err error) {
	var enc []byte
	err = o.DB.QueryRowContext(ctx, `SELECT tenant_id, secret, secret_enc FROM control.outcome_endpoint_secret($1)`, id).Scan(&tenant, &secret, &enc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if enc != nil { // rows written before encryption carry plaintext instead
		pt, err := boxOrDisabled(o.Secrets).Decrypt(ctx, enc, secretbox.Context(tenant, secretbox.PurposeOutcomeSecret, id))
		if err != nil {
			return "", "", false, err
		}
		secret = string(pt)
	}
	return tenant, secret, true, nil
}

// Record stores an event; a duplicate (same ID) is not stored again.
// It returns the stored event's current status.
func (o *OutcomeIngest) Record(ctx context.Context, ev outcomes.Event) (stored outcomes.Event, created bool, err error) {
	err = o.store().inTenant(ctx, ev.TenantID, func(tx *sql.Tx) error {
		var value any
		if ev.Value != nil {
			value = *ev.Value
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO control.outcome_events
			(id, tenant_id, correlations, name, success, value, comment, source, endpoint_id, occurred_at, received_at, expires_at, next_attempt_at)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, NULLIF($9, ''), $10, $11, $12, $11)
			ON CONFLICT (id) DO NOTHING`, ev.ID, ev.TenantID, pq.Array(ev.Correlations), ev.Name, ev.Success, value, ev.Comment,
			ev.Source, ev.EndpointID, ev.OccurredAt, ev.ReceivedAt, ev.ExpiresAt)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		created = n == 1
		stored, err = scanEvent(tx.QueryRowContext(ctx, `SELECT `+eventCols+` FROM control.outcome_events WHERE tenant_id = $1 AND id = $2`, ev.TenantID, ev.ID))
		return err
	})
	return stored, created, err
}

// Status returns one of the tenant's events; ErrNotFound if it isn't theirs.
func (o *OutcomeIngest) Status(ctx context.Context, tenant, id string) (ev outcomes.Event, err error) {
	err = o.store().inTenant(ctx, tenant, func(tx *sql.Tx) error {
		ev, err = scanEvent(tx.QueryRowContext(ctx, `SELECT `+eventCols+` FROM control.outcome_events WHERE tenant_id = $1 AND id = $2`, tenant, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return ev, err
}

// OutcomeMatchStore is the matcher's view (alerts worker role).
type OutcomeMatchStore struct {
	DB *sql.DB
}

func (o *OutcomeMatchStore) store() *Store { return &Store{DB: o.DB} }

// DueOutcomeTenants lists workspaces with events due for an attempt.
func (o *OutcomeMatchStore) DueOutcomeTenants(ctx context.Context) ([]string, error) {
	rows, err := o.DB.QueryContext(ctx, `SELECT control.outcome_due_tenants()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DueOutcomeEvents returns up to limit pending events due now, oldest first.
func (o *OutcomeMatchStore) DueOutcomeEvents(ctx context.Context, tenant string, limit int) (out []outcomes.Event, err error) {
	err = o.store().inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+eventCols+` FROM control.outcome_events
			WHERE tenant_id = $1 AND status = 'pending' AND next_attempt_at <= now() ORDER BY next_attempt_at, id LIMIT $2`, tenant, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEvent(rows)
			if err != nil {
				return err
			}
			e.TenantID = tenant
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// SaveOutcomeAttempt records the result of one matching attempt.
func (o *OutcomeMatchStore) SaveOutcomeAttempt(ctx context.Context, tenant string, ev outcomes.Event) error {
	return o.store().inTenant(ctx, tenant, func(tx *sql.Tx) error {
		return oneRow(tx.ExecContext(ctx, `UPDATE control.outcome_events
			SET status = $3, attempts = $4, next_attempt_at = $5, matched_traces = $6, matched_at = $7
			WHERE tenant_id = $1 AND id = $2 AND status = 'pending'`,
			tenant, ev.ID, ev.Status, ev.Attempts, ev.NextAttemptAt, pq.Array(ev.MatchedTraces), ev.MatchedAt))
	})
}

// PruneOutcomeEvents removes events older than 90 days.
func (o *OutcomeMatchStore) PruneOutcomeEvents(ctx context.Context) (n int64, err error) {
	err = o.DB.QueryRowContext(ctx, `SELECT control.outcome_events_prune()`).Scan(&n)
	return n, err
}
