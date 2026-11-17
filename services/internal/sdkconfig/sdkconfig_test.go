package sdkconfig

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := Config{SampleRate: 0.5, CaptureContent: true, RedactPatterns: []Pattern{
		{Name: "ORDER_ID", Pattern: `\bord_[0-9a-f]{8}\b`},
		{Name: "ACCT", Pattern: `acct-\d+`},
	}}
	if err := Validate(ok); err != nil {
		t.Fatal(err)
	}
	if err := Validate(Default()); err != nil {
		t.Fatal(err)
	}
	many := make([]Pattern, MaxPatterns+1)
	for i := range many {
		many[i] = Pattern{Name: "P" + strings.Repeat("X", i%5) + string(rune('A'+i)), Pattern: `x\d`}
	}
	bad := map[string]Config{
		"rate > 1":       {SampleRate: 1.5},
		"rate < 0":       {SampleRate: -0.1},
		"too many":       {SampleRate: 1, RedactPatterns: many},
		"lowercase name": {SampleRate: 1, RedactPatterns: []Pattern{{Name: "email", Pattern: `x`}}},
		"dup name":       {SampleRate: 1, RedactPatterns: []Pattern{{Name: "A", Pattern: `x`}, {Name: "A", Pattern: `y`}}},
		"bad regex":      {SampleRate: 1, RedactPatterns: []Pattern{{Name: "A", Pattern: `(`}}},
	}
	for name, c := range bad {
		if err := Validate(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidatePatternRejectsDangerousForms(t *testing.T) {
	for _, p := range []string{`(a+)+`, `(a*)*b`, `(\w+\s?)+$`, `(?:x{2,})+`, `((ab)*c)*`, ``, `a*`, `x?`, strings.Repeat("a", MaxPatternLength+1), `(\d)(\d)(\d)(\d)(\d)(\d)(\d)(\d)(\d)(\d)`, `\1`, `(?=x)y`} {
		if err := ValidatePattern(p); err == nil {
			t.Errorf("accepted %q", p)
		}
	}
	for _, p := range []string{`\bord_[0-9a-f]{8}\b`, `(?i)secret-\w+`, `(foo|bar)-\d+`, `x(y)?z+`, `[A-Z]{2}\d{6}`} {
		if err := ValidatePattern(p); err != nil {
			t.Errorf("rejected %q: %v", p, err)
		}
	}
}

func TestRedactor(t *testing.T) {
	r := NewRedactor([]Pattern{{Name: "ORDER", Pattern: `ord_\d+`}, {Name: "BAD", Pattern: `(a+)+`}})
	if got := r.Apply("refund ord_123 and ord_456"); got != "refund [REDACTED:ORDER] and [REDACTED:ORDER]" {
		t.Errorf("got %q", got)
	}
	if got := r.Apply("aaaa"); got != "aaaa" {
		t.Error("invalid pattern must be skipped")
	}
	if !NewRedactor(nil).Empty() || r.Empty() {
		t.Error("Empty")
	}
	// Replacement is literal: $1 in a marker can't expand.
	if got := NewRedactor([]Pattern{{Name: "X", Pattern: `(x)`}}).Apply("x"); got != "[REDACTED:X]" {
		t.Errorf("got %q", got)
	}
}

func TestSampledMatchesOpenTelemetryRule(t *testing.T) {
	// Low 64 bits: 0x0000000000000000 always kept (rate > 0), 0xffff... dropped below 1.
	lowZero := "ffffffffffffffff0000000000000000"
	lowMax := "0000000000000000ffffffffffffffff"
	half := "00000000000000007fffffffffffffff"  // just below 2^63
	above := "00000000000000008000000000000000" // exactly 2^63
	cases := []struct {
		id   string
		rate float64
		want bool
	}{
		{lowZero, 0.0001, true}, {lowZero, 0, false}, {lowMax, 0.999, false}, {lowMax, 1, true},
		{half, 0.5, true}, {above, 0.5, false}, {"short", 0.1, true},
	}
	for _, c := range cases {
		if got := Sampled(c.id, c.rate); got != c.want {
			t.Errorf("Sampled(%s, %v) = %v", c.id, c.rate, got)
		}
	}
	// Roughly rate of random-looking IDs are kept.
	kept := 0
	for i := 0; i < 10000; i++ {
		id := strings.Repeat("0", 16) + hex16(uint64(i)*0x9E3779B97F4A7C15)
		if Sampled(id, 0.25) {
			kept++
		}
	}
	if kept < 2300 || kept > 2700 {
		t.Errorf("kept %d of 10000 at 25%%", kept)
	}
}

func hex16(v uint64) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		b[i] = digits[v&0xf]
		v >>= 4
	}
	return string(b)
}
