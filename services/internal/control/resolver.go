package control

import (
	"context"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/lib/pq"

	"github.com/bindle-ai/bindle/services/internal/auth"
)

// KeyResolver resolves ingest keys to tenants with a short-TTL cache. It
// implements auth.Resolver. Connect it as the ingest role.
//
// Revoking a key takes effect within TTL. If the database is unreachable,
// keys seen recently keep working for StaleTTL so a database blip does not
// drop customers' telemetry; unknown keys are rejected.
type KeyResolver struct {
	DB          *sql.DB
	Log         *slog.Logger
	TTL         time.Duration // valid keys; default 60s
	NegativeTTL time.Duration // unknown/revoked keys; default 10s
	StaleTTL    time.Duration // serve last good answer on DB errors; default 10m
	MaxEntries  int           // default 100_000
	Now         func() time.Time

	mu      sync.Mutex
	cache   map[string]entry // hex(sha256(key)) -> entry
	used    map[string]bool  // key IDs seen since last flush
	lookups singleflight
}

type entry struct {
	tenant, keyID string
	ok            bool
	at            time.Time
}

var _ auth.Resolver = (*KeyResolver)(nil)

// Tenant implements auth.Resolver.
func (r *KeyResolver) Tenant(req *http.Request) (string, bool) {
	key := auth.KeyFromRequest(req)
	if key == "" || len(key) > 256 {
		return "", false
	}
	return r.Resolve(req.Context(), key)
}

// Resolve returns the tenant for a key.
func (r *KeyResolver) Resolve(ctx context.Context, key string) (string, bool) {
	h := HashKey(key)
	hk := hex.EncodeToString(h)
	now := r.now()

	r.mu.Lock()
	e, cached := r.cache[hk]
	r.mu.Unlock()
	if cached {
		ttl := r.ttl()
		if !e.ok {
			ttl = r.negTTL()
		}
		if now.Sub(e.at) < ttl {
			if e.ok {
				r.markUsed(e.keyID)
			}
			return e.tenant, e.ok
		}
	}

	res, err := r.lookups.do(hk, func() (entry, error) {
		var t, id string
		err := r.DB.QueryRowContext(ctx, `SELECT tenant_id, key_id FROM control.resolve_key($1)`, h).Scan(&t, &id)
		switch {
		case err == sql.ErrNoRows:
			return entry{at: now}, nil
		case err != nil:
			return entry{}, err
		}
		return entry{tenant: t, keyID: id, ok: true, at: now}, nil
	})
	if err != nil {
		r.logger().Error("key lookup failed", "err", err)
		if cached && e.ok && now.Sub(e.at) < r.staleTTL() {
			return e.tenant, true // fail open only for keys that were valid recently
		}
		return "", false
	}
	r.store(hk, res)
	if res.ok {
		r.markUsed(res.keyID)
	}
	return res.tenant, res.ok
}

func (r *KeyResolver) store(hk string, e entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = map[string]entry{}
	}
	max := r.MaxEntries
	if max <= 0 {
		max = 100_000
	}
	if len(r.cache) >= max { // bound memory: invalid keys can't grow it forever
		for k := range r.cache {
			delete(r.cache, k)
			if len(r.cache) < max/2 {
				break
			}
		}
	}
	r.cache[hk] = e
}

func (r *KeyResolver) markUsed(id string) {
	r.mu.Lock()
	if r.used == nil {
		r.used = map[string]bool{}
	}
	r.used[id] = true
	r.mu.Unlock()
}

// FlushUsage records last_used_at for keys seen since the last flush.
func (r *KeyResolver) FlushUsage(ctx context.Context) error {
	r.mu.Lock()
	ids := make([]string, 0, len(r.used))
	for id := range r.used {
		ids = append(ids, id)
	}
	r.used = nil
	r.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	_, err := r.DB.ExecContext(ctx, `SELECT control.touch_keys($1)`, pq.Array(ids))
	return err
}

// RunUsageFlusher calls FlushUsage every interval until ctx ends.
func (r *KeyResolver) RunUsageFlusher(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.FlushUsage(ctx); err != nil && ctx.Err() == nil {
				r.logger().Warn("recording key usage failed", "err", err)
			}
		}
	}
}

func (r *KeyResolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *KeyResolver) ttl() time.Duration      { return orDefault(r.TTL, 60*time.Second) }
func (r *KeyResolver) negTTL() time.Duration   { return orDefault(r.NegativeTTL, 10*time.Second) }
func (r *KeyResolver) staleTTL() time.Duration { return orDefault(r.StaleTTL, 10*time.Minute) }

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (r *KeyResolver) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// singleflight collapses concurrent lookups of the same key into one query.
type singleflight struct {
	mu    sync.Mutex
	calls map[string]*call
}

type call struct {
	wg  sync.WaitGroup
	val entry
	err error
}

func (g *singleflight) do(key string, fn func() (entry, error)) (entry, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = map[string]*call{}
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &call{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()
	c.val, c.err = fn()
	c.wg.Done()
	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	return c.val, c.err
}
