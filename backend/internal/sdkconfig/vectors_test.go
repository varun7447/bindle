package sdkconfig

import (
	"encoding/json"
	"os"
	"testing"
)

// Shared with the Python SDK's tests (sdks/python/tests/sampling_vectors.json)
// so both implementations provably keep the same traces. Regenerate with
// BINDLE_WRITE_VECTORS=1 go test ./internal/sdkconfig -run Vectors.
func TestSamplingVectors(t *testing.T) {
	ids := []string{
		"0af7651916cd43dd8448eb211c80319c", "4bf92f3577b34da6a3ce929d0e0e4736", "00000000000000000000000000000001",
		"0000000000000000ffffffffffffffff", "00000000000000007fffffffffffffff", "00000000000000008000000000000000",
		"5b8efff798038103d269b633813fc60c", "ee5d1e8d0c4f4e2c9a3b1f7d2e6c8a90", "1234567890abcdef1234567890abcdef",
		"ffffffffffffffff3333333333333333",
	}
	rates := []float64{0, 0.001, 0.1, 0.25, 0.5, 0.75, 0.999, 1}
	type vec struct {
		TraceID string  `json:"trace_id"`
		Rate    float64 `json:"rate"`
		Sampled bool    `json:"sampled"`
	}
	var out []vec
	for _, id := range ids {
		for _, r := range rates {
			out = append(out, vec{id, r, Sampled(id, r)})
		}
	}
	b, _ := json.MarshalIndent(out, "", " ")
	path := "../../../sdks/python/tests/sampling_vectors.json"
	if os.Getenv("BINDLE_WRITE_VECTORS") == "1" {
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Skip("vectors file not generated yet")
	}
	if string(existing) != string(b)+"\n" {
		t.Fatal("sampling vectors are stale: regenerate with BINDLE_WRITE_VECTORS=1")
	}
}
