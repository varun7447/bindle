package control

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/alerts"
	"github.com/bindle-ai/bindle/backend/internal/secretbox"
)

const ruleCols = `id, tenant_id, kind, threshold, window_minutes, min_runs, enabled, created_at`

func scanRule(sc interface{ Scan(...any) error }) (alerts.Rule, error) {
	var r alerts.Rule
	err := sc.Scan(&r.ID, &r.TenantID, &r.Kind, &r.Threshold, &r.WindowMinutes, &r.MinRuns, &r.Enabled, &r.CreatedAt)
	return r, err
}

// Targets and signing secrets are stored encrypted (target_enc, secret_enc);
// target and secret hold plaintext only for rows written before encryption.
const channelCols = `id, tenant_id, kind, name, COALESCE(target, ''), COALESCE(secret, ''), target_enc, secret_enc, display, enabled, created_at`

// channelRow is a channel as stored: Target and Secret are set only for
// legacy plaintext rows until reveal decrypts the ciphertexts.
type channelRow struct {
	alerts.Channel
	targetEnc, secretEnc []byte
}

func scanChannel(sc interface{ Scan(...any) error }) (channelRow, error) {
	var r channelRow
	c := &r.Channel
	err := sc.Scan(&c.ID, &c.TenantID, &c.Kind, &c.Name, &c.Target, &c.Secret, &r.targetEnc, &r.secretEnc, &c.Display, &c.Enabled, &c.CreatedAt)
	if c.Display == "" && c.Target != "" {
		c.Display = alerts.DisplayTarget(c.Kind, c.Target)
	}
	return r, err
}

// reveal decrypts a channel's target and secret for delivery.
func reveal(ctx context.Context, box secretbox.Box, r channelRow) (alerts.Channel, error) {
	c := r.Channel
	if r.targetEnc != nil {
		pt, err := boxOrDisabled(box).Decrypt(ctx, r.targetEnc, secretbox.Context(c.TenantID, secretbox.PurposeAlertTarget, c.ID))
		if err != nil {
			return alerts.Channel{}, fmt.Errorf("channel %s target: %w", c.ID, err)
		}
		c.Target = string(pt)
	}
	if r.secretEnc != nil {
		pt, err := boxOrDisabled(box).Decrypt(ctx, r.secretEnc, secretbox.Context(c.TenantID, secretbox.PurposeAlertSecret, c.ID))
		if err != nil {
			return alerts.Channel{}, fmt.Errorf("channel %s secret: %w", c.ID, err)
		}
		c.Secret = string(pt)
	}
	return c, nil
}

func boxOrDisabled(b secretbox.Box) secretbox.Box {
	if b == nil {
		return secretbox.Disabled{}
	}
	return b
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

func listChannels(ctx context.Context, tx *sql.Tx) ([]channelRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+channelCols+` FROM control.alert_channels ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []channelRow
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

// ListAlertChannels returns the tenant's channels for listing: names and
// masked targets, not decrypted (Target and Secret are empty except on rows
// written before encryption; the API never returns them).
func (s *Store) ListAlertChannels(ctx context.Context, tenant string) (out []alerts.Channel, err error) {
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := listChannels(ctx, tx)
		for _, r := range rows {
			out = append(out, r.Channel)
		}
		return err
	})
	return out, err
}

// DeliverableChannels returns the tenant's channels with targets and
// secrets decrypted, for the alerts worker.
func (s *Store) DeliverableChannels(ctx context.Context, tenant string) ([]alerts.Channel, error) {
	var rows []channelRow
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) (err error) { rows, err = listChannels(ctx, tx); return err })
	if err != nil {
		return nil, err
	}
	out := make([]alerts.Channel, 0, len(rows))
	for _, r := range rows {
		c, err := reveal(ctx, s.Secrets, r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// GetAlertChannel returns one channel, decrypted. The alerts worker uses it
// for test deliveries; the API never calls it.
func (s *Store) GetAlertChannel(ctx context.Context, tenant, id string) (out alerts.Channel, err error) {
	var r channelRow
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		r, err = scanChannel(tx.QueryRowContext(ctx, `SELECT `+channelCols+` FROM control.alert_channels WHERE id = $1`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return alerts.Channel{}, err
	}
	return reveal(ctx, s.Secrets, r)
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
	// Encrypt before storing; without a secrets key the channel isn't created.
	box := boxOrDisabled(s.Secrets)
	targetEnc, err := box.Encrypt(ctx, []byte(c.Target), secretbox.Context(tenant, secretbox.PurposeAlertTarget, id))
	if err != nil {
		return alerts.Channel{}, "", err
	}
	var secretEnc []byte
	if secret != "" {
		if secretEnc, err = box.Encrypt(ctx, []byte(secret), secretbox.Context(tenant, secretbox.PurposeAlertSecret, id)); err != nil {
			return alerts.Channel{}, "", err
		}
	}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		r, err := scanChannel(tx.QueryRowContext(ctx, `INSERT INTO control.alert_channels (id, tenant_id, kind, name, target_enc, secret_enc, display, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+channelCols, id, tenant, c.Kind, c.Name, targetEnc, secretEnc, alerts.DisplayTarget(c.Kind, c.Target), userID))
		out = r.Channel
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
type AlertStore struct {
	DB      *sql.DB
	Secrets secretbox.Box // decrypts channel targets and signing secrets
}

func (a *AlertStore) store() *Store { return &Store{DB: a.DB, Secrets: a.Secrets} }

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

// Channels implements alerts.Store: channels ready to deliver to.
func (a *AlertStore) Channels(ctx context.Context, tenant string) ([]alerts.Channel, error) {
	return a.store().DeliverableChannels(ctx, tenant)
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

// ---- test deliveries --------------------------------------------------------
//
// "Send test" in the console: the API records a request (it never decrypts
// channel targets or secrets), and the alerts worker delivers it.

// ChannelTest is a test delivery's state as the console sees it.
type ChannelTest struct {
	ID     string `json:"id"`
	Status string `json:"status"` // pending, running, ok or failed
	Error  string `json:"error,omitempty"`
}

// RequestChannelTest records a test delivery for one of the tenant's
// channels. ErrNotFound if the channel isn't the tenant's.
func (s *Store) RequestChannelTest(ctx context.Context, tenant, userID, channelID string) (ChannelTest, error) {
	id, err := newID("ct_")
	if err != nil {
		return ChannelTest{}, err
	}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM control.alert_channels WHERE id = $1`, channelID).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO control.alert_channel_tests (tenant_id, id, channel_id, requested_by) VALUES ($1, $2, $3, NULLIF($4, ''))`,
			tenant, id, channelID, userID)
		return err
	})
	if err != nil {
		return ChannelTest{}, err
	}
	return ChannelTest{ID: id, Status: "pending"}, nil
}

// GetChannelTest returns one test delivery of a channel. ErrNotFound if it
// isn't the tenant's.
func (s *Store) GetChannelTest(ctx context.Context, tenant, channelID, testID string) (out ChannelTest, err error) {
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var msg sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id, status, error FROM control.alert_channel_tests WHERE id = $1 AND channel_id = $2`, testID, channelID).
			Scan(&out.ID, &out.Status, &msg)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		out.Error = msg.String
		return err
	})
	return out, err
}

// ClaimChannelTests implements alerts.TestStore.
func (a *AlertStore) ClaimChannelTests(ctx context.Context, max int) ([]alerts.ChannelTestJob, error) {
	rows, err := a.DB.QueryContext(ctx, `SELECT tenant_id, id, channel_id FROM control.claim_channel_tests($1)`, max)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alerts.ChannelTestJob
	for rows.Next() {
		var j alerts.ChannelTestJob
		if err := rows.Scan(&j.TenantID, &j.ID, &j.ChannelID); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// TestChannel implements alerts.TestStore: the channel, decrypted.
func (a *AlertStore) TestChannel(ctx context.Context, tenant, channelID string) (alerts.Channel, error) {
	return a.store().GetAlertChannel(ctx, tenant, channelID)
}

// FinishChannelTest implements alerts.TestStore.
func (a *AlertStore) FinishChannelTest(ctx context.Context, tenant, id string, ok bool, message string) error {
	_, err := a.DB.ExecContext(ctx, `SELECT control.finish_channel_test($1, $2, $3, $4)`, tenant, id, ok, message)
	return err
}
