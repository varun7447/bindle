package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/cost"
	"github.com/bindle-ai/bindle/backend/internal/sdkconfig"
	"github.com/lib/pq"
)

// ErrVersionConflict means the config changed since the caller read it.
var ErrVersionConflict = errors.New("control: config was changed by someone else")

// GetSDKConfig returns the tenant's SDK config, or defaults (version 0).
func (s *Store) GetSDKConfig(ctx context.Context, tenant string) (sdkconfig.Config, error) {
	c := sdkconfig.Default()
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var patterns, limits, prices []byte
		err := tx.QueryRowContext(ctx, `SELECT version, sample_rate, capture_content, redact_patterns, limits, prices, updated_at
			FROM control.sdk_config WHERE tenant_id = $1`, tenant).Scan(&c.Version, &c.SampleRate, &c.CaptureContent, &patterns, &limits, &prices, &c.UpdatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		c.UpdatedAt = c.UpdatedAt.UTC()
		if err := json.Unmarshal(patterns, &c.RedactPatterns); err != nil {
			return err
		}
		if err := json.Unmarshal(limits, &c.Limits); err != nil {
			return err
		}
		if err := json.Unmarshal(prices, &c.Prices); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT agent FROM control.budget_state
			WHERE tenant_id = $1 AND day = (now() AT TIME ZONE 'UTC')::date ORDER BY agent`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				return err
			}
			c.OverBudget = append(c.OverBudget, a)
		}
		return rows.Err()
	})
	return c, err
}

// PutSDKConfig saves a validated config if the stored version is still
// c.Version (0 for "never saved"), and returns it with the new version.
func (s *Store) PutSDKConfig(ctx context.Context, tenant, userID string, c sdkconfig.Config) (sdkconfig.Config, error) {
	if err := sdkconfig.Validate(c); err != nil {
		return c, err
	}
	if c.RedactPatterns == nil {
		c.RedactPatterns = []sdkconfig.Pattern{}
	}
	patterns, err := json.Marshal(c.RedactPatterns)
	if err != nil {
		return c, err
	}
	var by any
	if userID != "" {
		by = userID
	}
	out := c
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var row *sql.Row
		if c.Version == 0 {
			row = tx.QueryRowContext(ctx, `INSERT INTO control.sdk_config (tenant_id, version, sample_rate, capture_content, redact_patterns, updated_by)
				VALUES ($1, 1, $2, $3, $4, $5) ON CONFLICT (tenant_id) DO NOTHING RETURNING version, updated_at`,
				tenant, c.SampleRate, c.CaptureContent, patterns, by)
		} else {
			row = tx.QueryRowContext(ctx, `UPDATE control.sdk_config
				SET version = version + 1, sample_rate = $3, capture_content = $4, redact_patterns = $5, updated_by = $6, updated_at = now()
				WHERE tenant_id = $1 AND version = $2 RETURNING version, updated_at`,
				tenant, c.Version, c.SampleRate, c.CaptureContent, patterns, by)
		}
		err := row.Scan(&out.Version, &out.UpdatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrVersionConflict
		}
		out.UpdatedAt = out.UpdatedAt.UTC()
		return err
	})
	return out, err
}

// PutLimits saves validated limits if the stored version is still version
// (0 for "never saved"), and returns the whole config with the new version.
// Limits share the config's version, so an edit to either is detected.
func (s *Store) PutLimits(ctx context.Context, tenant, userID string, version int64, limits []sdkconfig.Limit) (sdkconfig.Config, error) {
	if err := sdkconfig.ValidateLimits(limits); err != nil {
		return sdkconfig.Config{}, err
	}
	if limits == nil {
		limits = []sdkconfig.Limit{}
	}
	return s.putColumn(ctx, tenant, userID, version, "limits", limits)
}

// PutPrices saves validated model price overrides, like PutLimits.
func (s *Store) PutPrices(ctx context.Context, tenant, userID string, version int64, prices []sdkconfig.PriceOverride) (sdkconfig.Config, error) {
	if err := sdkconfig.ValidatePrices(prices); err != nil {
		return sdkconfig.Config{}, err
	}
	if prices == nil {
		prices = []sdkconfig.PriceOverride{}
	}
	return s.putColumn(ctx, tenant, userID, version, "prices", prices)
}

// putColumn writes one JSON column (limits or prices) under the shared
// config version.
func (s *Store) putColumn(ctx context.Context, tenant, userID string, version int64, column string, v any) (sdkconfig.Config, error) {
	if column != "limits" && column != "prices" {
		return sdkconfig.Config{}, errors.New("control: unknown config column")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return sdkconfig.Config{}, err
	}
	var by any
	if userID != "" {
		by = userID
	}
	d := sdkconfig.Default()
	err = s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var res sql.Result
		var err error
		if version == 0 {
			// column is one of two constants checked above, never input.
			res, err = tx.ExecContext(ctx, `INSERT INTO control.sdk_config (tenant_id, version, sample_rate, capture_content, redact_patterns, `+column+`, updated_by)
				VALUES ($1, 1, $2, $3, '[]', $4, $5) ON CONFLICT (tenant_id) DO NOTHING`, tenant, d.SampleRate, d.CaptureContent, raw, by)
		} else {
			res, err = tx.ExecContext(ctx, `UPDATE control.sdk_config
				SET version = version + 1, `+column+` = $3, updated_by = $4, updated_at = now()
				WHERE tenant_id = $1 AND version = $2`, tenant, version, raw, by)
		}
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrVersionConflict
		}
		return nil
	})
	if err != nil {
		return sdkconfig.Config{}, err
	}
	return s.GetSDKConfig(ctx, tenant)
}

// ConfigResolver gives ingest each tenant's config with a short-TTL cache.
// If the database is unreachable, the last known config is served for up
// to StaleTTL; with none known, an error is returned so ingest can answer
// 503 (exporters retry) rather than guess, since a tenant may have turned
// content capture off.
type ConfigResolver struct {
	DB       *sql.DB
	Log      *slog.Logger
	TTL      time.Duration // default 30s
	StaleTTL time.Duration // default 10m
	Now      func() time.Time

	mu    sync.Mutex
	cache map[string]cfgEntry
	group singleflight
}

type cfgEntry struct {
	cfg    sdkconfig.Config
	red    *sdkconfig.Redactor
	prices *cost.Table
	at     time.Time
}

// Policy returns the tenant's config and compiled extra patterns.
func (r *ConfigResolver) Policy(ctx context.Context, tenant string) (sdkconfig.Policy, error) {
	now := r.now()
	r.mu.Lock()
	e, ok := r.cache[tenant]
	r.mu.Unlock()
	if ok && now.Sub(e.at) < orDefault(r.TTL, 30*time.Second) {
		return sdkconfig.Policy{Config: e.cfg, Redactor: e.red, Prices: e.prices}, nil
	}
	_, err := r.group.do("cfg:"+tenant, func() (entry, error) {
		c := sdkconfig.Default()
		var patterns, limits, prices []byte
		var over, stopped pq.StringArray
		err := r.DB.QueryRowContext(ctx, `SELECT version, sample_rate, capture_content, redact_patterns, limits, over_budget, prices, updated_at, stopped_runs FROM control.sdk_config_for($1)`, tenant).
			Scan(&c.Version, &c.SampleRate, &c.CaptureContent, &patterns, &limits, &over, &prices, &c.UpdatedAt, &stopped)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return entry{}, err
		default:
			if err := json.Unmarshal(patterns, &c.RedactPatterns); err != nil {
				return entry{}, err
			}
			if err := json.Unmarshal(limits, &c.Limits); err != nil {
				return entry{}, err
			}
			c.OverBudget = append([]string{}, over...)
			c.StoppedRuns = append([]string{}, stopped...)
			if c.Version == 0 {
				c.UpdatedAt = time.Time{} // never saved: defaults
			}
			if err := json.Unmarshal(prices, &c.Prices); err != nil {
				return entry{}, err
			}
		}
		r.mu.Lock()
		if r.cache == nil || len(r.cache) > 100_000 {
			r.cache = map[string]cfgEntry{}
		}
		r.cache[tenant] = cfgEntry{cfg: c, red: sdkconfig.NewRedactor(c.RedactPatterns), prices: sdkconfig.PriceTable(c.Prices), at: now}
		r.mu.Unlock()
		return entry{ok: true}, nil
	})
	if err != nil {
		if ok && now.Sub(e.at) < orDefault(r.StaleTTL, 10*time.Minute) {
			if r.Log != nil {
				r.Log.Warn("config lookup failed; using last known config", "tenant", tenant, "err", err)
			}
			return sdkconfig.Policy{Config: e.cfg, Redactor: e.red, Prices: e.prices}, nil
		}
		return sdkconfig.Policy{}, err
	}
	r.mu.Lock()
	e = r.cache[tenant]
	r.mu.Unlock()
	return sdkconfig.Policy{Config: e.cfg, Redactor: e.red, Prices: e.prices}, nil
}

func (r *ConfigResolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
