package control

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// MgmtKeyPrefix marks management keys. They are for the CLI and CI
// (passport pushes; vault sync and skills later), never for agents.
const MgmtKeyPrefix = "bd_mgmt_"

// Management key scopes.
const (
	ScopePassportWrite = "passport:write"
	ScopeVaultSync     = "vault:sync"
	ScopeSkillsWrite   = "skills:write"
)

// Scopes lists every management key scope.
var Scopes = []string{ScopePassportWrite, ScopeVaultSync, ScopeSkillsWrite}

// ErrInvalidScopes is returned for an empty or unknown scope list.
var ErrInvalidScopes = errors.New("control: scopes must be one or more of " + strings.Join(Scopes, ", "))

// MgmtKey is a management key without its secret.
type MgmtKey struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	DisplayPrefix string     `json:"display_prefix"`
	Scopes        []string   `json:"scopes"`
	CreatedBy     string     `json:"created_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
}

func newMgmtKey() (key, display string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	key = MgmtKeyPrefix + strings.ToLower(b32.EncodeToString(b))
	return key, key[:len(MgmtKeyPrefix)+8], nil
}

// CleanScopes validates and orders a scope list.
func CleanScopes(scopes []string) ([]string, error) {
	seen := map[string]bool{}
	for _, s := range scopes {
		ok := false
		for _, known := range Scopes {
			ok = ok || s == known
		}
		if !ok {
			return nil, ErrInvalidScopes
		}
		seen[s] = true
	}
	var out []string
	for _, s := range Scopes {
		if seen[s] {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, ErrInvalidScopes
	}
	return out, nil
}

// CreateMgmtKey creates a management key and returns its secret, which is
// never stored or shown again.
func (s *Store) CreateMgmtKey(ctx context.Context, tenant, userID, name string, scopes []string) (MgmtKey, string, error) {
	scopes, err := CleanScopes(scopes)
	if err != nil {
		return MgmtKey{}, "", err
	}
	secret, display, err := newMgmtKey()
	if err != nil {
		return MgmtKey{}, "", err
	}
	id, err := newID("mkey_")
	if err != nil {
		return MgmtKey{}, "", err
	}
	k := MgmtKey{ID: id, Name: cleanName(name), DisplayPrefix: display, Scopes: scopes, CreatedBy: userID}
	var createdBy any
	if userID != "" {
		createdBy = userID
	}
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `INSERT INTO control.mgmt_keys (id, tenant_id, name, display_prefix, key_hash, scopes, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
			id, tenant, k.Name, display, HashKey(secret), pq.Array(scopes), createdBy).Scan(&k.CreatedAt)
	})
	k.CreatedAt = k.CreatedAt.UTC()
	return k, secret, err
}

// ListMgmtKeys returns the tenant's management keys, newest first.
func (s *Store) ListMgmtKeys(ctx context.Context, tenant string) ([]MgmtKey, error) {
	var out []MgmtKey
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, name, display_prefix, scopes, coalesce(created_by, ''), created_at, revoked_at, last_used_at
			FROM control.mgmt_keys WHERE tenant_id = $1 ORDER BY created_at DESC, id LIMIT 200`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k MgmtKey
			var scopes pq.StringArray
			var rev, used sql.NullTime
			if err := rows.Scan(&k.ID, &k.Name, &k.DisplayPrefix, &scopes, &k.CreatedBy, &k.CreatedAt, &rev, &used); err != nil {
				return err
			}
			k.Scopes, k.CreatedAt = append([]string{}, scopes...), k.CreatedAt.UTC()
			if rev.Valid {
				t := rev.Time.UTC()
				k.RevokedAt = &t
			}
			if used.Valid {
				t := used.Time.UTC()
				k.LastUsedAt = &t
			}
			out = append(out, k)
		}
		return rows.Err()
	})
	return out, err
}

// RevokeMgmtKey revokes a management key. Revoking twice is a no-op.
func (s *Store) RevokeMgmtKey(ctx context.Context, tenant, keyID string) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE control.mgmt_keys SET revoked_at = coalesce(revoked_at, now()) WHERE tenant_id = $1 AND id = $2`, tenant, keyID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// MgmtPrincipal is who a management key acts for.
type MgmtPrincipal struct {
	TenantID string
	KeyID    string
	Scopes   []string
}

// Has reports whether the key has a scope.
func (p MgmtPrincipal) Has(scope string) bool {
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// MgmtKeyResolver resolves management keys, as the ingest role, with a short
// cache. Unlike ingest keys there is no stale serving: a management action
// fails while the database is unreachable rather than trusting an old
// answer.
type MgmtKeyResolver struct {
	DB          *sql.DB
	TTL         time.Duration // default 30s
	NegativeTTL time.Duration // default 10s
	Now         func() time.Time

	mu    sync.Mutex
	cache map[string]mgmtEntry
}

type mgmtEntry struct {
	p  MgmtPrincipal
	ok bool
	at time.Time
}

// Resolve returns the key's principal. Only bd_mgmt_ keys are looked up, so
// an ingest key never resolves here.
func (r *MgmtKeyResolver) Resolve(ctx context.Context, key string) (MgmtPrincipal, bool, error) {
	if !strings.HasPrefix(key, MgmtKeyPrefix) || len(key) > 256 {
		return MgmtPrincipal{}, false, nil
	}
	h := hex.EncodeToString(HashKey(key))
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	r.mu.Lock()
	e, hit := r.cache[h]
	r.mu.Unlock()
	if hit {
		ttl := orDefault(r.TTL, 30*time.Second)
		if !e.ok {
			ttl = orDefault(r.NegativeTTL, 10*time.Second)
		}
		if now.Sub(e.at) < ttl {
			return e.p, e.ok, nil
		}
	}
	var p MgmtPrincipal
	var scopes pq.StringArray
	err := r.DB.QueryRowContext(ctx, `SELECT tenant_id, key_id, scopes FROM control.resolve_mgmt_key($1)`, HashKey(key)).Scan(&p.TenantID, &p.KeyID, &scopes)
	ok := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return MgmtPrincipal{}, false, fmt.Errorf("control: resolve management key: %w", err)
	}
	p.Scopes = append([]string{}, scopes...)
	r.mu.Lock()
	if r.cache == nil || len(r.cache) > 10_000 {
		r.cache = map[string]mgmtEntry{}
	}
	r.cache[h] = mgmtEntry{p: p, ok: ok, at: now}
	r.mu.Unlock()
	return p, ok, nil
}
