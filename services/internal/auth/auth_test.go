package auth

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseEnv(t *testing.T) {
	k, err := ParseEnv(" bd_a=tenantA , bd_b=tenantB,")
	if err != nil {
		t.Fatal(err)
	}
	if len(k.byHash) != 2 {
		t.Fatalf("want 2 keys, got %d", len(k.byHash))
	}
	for h := range k.byHash {
		if strings.Contains(h, "bd_") {
			t.Fatalf("raw key stored: %q", h)
		}
	}
	for _, bad := range []string{"novalue", "=t", "k=", "a=b,broken"} {
		if _, err := ParseEnv(bad); err == nil {
			t.Errorf("ParseEnv(%q): want error", bad)
		}
	}
	if k, err := ParseEnv(""); err != nil || len(k.byHash) != 0 {
		t.Fatalf("empty env: %v %v", k, err)
	}
}

func TestTenant(t *testing.T) {
	k, _ := ParseEnv("bd_a=tenantA")
	cases := []struct {
		name    string
		headers map[string]string
		want    string
		ok      bool
	}{
		{"bearer", map[string]string{"Authorization": "Bearer bd_a"}, "tenantA", true},
		{"bearer case-insensitive", map[string]string{"Authorization": "bearer  bd_a "}, "tenantA", true},
		{"x-bindle-key", map[string]string{"x-bindle-key": "bd_a"}, "tenantA", true},
		{"x-bindle-key wins", map[string]string{"x-bindle-key": "bd_a", "Authorization": "Bearer nope"}, "tenantA", true},
		{"unknown key", map[string]string{"Authorization": "Bearer bd_x"}, "", false},
		{"basic scheme", map[string]string{"Authorization": "Basic bd_a"}, "", false},
		{"no header", nil, "", false},
		{"bearer only", map[string]string{"Authorization": "Bearer "}, "", false},
		// Tenant hints in the request must never be honoured.
		{"tenant header ignored", map[string]string{"x-bindle-tenant": "tenantA"}, "", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/v1/traces?tenant=tenantA", nil)
		for h, v := range c.headers {
			r.Header.Set(h, v)
		}
		got, ok := k.Tenant(r)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got (%q,%v) want (%q,%v)", c.name, got, ok, c.want, c.ok)
		}
	}
}
