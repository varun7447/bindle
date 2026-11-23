// Package correlate links runs to business events that arrive later, such as
// a support ticket being reopened, without storing the identifiers involved.
//
// An agent records a correlation key during a run (bindle.correlate in the
// SDKs). The SDK sends only a digest of the key and value; a business system
// later reports an event with the raw value, which ingest digests the same
// way. Stored values are tenant-bound hashes of the digest, so the same
// ticket ID in two workspaces never matches across them, and the raw value
// is never stored.
package correlate

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// AttrPrefix marks correlation attributes on spans: bindle.correlation.<key>.
const AttrPrefix = "bindle.correlation."

// Limits shared with the SDKs (testdata/vectors.json).
const (
	MaxKeys     = 8
	MaxValueLen = 256
)

var (
	keyRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	digestRe = regexp.MustCompile(`^v1:[0-9a-f]{64}$`)
	storedRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}:[0-9a-f]{64}$`)
)

// ValidKey reports whether k may be used as a correlation key.
func ValidKey(k string) bool { return keyRe.MatchString(k) }

// ValidValue reports whether v may be used as a correlation value.
func ValidValue(v string) bool { return v != "" && len(v) <= MaxValueLen }

// ValidStored reports whether s has the stored form ("<key>:<64 hex>").
func ValidStored(s string) bool { return storedRe.MatchString(s) }

// Digest is what the SDKs send for a key and value: "v1:" + hex SHA-256 of a
// domain-separated encoding. It is not tenant-bound (the SDK doesn't know the
// tenant); Stored binds it.
func Digest(key, value string) string {
	h := sha256.Sum256([]byte("bindle.correlation.v1\x00" + key + "\x00" + value))
	return "v1:" + hex.EncodeToString(h[:])
}

// Stored is the indexed form: "<key>:" + hex SHA-256 of tenant and digest.
// The key stays readable (it names the business system); the value does not.
func Stored(tenant, key, digest string) string {
	h := sha256.Sum256([]byte(tenant + "\x00" + digest))
	return key + ":" + hex.EncodeToString(h[:])
}

// FromValue is Stored for a raw value (an event from a business system, or a
// span attribute set without the SDK).
func FromValue(tenant, key, value string) string { return Stored(tenant, key, Digest(key, value)) }

// FromAttributes returns the stored correlations carried by a span's
// attributes, sorted, and the attribute names to drop before anything is
// stored. Attribute values may be SDK digests or raw values; invalid keys and
// values are dropped, and at most MaxKeys are kept.
func FromAttributes(tenant string, attrs map[string]any) (stored []string, drop []string) {
	type kv struct{ k, v string }
	var found []kv
	for name, raw := range attrs {
		if !strings.HasPrefix(name, AttrPrefix) {
			continue
		}
		drop = append(drop, name)
		key := strings.TrimPrefix(name, AttrPrefix)
		v, ok := raw.(string)
		if !ok || !ValidKey(key) {
			continue
		}
		if !digestRe.MatchString(v) {
			if !ValidValue(v) {
				continue
			}
			v = Digest(key, v)
		}
		found = append(found, kv{key, v})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].k < found[j].k })
	if len(found) > MaxKeys {
		found = found[:MaxKeys]
	}
	for _, f := range found {
		stored = append(stored, Stored(tenant, f.k, f.v))
	}
	sort.Strings(drop)
	return stored, drop
}
