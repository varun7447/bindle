package control

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/bindle-ai/bindle/backend/internal/alerts"
	"github.com/bindle-ai/bindle/backend/internal/secretbox"
)

// SecretCounts reports what EncryptLegacySecrets changed.
type SecretCounts struct {
	AlertChannels    int
	OutcomeEndpoints int
}

// EncryptLegacySecrets encrypts secrets stored in plaintext before
// encryption existed and clears the plaintext. It is idempotent: rows that
// are already encrypted are skipped, and each row is updated only if its
// plaintext is unchanged since it was read. db must be the schema-owner
// connection (bindle-admin), which row-level security doesn't restrict.
func EncryptLegacySecrets(ctx context.Context, db *sql.DB, box secretbox.Box) (SecretCounts, error) {
	var n SecretCounts
	if box == nil {
		return n, secretbox.ErrNotConfigured
	}
	type channel struct{ id, tenant, kind, target, secret string }
	var chs []channel
	rows, err := db.QueryContext(ctx, `SELECT id, tenant_id, kind, COALESCE(target, ''), COALESCE(secret, '') FROM control.alert_channels
		WHERE target IS NOT NULL OR (secret IS NOT NULL AND secret <> '')`)
	if err != nil {
		return n, err
	}
	for rows.Next() {
		var c channel
		if err := rows.Scan(&c.id, &c.tenant, &c.kind, &c.target, &c.secret); err != nil {
			rows.Close()
			return n, err
		}
		chs = append(chs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return n, err
	}
	for _, c := range chs {
		targetEnc, err := box.Encrypt(ctx, []byte(c.target), secretbox.Context(c.tenant, secretbox.PurposeAlertTarget, c.id))
		if err != nil {
			return n, fmt.Errorf("channel %s: %w", c.id, err)
		}
		var secretEnc []byte
		if c.secret != "" {
			if secretEnc, err = box.Encrypt(ctx, []byte(c.secret), secretbox.Context(c.tenant, secretbox.PurposeAlertSecret, c.id)); err != nil {
				return n, fmt.Errorf("channel %s: %w", c.id, err)
			}
		}
		res, err := db.ExecContext(ctx, `UPDATE control.alert_channels SET target_enc = $2, secret_enc = $3, display = $4, target = NULL, secret = NULL
			WHERE id = $1 AND COALESCE(target, '') = $5 AND COALESCE(secret, '') = $6`,
			c.id, targetEnc, secretEnc, alerts.DisplayTarget(c.kind, c.target), c.target, c.secret)
		if err != nil {
			return n, err
		}
		if k, _ := res.RowsAffected(); k == 1 {
			n.AlertChannels++
		}
	}

	type endpoint struct{ id, tenant, secret string }
	var eps []endpoint
	rows, err = db.QueryContext(ctx, `SELECT id, tenant_id, secret FROM control.outcome_endpoints WHERE secret IS NOT NULL`)
	if err != nil {
		return n, err
	}
	for rows.Next() {
		var e endpoint
		if err := rows.Scan(&e.id, &e.tenant, &e.secret); err != nil {
			rows.Close()
			return n, err
		}
		eps = append(eps, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return n, err
	}
	for _, e := range eps {
		enc, err := box.Encrypt(ctx, []byte(e.secret), secretbox.Context(e.tenant, secretbox.PurposeOutcomeSecret, e.id))
		if err != nil {
			return n, fmt.Errorf("endpoint %s: %w", e.id, err)
		}
		res, err := db.ExecContext(ctx, `UPDATE control.outcome_endpoints SET secret_enc = $2, secret = NULL WHERE id = $1 AND secret = $3`, e.id, enc, e.secret)
		if err != nil {
			return n, err
		}
		if k, _ := res.RowsAffected(); k == 1 {
			n.OutcomeEndpoints++
		}
	}
	return n, nil
}
