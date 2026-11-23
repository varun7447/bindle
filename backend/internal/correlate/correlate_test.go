package correlate

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

type vectors struct {
	MaxKeys       int `json:"max_keys"`
	MaxValueBytes int `json:"max_value_bytes"`
	Digests       []struct {
		Key, Value, Digest string
	} `json:"digests"`
	Stored []struct {
		Tenant, Key, Digest, Stored string
	} `json:"stored"`
	ValidKeys     []string `json:"valid_keys"`
	InvalidKeys   []string `json:"invalid_keys"`
	InvalidValues []string `json:"invalid_values"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("../../../sdks/python/tests/correlation_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSharedVectors(t *testing.T) {
	v := loadVectors(t)
	if v.MaxKeys != MaxKeys || v.MaxValueBytes != MaxValueLen {
		t.Fatalf("limits drifted from the SDK vectors: %d/%d", v.MaxKeys, v.MaxValueBytes)
	}
	for _, d := range v.Digests {
		if got := Digest(d.Key, d.Value); got != d.Digest {
			t.Errorf("Digest(%q, %q) = %s, want %s", d.Key, d.Value, got, d.Digest)
		}
	}
	for _, s := range v.Stored {
		if got := Stored(s.Tenant, s.Key, s.Digest); got != s.Stored {
			t.Errorf("Stored(%q) = %s, want %s", s.Tenant, got, s.Stored)
		}
		if !ValidStored(s.Stored) {
			t.Errorf("ValidStored(%s) = false", s.Stored)
		}
	}
	for _, k := range v.ValidKeys {
		if !ValidKey(k) {
			t.Errorf("ValidKey(%q) = false", k)
		}
	}
	for _, k := range v.InvalidKeys {
		if ValidKey(k) {
			t.Errorf("ValidKey(%q) = true", k)
		}
	}
	for _, val := range v.InvalidValues {
		if ValidValue(val) {
			t.Errorf("ValidValue(len %d) = true", len(val))
		}
	}
}

func TestTenantBound(t *testing.T) {
	a, b := FromValue("t_1", "ticket_id", "T-123"), FromValue("t_2", "ticket_id", "T-123")
	if a == b {
		t.Fatal("the same value in two tenants must not hash the same")
	}
	if strings.Contains(a, "T-123") {
		t.Fatal("raw value leaked into the stored form")
	}
}

func TestFromAttributes(t *testing.T) {
	attrs := map[string]any{
		"bindle.correlation.ticket_id": Digest("ticket_id", "T-123"), // from the SDK
		"bindle.correlation.order":     "o_981",                      // raw, set without the SDK
		"bindle.correlation.Bad":       "x",                          // invalid key: dropped
		"bindle.correlation.n":         int64(4),                     // not a string: dropped
		"bindle.correlation.empty":     "",                           // invalid value: dropped
		"gen_ai.tool.name":             "lookup",
	}
	stored, drop := FromAttributes("t_1", attrs)
	want := []string{FromValue("t_1", "order", "o_981"), FromValue("t_1", "ticket_id", "T-123")}
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("stored = %v, want %v", stored, want)
	}
	if len(drop) != 5 {
		t.Fatalf("every correlation attribute must be dropped from storage, got %v", drop)
	}
	for _, d := range drop {
		if !strings.HasPrefix(d, AttrPrefix) {
			t.Fatalf("dropped a non-correlation attribute %q", d)
		}
	}
}

func TestFromAttributesCapsKeys(t *testing.T) {
	attrs := map[string]any{}
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		attrs[AttrPrefix+k] = "v"
	}
	stored, drop := FromAttributes("t", attrs)
	if len(stored) != MaxKeys || len(drop) != 10 {
		t.Fatalf("got %d stored, %d dropped", len(stored), len(drop))
	}
}
