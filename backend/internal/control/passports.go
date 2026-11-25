package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/passport"
)

// PassportVersion is one pushed version of an agent's passport.
type PassportVersion struct {
	Agent    string             `json:"agent"`
	Version  int                `json:"version"`
	SHA256   string             `json:"sha256"`
	PushedBy string             `json:"pushed_by"`
	PushedAt time.Time          `json:"pushed_at"`
	Passport *passport.Passport `json:"passport"`
}

// PushResult is what a passport push did.
type PushResult struct {
	Version   int       `json:"version"`
	PushedAt  time.Time `json:"pushed_at"`
	Unchanged bool      `json:"unchanged"` // the same document was already the active version
}

// PassportWriter pushes passports, connected as the ingest role (the public
// edge where management keys are accepted).
type PassportWriter struct{ DB *sql.DB }

// Push stores p as the agent's next version, unless it is the active one
// already. by is "key:<management key id>".
func (w *PassportWriter) Push(ctx context.Context, tenant string, p *passport.Passport, by string) (PushResult, error) {
	doc, digest, err := Canonical(p)
	if err != nil {
		return PushResult{}, err
	}
	var r PushResult
	err = w.DB.QueryRowContext(ctx, `SELECT version, pushed_at, unchanged FROM control.push_passport($1, $2, $3, $4, $5)`,
		tenant, p.Agent, doc, digest, by).Scan(&r.Version, &r.PushedAt, &r.Unchanged)
	r.PushedAt = r.PushedAt.UTC()
	return r, err
}

// Canonical is the passport as stored: JSON re-encoded from the parsed
// passport, so whitespace and key order don't create new versions.
func Canonical(p *passport.Passport) ([]byte, []byte, error) {
	doc, err := json.Marshal(p)
	if err != nil {
		return nil, nil, err
	}
	h := sha256.Sum256(doc)
	return doc, h[:], nil
}

// ListPassports returns each agent's active passport (api role).
func (s *Store) ListPassports(ctx context.Context, tenant string) ([]PassportVersion, error) {
	return s.passportRows(ctx, tenant, `SELECT DISTINCT ON (agent) agent, version, sha256, pushed_by, pushed_at, document
		FROM control.passports WHERE tenant_id = $1 ORDER BY agent, version DESC LIMIT 1000`, tenant)
}

// PassportHistory returns an agent's versions, newest first (at most 100).
func (s *Store) PassportHistory(ctx context.Context, tenant, agent string) ([]PassportVersion, error) {
	return s.passportRows(ctx, tenant, `SELECT agent, version, sha256, pushed_by, pushed_at, document
		FROM control.passports WHERE tenant_id = $1 AND agent = $2 ORDER BY version DESC LIMIT 100`, tenant, agent)
}

func (s *Store) passportRows(ctx context.Context, tenant, q string, args ...any) ([]PassportVersion, error) {
	out := []PassportVersion{}
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v PassportVersion
			var sum, doc []byte
			if err := rows.Scan(&v.Agent, &v.Version, &sum, &v.PushedBy, &v.PushedAt, &doc); err != nil {
				return err
			}
			p, err := passport.Parse(doc, nil)
			if err != nil {
				return err
			}
			v.SHA256, v.PushedAt, v.Passport = hex.EncodeToString(sum), v.PushedAt.UTC(), p
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

// ActivePassports returns a tenant's active passports, read through
// control.passports_for (ingest and alerts roles).
func ActivePassports(ctx context.Context, db *sql.DB, tenant string) (passport.Set, error) {
	rows, err := db.QueryContext(ctx, `SELECT agent, document FROM control.passports_for($1)`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := passport.Set{}
	for rows.Next() {
		var agent string
		var doc []byte
		if err := rows.Scan(&agent, &doc); err != nil {
			return nil, err
		}
		p, err := passport.Parse(doc, nil)
		if err != nil {
			return nil, err
		}
		set[agent] = p
	}
	return set, rows.Err()
}

// PassportReader reads active passports as the alerts role.
type PassportReader struct{ DB *sql.DB }

// Tenants returns tenants with a passport and the passports feature on.
func (r *PassportReader) Tenants(ctx context.Context) ([]string, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT control.passport_tenants()`)
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

// Passports returns the tenant's active passports.
func (r *PassportReader) Passports(ctx context.Context, tenant string) (passport.Set, error) {
	return ActivePassports(ctx, r.DB, tenant)
}

// PassportResolver caches each tenant's active passports for the pipeline.
// Only tenants with the "passports" feature have any. On a database error
// it keeps the last known set for StaleTTL, then reports none: passports
// are observe-only, so a lookup failure never holds up ingest.
type PassportResolver struct {
	DB       *sql.DB
	Features interface {
		Enabled(ctx context.Context, tenant, feature string) bool
	}
	Log      *slog.Logger
	TTL      time.Duration // default 30s
	StaleTTL time.Duration // default 10m
	Now      func() time.Time

	mu    sync.Mutex
	cache map[string]passportEntry
}

type passportEntry struct {
	set passport.Set
	at  time.Time
}

// Passports returns the tenant's active passports (nil: none, or the
// feature is off).
func (r *PassportResolver) Passports(ctx context.Context, tenant string) passport.Set {
	if r == nil || r.DB == nil || tenant == "" || r.Features == nil || !r.Features.Enabled(ctx, tenant, passport.Feature) {
		return nil
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	r.mu.Lock()
	e, ok := r.cache[tenant]
	r.mu.Unlock()
	if ok && now.Sub(e.at) < orDefault(r.TTL, 30*time.Second) {
		return e.set
	}
	set, err := ActivePassports(ctx, r.DB, tenant)
	if err != nil {
		if r.Log != nil {
			r.Log.Warn("passport lookup failed; drift detection uses the last known passports", "tenant", tenant, "err", err)
		}
		if ok && now.Sub(e.at) < orDefault(r.StaleTTL, 10*time.Minute) {
			return e.set
		}
		return nil
	}
	r.mu.Lock()
	if r.cache == nil || len(r.cache) > 100_000 {
		r.cache = map[string]passportEntry{}
	}
	r.cache[tenant] = passportEntry{set: set, at: now}
	r.mu.Unlock()
	return set
}
