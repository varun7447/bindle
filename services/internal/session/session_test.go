package session

import (
	"errors"
	"net/http/httptest"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaultIsFailClosed(t *testing.T) {
	res, err := FromEnv(env(nil), "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/runs?tenant=acme", nil)
	r.Header.Set("Authorization", "Bearer anything")
	r.Header.Set("x-bindle-tenant", "acme")
	if tenant, err := res.Tenant(r); !errors.Is(err, ErrUnauthenticated) || tenant != "" {
		t.Fatalf("fail-closed resolver admitted request: %q %v", tenant, err)
	}
}

func TestBypassLoopbackOnly(t *testing.T) {
	e := env(map[string]string{EnvBypassTenant: "dev-tenant"})
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		res, err := FromEnv(e, addr)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		if tenant, err := res.Tenant(httptest.NewRequest("GET", "/api/runs?tenant=other", nil)); err != nil || tenant != "dev-tenant" {
			t.Fatalf("%s: got %q %v", addr, tenant, err)
		}
	}
	for _, addr := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "10.0.0.5:8080", "garbage"} {
		if _, err := FromEnv(e, addr); err == nil {
			t.Errorf("%s: bypass must be refused on non-loopback listener", addr)
		}
	}
}

func TestBypassInContainerNeedsExplicitOptIn(t *testing.T) {
	e := env(map[string]string{EnvBypassTenant: "dev-tenant", EnvBypassInContainer: "true"})
	if _, err := FromEnv(e, ":8080"); err == nil {
		t.Fatal("only the exact value 1 opts in")
	}
	e = env(map[string]string{EnvBypassTenant: "dev-tenant", EnvBypassInContainer: "1"})
	if _, err := FromEnv(e, ":8080"); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyBypassTenantRejects(t *testing.T) {
	if _, err := (DevBypass{}).Tenant(httptest.NewRequest("GET", "/", nil)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("empty bypass tenant must reject")
	}
}
