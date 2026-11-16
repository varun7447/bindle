// Package cost computes per-step LLM cost from a server-side price table, so
// customers never redeploy when model prices change.
package cost

import (
	"encoding/json"
	"os"
	"strings"
)

// Price is USD per million tokens.
type Price struct {
	InputPerMTok       float64 `json:"input_per_mtok"`
	OutputPerMTok      float64 `json:"output_per_mtok"`
	CachedInputPerMTok float64 `json:"cached_input_per_mtok"`
}

// Table maps "provider/model" or "model" (exact or prefix) to a Price.
type Table struct {
	prices map[string]Price
}

// Load reads a JSON price table. An empty path returns an empty table
// (cost is then reported as unknown rather than guessed).
func Load(path string) (*Table, error) {
	t := &Table{prices: map[string]Price{}}
	if path == "" {
		return t, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	for k, v := range raw {
		if strings.HasPrefix(k, "_") { // comments
			continue
		}
		var p Price
		if err := json.Unmarshal(v, &p); err != nil {
			return nil, err
		}
		t.prices[strings.ToLower(k)] = p
	}
	return t, nil
}

// New builds a table from a map (useful in tests).
func New(prices map[string]Price) *Table {
	t := &Table{prices: map[string]Price{}}
	for k, v := range prices {
		t.prices[strings.ToLower(k)] = v
	}
	return t
}

// Lookup finds the best price for provider/model: exact keys first, then the
// longest key that prefixes the model name (handles dated model versions).
func (t *Table) Lookup(provider, model string) (Price, bool) {
	if t == nil || model == "" {
		return Price{}, false
	}
	provider, model = strings.ToLower(provider), strings.ToLower(model)
	if p, ok := t.prices[provider+"/"+model]; ok {
		return p, true
	}
	if p, ok := t.prices[model]; ok {
		return p, true
	}
	best, bestLen := Price{}, 0
	for k, p := range t.prices {
		key := k
		if i := strings.Index(k, "/"); i >= 0 {
			if k[:i] != provider {
				continue
			}
			key = k[i+1:]
		}
		if strings.HasPrefix(model, key) && len(key) > bestLen {
			best, bestLen = p, len(key)
		}
	}
	return best, bestLen > 0
}

// Compute returns the USD cost and whether the model was priced.
func (t *Table) Compute(provider, model string, in, out, cached int64) (float64, bool) {
	p, ok := t.Lookup(provider, model)
	if !ok {
		return 0, false
	}
	uncached := in - cached
	if uncached < 0 {
		uncached = 0
	}
	c := float64(uncached)*p.InputPerMTok/1e6 +
		float64(cached)*p.CachedInputPerMTok/1e6 +
		float64(out)*p.OutputPerMTok/1e6
	return c, true
}
