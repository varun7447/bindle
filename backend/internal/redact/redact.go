// Package redact is Bindle's server-side second-pass redaction. The SDK and
// tenant-side collector redact first; this catches anything that slipped
// through before data is stored.
package redact

import (
	"regexp"
	"strings"
)

type rule struct {
	label string
	re    *regexp.Regexp
	check func(string) bool // optional extra validation (e.g. Luhn)
}

// Redactor replaces sensitive substrings with [REDACTED:<LABEL>] markers.
type Redactor struct {
	rules []rule
}

// Default returns the default policy: secrets first, then PII.
func Default() *Redactor {
	return &Redactor{rules: []rule{
		{label: "PRIVATE_KEY", re: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)},
		{label: "SECRET", re: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
		{label: "SECRET", re: regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}`)},
		{label: "SECRET", re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)},
		{label: "SECRET", re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{10,}`)},
		{label: "SECRET", re: regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/\-]{20,}=*`)},
		{label: "CARD", re: regexp.MustCompile(`\b(?:\d[ \-]?){12,18}\d\b`), check: luhnValid},
		{label: "SSN", re: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
		{label: "EMAIL", re: regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)},
		{label: "PHONE", re: regexp.MustCompile(`(?:\+?1[\-. ]?)?\(?\b\d{3}\)?[\-. ]?\d{3}[\-. ]?\d{4}\b`)},
	}}
}

// Apply returns s with every match replaced.
func (r *Redactor) Apply(s string) string {
	if r == nil || s == "" {
		return s
	}
	for _, ru := range r.rules {
		marker := "[REDACTED:" + ru.label + "]"
		if ru.check == nil {
			s = ru.re.ReplaceAllString(s, marker)
			continue
		}
		s = ru.re.ReplaceAllStringFunc(s, func(m string) string {
			if ru.check(m) {
				return marker
			}
			return m
		})
	}
	return s
}

func luhnValid(s string) bool {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}
