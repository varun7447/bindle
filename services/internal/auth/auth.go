// Package auth resolves a tenant from an ingest API key. The tenant is never
// taken from the telemetry payload itself.
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// Keys maps sha256(api key) -> tenant ID. Only hashes are kept in memory.
type Keys struct {
	byHash map[string]string
}

// ParseEnv parses "key1=tenantA,key2=tenantB". In production keys come from
// Secrets Manager or the control-plane database; this format is for dev.
func ParseEnv(s string) (*Keys, error) {
	k := &Keys{byHash: map[string]string{}}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, tenant, ok := strings.Cut(pair, "=")
		if !ok || key == "" || tenant == "" {
			return nil, fmt.Errorf("auth: bad key entry %q (want key=tenant)", pair)
		}
		k.Add(key, tenant)
	}
	return k, nil
}

// Add registers a key.
func (k *Keys) Add(key, tenant string) {
	k.byHash[hash(key)] = tenant
}

// Tenant returns the tenant for the request's key, read from
// "Authorization: Bearer <key>" or "x-bindle-key: <key>".
func (k *Keys) Tenant(r *http.Request) (string, bool) {
	key := r.Header.Get("x-bindle-key")
	if key == "" {
		h := r.Header.Get("Authorization")
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			key = strings.TrimSpace(h[7:])
		}
	}
	if key == "" {
		return "", false
	}
	t, ok := k.byHash[hash(key)]
	return t, ok
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
