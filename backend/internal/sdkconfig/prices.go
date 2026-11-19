package sdkconfig

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/bindle-ai/bindle/backend/internal/cost"
)

// PriceOverride is a workspace's own price for a model (negotiated
// discount, batch or provisioned pricing). It applies to steps ingested
// after it is saved; costs already stored don't change. Model matching is
// the same as the list price table: an exact provider/model, else the model
// alone, else the model plus a date or version suffix.
type PriceOverride struct {
	Provider           string   `json:"provider"` // optional: "" matches any provider
	Model              string   `json:"model"`
	InputPerMTok       float64  `json:"input_per_mtok"`
	OutputPerMTok      float64  `json:"output_per_mtok"`
	CachedInputPerMTok *float64 `json:"cached_input_per_mtok"` // nil: cached tokens make the cost unknown
	Note               string   `json:"note"`
}

const (
	MaxPriceOverrides = 100
	maxPricePerMTok   = 10_000
	maxNote           = 200
)

var (
	providerRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	modelRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,199}$`)
)

// ValidatePrices checks overrides before they are saved.
func ValidatePrices(ps []PriceOverride) error {
	if len(ps) > MaxPriceOverrides {
		return fmt.Errorf("at most %d model prices", MaxPriceOverrides)
	}
	seen := map[string]bool{}
	for i, p := range ps {
		at := fmt.Sprintf("price %d", i+1)
		if p.Provider != "" && !providerRe.MatchString(p.Provider) {
			return fmt.Errorf("%s: provider must be lowercase letters, digits, '.', '_' or '-' (or empty for any)", at)
		}
		if !modelRe.MatchString(p.Model) {
			return fmt.Errorf("%s: model must be 1-200 characters without spaces", at)
		}
		key := strings.ToLower(p.Provider + "/" + p.Model)
		if seen[key] {
			return fmt.Errorf("%s: %s is listed twice", at, p.Model)
		}
		seen[key] = true
		for _, v := range []struct {
			name string
			v    *float64
		}{{"input", &p.InputPerMTok}, {"output", &p.OutputPerMTok}, {"cached input", p.CachedInputPerMTok}} {
			if v.v != nil && (math.IsNaN(*v.v) || *v.v < 0 || *v.v > maxPricePerMTok) {
				return fmt.Errorf("%s: %s price must be between 0 and %d USD per million tokens", at, v.name, maxPricePerMTok)
			}
		}
		if len(p.Note) > maxNote {
			return fmt.Errorf("%s: note is longer than %d characters", at, maxNote)
		}
	}
	return nil
}

// PriceTable compiles overrides; nil when there are none.
func PriceTable(ps []PriceOverride) *cost.Table {
	if len(ps) == 0 {
		return nil
	}
	m := make(map[string]cost.Price, len(ps))
	for _, p := range ps {
		key := p.Model
		if p.Provider != "" {
			key = p.Provider + "/" + p.Model
		}
		m[key] = cost.Price{InputPerMTok: p.InputPerMTok, OutputPerMTok: p.OutputPerMTok, CachedInputPerMTok: p.CachedInputPerMTok, Source: "workspace"}
	}
	return cost.New(m)
}
