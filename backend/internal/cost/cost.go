// Package cost computes per-step LLM cost from a server-side price table, so
// customers never redeploy when model prices change.
//
// Prices are never guessed. A model that isn't in the table, or a step that
// uses a price the provider doesn't publish (for example cached input on a
// model without caching), is reported as cost unknown.
package cost

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
)

// Price is USD per million tokens for one model.
type Price struct {
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
	// nil when the provider publishes no cached-input price.
	CachedInputPerMTok *float64 `json:"cached_input_per_mtok,omitempty"`

	// Prompts with more input tokens than LongAbove use the Long* prices
	// (0 = no long-prompt tier). A nil Long* price means "not published".
	LongAbove              int64    `json:"long_above_input_tokens,omitempty"`
	LongInputPerMTok       *float64 `json:"long_input_per_mtok,omitempty"`
	LongOutputPerMTok      *float64 `json:"long_output_per_mtok,omitempty"`
	LongCachedInputPerMTok *float64 `json:"long_cached_input_per_mtok,omitempty"`

	// Provenance, for people maintaining the table (not used in math).
	Source string `json:"source,omitempty"`
	AsOf   string `json:"as_of,omitempty"`
}

// F is a helper for literal prices in Go code and tests.
func F(v float64) *float64 { return &v }

// Table maps "provider/model" or "model" to a Price.
type Table struct {
	prices map[string]Price
}

// Load reads a JSON price table. An empty path returns an empty table
// (cost is then reported as unknown rather than guessed). Keys starting
// with "_" are comments.
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
		if strings.HasPrefix(k, "_") {
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

// Len is the number of priced models.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.prices)
}

// A key may match a longer model name only through a version or date
// suffix ("claude-opus-4-5-20251101", "gpt-4o-2024-08-06", "...@20250514",
// "...-v1:0", "...-latest"), never through a different model's name:
// "gpt-5" must not price "gpt-5-mini", and "claude-fable-5" must not price
// a future "claude-fable-5-3".
var versionSuffix = regexp.MustCompile(`^(?:[-@](?:\d{4}-?\d{2}-?\d{2}|\d{8}|v\d+(?::\d+)?|latest|preview)\b)`)

// Lookup finds the price for provider/model: "provider/model" first, then a
// provider-less "model" key, then the longest key that the model extends
// with only a version or date suffix.
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
		if len(key) > bestLen && strings.HasPrefix(model, key) && versionSuffix.MatchString(model[len(key):]) {
			best, bestLen = p, len(key)
		}
	}
	return best, bestLen > 0
}

// Compute returns the USD cost of one step and whether it could be priced.
// in counts all input tokens (cached included); cached is the cached part.
func (t *Table) Compute(provider, model string, in, out, cached int64) (float64, bool) {
	p, ok := t.Lookup(provider, model)
	if !ok {
		return 0, false
	}
	inPrice, outPrice, cachedPrice := &p.InputPerMTok, &p.OutputPerMTok, p.CachedInputPerMTok
	if p.LongAbove > 0 && in > p.LongAbove {
		inPrice, outPrice, cachedPrice = p.LongInputPerMTok, p.LongOutputPerMTok, p.LongCachedInputPerMTok
		if inPrice == nil || outPrice == nil {
			return 0, false
		}
	}
	if cached < 0 {
		cached = 0
	}
	if cached > in {
		cached = in
	}
	if cached > 0 && cachedPrice == nil {
		return 0, false // no published cached price: don't guess
	}
	c := float64(in-cached)**inPrice/1e6 + float64(out)**outPrice/1e6
	if cached > 0 {
		c += float64(cached) * *cachedPrice / 1e6
	}
	return c, true
}
