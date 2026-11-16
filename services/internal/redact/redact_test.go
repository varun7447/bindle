package redact

import (
	"strings"
	"testing"
)

func TestDefaultRedaction(t *testing.T) {
	r := Default()
	cases := map[string]string{
		"contact jane.doe@acme.io today":                         "[REDACTED:EMAIL]",
		"ssn 123-45-6789":                                        "[REDACTED:SSN]",
		"call (415) 555-0134":                                    "[REDACTED:PHONE]",
		"card 4111 1111 1111 1111":                               "[REDACTED:CARD]",
		"key AKIAABCDEFGHIJKLMNOP":                               "[REDACTED:SECRET]",
		"token sk-abcdefghijklmnopqrstuvwxyz1":                   "[REDACTED:SECRET]",
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz123456": "[REDACTED:SECRET]",
	}
	for in, want := range cases {
		got := r.Apply(in)
		if !strings.Contains(got, want) {
			t.Errorf("Apply(%q) = %q, want it to contain %q", in, got, want)
		}
	}
}

func TestCardRequiresLuhn(t *testing.T) {
	got := Default().Apply("order 1234 5678 9012 3456")
	if strings.Contains(got, "CARD") {
		t.Fatalf("non-Luhn number redacted as card: %q", got)
	}
}

func TestLeavesNormalTextAlone(t *testing.T) {
	in := "Refund timelines are 5-7 business days per policy section 2."
	if got := Default().Apply(in); got != in {
		t.Fatalf("changed normal text: %q", got)
	}
}
