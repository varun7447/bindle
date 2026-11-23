package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/lib/pq"
)

// Features that can be turned on per workspace. All are off by default; only
// the operator turns them on (bindle-admin features enable).
var Features = []string{"ledger", "passports", "outcome_grading", "learning_loop"}

// IsFeature reports whether name is a known feature.
func IsFeature(name string) bool {
	for _, f := range Features {
		if f == name {
			return true
		}
	}
	return false
}

// FeatureResolver answers "is this feature on for this tenant?" for the API,
// the pipeline and workers. Lookups are cached for TTL; when the database
// can't be reached, the last known answer is used for up to StaleTTL, and
// after that features count as off (fail closed).
type FeatureResolver struct {
	DB       *sql.DB
	Log      *slog.Logger
	TTL      time.Duration // default 30s
	StaleTTL time.Duration // default 10m
	Now      func() time.Time

	mu    sync.Mutex
	cache map[string]featureEntry
}

type featureEntry struct {
	on map[string]bool
	at time.Time
}

func (r *FeatureResolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// List returns the tenant's enabled features, sorted.
func (r *FeatureResolver) List(ctx context.Context, tenant string) ([]string, error) {
	on, err := r.lookup(ctx, tenant)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(on))
	for f := range on {
		out = append(out, f)
	}
	sort.Strings(out)
	return out, nil
}

// Enabled reports whether feature is on for tenant. Errors count as off.
func (r *FeatureResolver) Enabled(ctx context.Context, tenant, feature string) bool {
	on, err := r.lookup(ctx, tenant)
	return err == nil && on[feature]
}

func (r *FeatureResolver) lookup(ctx context.Context, tenant string) (map[string]bool, error) {
	if r == nil || r.DB == nil || tenant == "" {
		return map[string]bool{}, nil
	}
	now := r.now()
	r.mu.Lock()
	e, ok := r.cache[tenant]
	r.mu.Unlock()
	if ok && now.Sub(e.at) < orDefault(r.TTL, 30*time.Second) {
		return e.on, nil
	}
	var names pq.StringArray
	err := r.DB.QueryRowContext(ctx, `SELECT control.features_for($1)`, tenant).Scan(&names)
	if err != nil {
		if ok && now.Sub(e.at) < orDefault(r.StaleTTL, 10*time.Minute) {
			if r.Log != nil {
				r.Log.Warn("feature lookup failed; using last known flags", "tenant", tenant, "err", err)
			}
			return e.on, nil
		}
		return nil, err
	}
	on := make(map[string]bool, len(names))
	for _, n := range names {
		on[n] = true
	}
	r.mu.Lock()
	if r.cache == nil || len(r.cache) > 100_000 {
		r.cache = map[string]featureEntry{}
	}
	r.cache[tenant] = featureEntry{on: on, at: now}
	r.mu.Unlock()
	return on, nil
}

// FeatureFlag is one enabled feature, as listed by bindle-admin.
type FeatureFlag struct {
	TenantID  string
	Feature   string
	EnabledBy string
	EnabledAt time.Time
}

// SetFeature turns a feature on or off for a tenant. db must be connected
// as the schema owner: no app role can write flags.
func SetFeature(ctx context.Context, db *sql.DB, tenant, feature string, on bool, by string) error {
	if !IsFeature(feature) {
		return fmt.Errorf("unknown feature %q (known: %v)", feature, Features)
	}
	if tenant == "" {
		return errors.New("tenant is required")
	}
	if !on {
		_, err := db.ExecContext(ctx, `DELETE FROM control.tenant_features WHERE tenant_id = $1 AND feature = $2`, tenant, feature)
		return err
	}
	if by == "" {
		by = "operator"
	}
	res, err := db.ExecContext(ctx, `INSERT INTO control.tenant_features (tenant_id, feature, enabled_by)
		SELECT id, $2, $3 FROM control.tenants WHERE id = $1
		ON CONFLICT (tenant_id, feature) DO NOTHING`, tenant, feature, by)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM control.tenants WHERE id = $1)`, tenant).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("no tenant %q", tenant)
		}
	}
	return nil
}

// ListFeatures lists enabled features, for one tenant or (tenant "") all.
func ListFeatures(ctx context.Context, db *sql.DB, tenant string) ([]FeatureFlag, error) {
	rows, err := db.QueryContext(ctx, `SELECT tenant_id, feature, enabled_by, enabled_at FROM control.tenant_features
		WHERE $1 = '' OR tenant_id = $1 ORDER BY tenant_id, feature`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeatureFlag
	for rows.Next() {
		var f FeatureFlag
		if err := rows.Scan(&f.TenantID, &f.Feature, &f.EnabledBy, &f.EnabledAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
