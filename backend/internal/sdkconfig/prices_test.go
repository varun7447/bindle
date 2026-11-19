package sdkconfig

import (
	"math"
	"testing"

	"github.com/bindle-ai/bindle/backend/internal/cost"
)

func TestValidatePrices(t *testing.T) {
	ok := []PriceOverride{{Model: "claude-sonnet-5-5", InputPerMTok: 2.4, OutputPerMTok: 12, CachedInputPerMTok: cost.F(0.24)},
		{Provider: "aws.bedrock", Model: "anthropic.claude-sonnet-5-5-v1:0", InputPerMTok: 3, OutputPerMTok: 15}}
	if err := ValidatePrices(ok); err != nil {
		t.Fatal(err)
	}
	bad := map[string][]PriceOverride{
		"empty model":   {{}},
		"space":         {{Model: "a b"}},
		"provider case": {{Provider: "OpenAI", Model: "gpt"}},
		"duplicate":     {{Model: "Gpt-X"}, {Model: "gpt-x"}},
		"negative":      {{Model: "m", InputPerMTok: -1}},
		"nan":           {{Model: "m", OutputPerMTok: math.NaN()}},
		"huge cached":   {{Model: "m", CachedInputPerMTok: cost.F(1e6)}},
	}
	for name, ps := range bad {
		if ValidatePrices(ps) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPriceTable(t *testing.T) {
	if PriceTable(nil) != nil {
		t.Fatal("no overrides: nil table")
	}
	tb := PriceTable([]PriceOverride{{Model: "gpt-x", InputPerMTok: 1, OutputPerMTok: 2}, {Provider: "anthropic", Model: "claude-y", InputPerMTok: 3, OutputPerMTok: 4}})
	if c, ok := tb.Compute("openai", "gpt-x-2026-01-01", 1_000_000, 1_000_000, 0); !ok || c != 3 {
		t.Errorf("provider-less override with dated model: %v %v", c, ok)
	}
	if _, ok := tb.Compute("other", "claude-y", 1, 1, 0); ok {
		t.Error("provider-scoped override matched another provider")
	}
	if _, ok := tb.Compute("openai", "gpt-x", 10, 1, 5); ok {
		t.Error("cached tokens without a cached price must be unknown")
	}
}
