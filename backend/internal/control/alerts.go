package control

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/alerts"
)

const ruleCols = `id, tenant_id, kind, threshold, window_minutes, min_runs, enabled, created_at`

func scanRule(sc interface{ Scan(...any) error }) (alerts.Rule, error) {
	var r alerts.Rule
	err := sc.Scan(&r.ID, &r.TenantID, &r.Kind, &r.Threshold, &r.WindowMinutes, &r.MinRuns, &r.Enabled, &r.CreatedAt)
	return r, err
}

const channelCols = `id, tenant_id, kind, name, target, secret, enabled, created_at`

func scanChannel(sc interface{ Scan(...any) error }) (alerts.Channel, error) {
	var c alerts.Channel
	err := sc.Scan(&c.ID, &c.TenantID, &c.Kind, &c.Name, &c.Target, &c.Secret, &c.Enabled, &c.CreatedAt)
	c.Display = alerts.DisplayTarget(c.Kind, c.Target)
	return c, err
}

func listRules(ctx context.Context, tx *sql.Tx) ([]alerts.Rule, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+ruleCols+` FROM control.alert_rules ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alerts.Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func listChannels(ctx context.Context, tx *sql.Tx) ([]alerts.Channel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+channelCols+` FROM control.alert_channels ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alerts.Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func insertRule(ctx context.Context, tx *sql.Tx, tenant, userID string, r alerts.Rule) (alerts.Rule, error) {
	if err := alerts.ValidateRule(r); err != nil {
		return alerts.Rule{}, err
	}
	id, err := newID("ar_")
	if err != nil {
		return alerts.Rule{}, err
	}
	var by any
	if userID != "" {
		by = userID
	}
	return scanRule(tx.QueryRowContext(ctx, `INSERT INTO control.alert_rules (id, tenant_id, kind, threshold, window_minutes, min_runs, enabled, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+ruleCols,
		id, tenant, r.Kind, r.Threshold, r.WindowMinutes, r.MinRuns, r.Enabled, by))
}

// ---- console (API role) --------------------------------------------------

// ListAlertRules returns the tenant's rules.
func (s *Store) ListAlertRules(ctx context.Context, tenant string) (out []alerts.Rule, err error) {
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error { out, err = listRules(ctx, tx); return err })
	return out, err
}

// CreateAlertRule validates and adds a rule.
func (s *Store) CreateAlertRule(ctx context.Context, tenant, userID string, r alerts.Rule) (out alerts.Rule, err error) {
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error { out, err = insertRule(ctx, tx, tenant, userID, r); return err })
	return out, err
}

// UpdateAlertRule changes a rule's settings (its kind is fixed).
func (s *Store) UpdateAlertRule(ctx context.Context, tenant, id string, r alerts.Rule) (out alerts.Rule, err error) {
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		cur, err := scanRule(tx.QueryRowContext(ctx, `SELECT `+ruleCols+` FROM control.alert_rules WHERE id = $1`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		r.Kind = cur.Kind
		if err := alerts.ValidateRule(r); err != nil {
			return err
		}
		out, err = scanRule(tx.QueryRowContext(ctx, `UPDATE control.alert_rules SET threshold = $2, window_minutes = $3, min_runs = $4, enabled = $5
			WHERE id = $1 RETURNING `+ruleCols, id, r.Threshold, r.WindowMinutes, r.MinRuns, r.Enabled))
		return err
	})
	return out, err
}

// ListAlertChannels returns the tenant's channels (targets included; the
// API must not return them).
func (s *Store) ListAlertChannels(ctx context.Context, tenant string) (out []alerts.Channel, err error) {
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error { out, err = listChannels(ctx, tx); return err })
	return out, err
}

// GetAlertChannel returns one channel.
func (s *Store) GetAlertChannel(ctx context.Context, tenant, id string) (out alerts.Channel, err error) {
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		out, err = scanChannel(tx.QueryRowContext(ctx, `SELECT `+channelCols+` FROM control.alert_channels WHERE id = $1`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return out, err
}

// CreateAlertChannel validates and adds a channel. Webhook channels get a
// signing secret, returned here once.
func (s *Store) CreateAlertChannel(ctx context.Context, tenant, userID string, c alerts.Channel) (out alerts.Channel, secret string, err error) {
	c.Enabled = true
	if err := alerts.ValidateChannel(c); err != nil {
		return alerts.Channel{}, "", err
	}
	if c.Kind == "webhook" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return alerts.Channel{}, "", err
		}
		secret = "whsec_" + hex.EncodeToString(b)
	}
	id, err := newID("ch_")
	if err != nil {
		return alerts.Channel{}, "", err
	}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		out, err = scanChannel(tx.QueryRowContext(ctx, `INSERT INTO control.alert_channels (id, tenant_id, kind, name, target, secret, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+channelCols, id, tenant, c.Kind, c.Name, c.Target, secret, userID))
		return err
	})
	return out, secret, err
}

// SetAlertChannelEnabled pauses or resumes a channel.
func (s *Store) SetAlertChannelEnabled(ctx context.Context, tenant, id string, enabled bool) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE control.alert_channels SET enabled = $2 WHERE id = $1`, id, enabled)
		return oneRow(res, err)
	})
}

// DeleteAlertChannel removes a channel and its secret.
func (s *Store) DeleteAlertChannel(ctx context.Context, tenant, id string) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM control.alert_channels WHERE id = $1`, id)
		return oneRow(res, err)
	})
}

func oneRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListAlertEvents returns the most recent fired alerts.
func (s *Store) ListAlertEvents(ctx context.Context, tenant string, limit int) (out []alerts.Event, err error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, rule_id, kind, summary, trace_id, details, delivery, fired_at
			FROM control.alert_events ORDER BY fired_at DESC, id LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev alerts.Event
			var details, delivery []byte
			if err := rows.Scan(&ev.ID, &ev.RuleID, &ev.Kind, &ev.Summary, &ev.TraceID, &details, &delivery, &ev.FiredAt); err != nil {
				return err
			}
			_ = json.Unmarshal(details, &ev.Details)
			_ = json.Unmarshal(delivery, &ev.Delivery)
			out = append(out, ev)
		}
		return rows.Err()
	})
	return out, err
}

// ---- evaluator (alerts role) ---------------------------------------------

// AlertStore is the evaluator's view of the control plane; it implements
// alerts.Store with the least-privileged alerts role.
type AlertStore struct{ DB *sql.DB }

func (a *AlertStore) store() *Store { return &Store{DB: a.DB} }

// AlertingTenants implements alerts.Store.
func (a *AlertStore) AlertingTenants(ctx context.Context) ([]string, error) {
	rows, err := a.DB.QueryContext(ctx, `SELECT control.alerting_tenants()`)
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

// Rules implements alerts.Store.
func (a *AlertStore) Rules(ctx context.Context, tenant string) ([]alerts.Rule, error) {
	return a.store().ListAlertRules(ctx, tenant)
}

// Channels implements alerts.Store.
func (a *AlertStore) Channels(ctx context.Context, tenant string) ([]alerts.Channel, error) {
	return a.store().ListAlertChannels(ctx, tenant)
}

// RecordEvent implements alerts.Store.
func (a *AlertStore) RecordEvent(ctx context.Context, ev alerts.Event) (created bool, err error) {
	details, err := json.Marshal(ev.Details)
	if err != nil {
		return false, err
	}
	if ev.FiredAt.IsZero() {
		ev.FiredAt = time.Now()
	}
	err = a.store().inTenant(ctx, ev.TenantID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO control.alert_events (id, tenant_id, rule_id, kind, fingerprint, summary, trace_id, details, fired_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (rule_id, fingerprint) DO NOTHING`,
			ev.ID, ev.TenantID, ev.RuleID, ev.Kind, ev.Fingerprint, ev.Summary, ev.TraceID, details, ev.FiredAt)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		created = n == 1
		return nil
	})
	return created, err
}

// SetDelivery implements alerts.Store.
func (a *AlertStore) SetDelivery(ctx context.Context, tenant, eventID string, d map[string]string) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return a.store().inTenant(ctx, tenant, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE control.alert_events SET delivery = $2 WHERE id = $1`, eventID, b)
		return err
	})
}

// BudgetTenants implements alerts.Store: tenants with a daily budget, and
// their limits.
func (a *AlertStore) BudgetTenants(ctx context.Context) ([]alerts.BudgetTenant, error) {
	rows, err := a.DB.QueryContext(ctx, `SELECT tenant_id, limits FROM control.budget_tenants()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alerts.BudgetTenant
	for rows.Next() {
		var bt alerts.BudgetTenant
		var raw []byte
		if err := rows.Scan(&bt.Tenant, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &bt.Limits); err != nil {
			return nil, err
		}
		out = append(out, bt)
	}
	return out, rows.Err()
}

// RecordBudgetExceeded implements alerts.Store: it records that agent went
// over budget on day, once; it reports whether this call recorded it.
func (a *AlertStore) RecordBudgetExceeded(ctx context.Context, tenant, agent string, day time.Time, spent, budget float64) (created bool, err error) {
	err = a.store().inTenant(ctx, tenant, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO control.budget_state (tenant_id, agent, day, spent_usd, budget_usd)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`, tenant, agent, day.UTC().Format("2006-01-02"), spent, budget)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		created = n == 1
		return err
	})
	return created, err
}
