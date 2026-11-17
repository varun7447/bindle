package control

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/bindle-ai/bindle/services/internal/alerts"
)

// Store is the API's access to the control plane. Connect it as the API role.
type Store struct {
	DB *sql.DB
}

// ErrNotFound means the row does not exist for this tenant.
var ErrNotFound = errors.New("control: not found")

// Roles a member can have. Owners and admins manage keys.
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// User is a person, identified by their Cognito sub.
type User struct {
	ID    string
	Sub   string
	Email string
}

// Membership is a user's tenant and role.
type Membership struct {
	TenantID   string
	TenantName string
	Role       string
}

// CanManageKeys reports whether the role may create, rotate or revoke keys.
func (m Membership) CanManageKeys() bool { return m.Role == RoleOwner || m.Role == RoleAdmin }

// APIKey describes a key without its secret.
type APIKey struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	DisplayPrefix string     `json:"display_prefix"`
	CreatedBy     string     `json:"created_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	ReplacedBy    string     `json:"replaced_by,omitempty"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
}

// EnsureUser returns the user for a Cognito sub, creating it on first sign-in.
func (s *Store) EnsureUser(ctx context.Context, sub, email string) (User, error) {
	if sub == "" {
		return User{}, errors.New("control: empty subject")
	}
	u := User{Sub: sub}
	err := s.DB.QueryRowContext(ctx, `SELECT id, email FROM control.users WHERE cognito_sub = $1`, sub).Scan(&u.ID, &u.Email)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return User{}, err
	}
	id, err := newID("usr_")
	if err != nil {
		return User{}, err
	}
	// ON CONFLICT handles two first requests racing; either way, read back.
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO control.users (id, cognito_sub, email) VALUES ($1, $2, $3)
		ON CONFLICT (cognito_sub) DO NOTHING`, id, sub, email); err != nil {
		return User{}, err
	}
	err = s.DB.QueryRowContext(ctx, `SELECT id, email FROM control.users WHERE cognito_sub = $1`, sub).Scan(&u.ID, &u.Email)
	return u, err
}

// MembershipFor returns the user's tenant and role; ErrNotFound if none.
func (s *Store) MembershipFor(ctx context.Context, userID string) (Membership, error) {
	var m Membership
	err := s.DB.QueryRowContext(ctx, `SELECT m.tenant_id, t.name, m.role
		FROM control.memberships m JOIN control.tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1`, userID).Scan(&m.TenantID, &m.TenantName, &m.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return Membership{}, ErrNotFound
	}
	return m, err
}

// Provision creates a tenant with the user as owner, unless the user already
// has one (then it is returned). Safe against concurrent first requests.
func (s *Store) Provision(ctx context.Context, u User, tenantName string) (Membership, error) {
	if m, err := s.MembershipFor(ctx, u.ID); err == nil || !errors.Is(err, ErrNotFound) {
		return m, err
	}
	tid, err := newID("tn_")
	if err != nil {
		return Membership{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Membership{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO control.tenants (id, name) VALUES ($1, $2)`, tid, tenantName); err != nil {
		return Membership{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO control.memberships (tenant_id, user_id, role) VALUES ($1, $2, 'owner')`, tid, u.ID)
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" { // the user got a tenant concurrently
		_ = tx.Rollback()
		return s.MembershipFor(ctx, u.ID)
	}
	if err != nil {
		return Membership{}, err
	}
	// Every workspace starts with the default alert rules (no channels yet,
	// so they show in the console until a channel is added).
	if _, err := tx.ExecContext(ctx, `SELECT set_config('bindle.tenant', $1, true)`, tid); err != nil {
		return Membership{}, err
	}
	for _, r := range alerts.DefaultRules() {
		if _, err := insertRule(ctx, tx, tid, u.ID, r); err != nil {
			return Membership{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Membership{}, err
	}
	return Membership{TenantID: tid, TenantName: tenantName, Role: RoleOwner}, nil
}

// inTenant runs fn in a transaction scoped to tenant by row-level security.
func (s *Store) inTenant(ctx context.Context, tenant string, fn func(*sql.Tx) error) error {
	if tenant == "" {
		return errors.New("control: tenant is required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('bindle.tenant', $1, true), set_config('statement_timeout', '5000', true)`, tenant); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

const maxKeyName = 100

func cleanName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "default"
	}
	if len(name) > maxKeyName {
		name = name[:maxKeyName]
	}
	return name
}

func insertKey(ctx context.Context, tx *sql.Tx, tenant, userID, name string) (APIKey, string, error) {
	secret, display, err := newKey()
	if err != nil {
		return APIKey{}, "", err
	}
	id, err := newID("key_")
	if err != nil {
		return APIKey{}, "", err
	}
	k := APIKey{ID: id, Name: cleanName(name), DisplayPrefix: display, CreatedBy: userID}
	var createdBy any
	if userID != "" {
		createdBy = userID
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO control.api_keys (id, tenant_id, name, display_prefix, key_hash, created_by)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`,
		id, tenant, k.Name, display, HashKey(secret), createdBy).Scan(&k.CreatedAt)
	return k, secret, err
}

// CreateKey creates a key and returns it with its secret, which is never
// stored or shown again.
func (s *Store) CreateKey(ctx context.Context, tenant, userID, name string) (APIKey, string, error) {
	var k APIKey
	var secret string
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var err error
		k, secret, err = insertKey(ctx, tx, tenant, userID, name)
		return err
	})
	return k, secret, err
}

const keyCols = `id, name, display_prefix, coalesce(created_by, ''), created_at, expires_at, revoked_at, coalesce(replaced_by, ''), last_used_at`

func scanKey(sc interface{ Scan(...any) error }) (APIKey, error) {
	var k APIKey
	var exp, rev, used sql.NullTime
	err := sc.Scan(&k.ID, &k.Name, &k.DisplayPrefix, &k.CreatedBy, &k.CreatedAt, &exp, &rev, &k.ReplacedBy, &used)
	tp := func(t sql.NullTime) *time.Time {
		if !t.Valid {
			return nil
		}
		v := t.Time.UTC()
		return &v
	}
	k.CreatedAt = k.CreatedAt.UTC()
	k.ExpiresAt, k.RevokedAt, k.LastUsedAt = tp(exp), tp(rev), tp(used)
	return k, err
}

// ListKeys returns the tenant's keys, newest first, without secrets.
func (s *Store) ListKeys(ctx context.Context, tenant string) ([]APIKey, error) {
	var out []APIKey
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+keyCols+` FROM control.api_keys WHERE tenant_id = $1 ORDER BY created_at DESC, id LIMIT 500`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			k, err := scanKey(rows)
			if err != nil {
				return err
			}
			out = append(out, k)
		}
		return rows.Err()
	})
	return out, err
}

// RevokeKey revokes a key immediately. Revoking a revoked key is a no-op.
func (s *Store) RevokeKey(ctx context.Context, tenant, keyID string) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE control.api_keys SET revoked_at = coalesce(revoked_at, now())
			WHERE tenant_id = $1 AND id = $2`, tenant, keyID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RotateKey creates a replacement key with the same name. The old key keeps
// working for grace (so running agents can be redeployed), then expires.
// Rotating a revoked or already-rotated key is refused with ErrNotFound.
func (s *Store) RotateKey(ctx context.Context, tenant, userID, keyID string, grace time.Duration) (APIKey, string, error) {
	var k APIKey
	var secret string
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var name string
		err := tx.QueryRowContext(ctx, `SELECT name FROM control.api_keys
			WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL AND replaced_by IS NULL
			  AND (expires_at IS NULL OR expires_at > now())
			FOR UPDATE`, tenant, keyID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if k, secret, err = insertKey(ctx, tx, tenant, userID, name); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE control.api_keys SET replaced_by = $3, expires_at = now() + $4 * interval '1 second'
			WHERE tenant_id = $1 AND id = $2`, tenant, keyID, k.ID, int64(grace/time.Second))
		return err
	})
	return k, secret, err
}

// SeedKey makes an existing key value valid for a tenant, creating the
// tenant if needed. For local development only (deploy/dev-env.sh); run it
// as the schema owner. Idempotent.
func SeedKey(ctx context.Context, owner *sql.DB, tenantID, tenantName, key string) error {
	if !strings.HasPrefix(key, "bd_") || len(key) < 24 {
		return errors.New("control: seed key must look like an ingest key")
	}
	tx, err := owner.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `INSERT INTO control.tenants (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`, tenantID, tenantName)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 { // new tenant: default alert rules, as in Provision
		for _, r := range alerts.DefaultRules() {
			if _, err := insertRule(ctx, tx, tenantID, "", r); err != nil {
				return err
			}
		}
	}
	id, err := newID("key_")
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO control.api_keys (id, tenant_id, name, display_prefix, key_hash)
		VALUES ($1, $2, 'local dev', $3, $4) ON CONFLICT (key_hash) DO NOTHING`,
		id, tenantID, key[:min(len(key), 16)], HashKey(key)); err != nil {
		return err
	}
	return tx.Commit()
}
