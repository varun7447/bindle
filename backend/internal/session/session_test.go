package session

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/control"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaultIsFailClosed(t *testing.T) {
	res, err := FromEnv(env(nil), "127.0.0.1:8080", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/runs?tenant=acme", nil)
	r.Header.Set("Authorization", "Bearer anything")
	if p, err := res.Resolve(r); !errors.Is(err, ErrUnauthenticated) || p.TenantID != "" {
		t.Fatalf("fail-closed resolver admitted request: %+v %v", p, err)
	}
}

func TestBypassLoopbackOnly(t *testing.T) {
	e := env(map[string]string{EnvBypassTenant: "dev-tenant"})
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		res, err := FromEnv(e, addr, nil)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		if p, err := res.Resolve(httptest.NewRequest("GET", "/api/runs?tenant=other", nil)); err != nil || p.TenantID != "dev-tenant" || !p.CanManageKeys() {
			t.Fatalf("%s: got %+v %v", addr, p, err)
		}
	}
	for _, addr := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "10.0.0.5:8080", "garbage"} {
		if _, err := FromEnv(e, addr, nil); err == nil {
			t.Errorf("%s: bypass must be refused on non-loopback listener", addr)
		}
	}
	if _, err := FromEnv(env(map[string]string{EnvBypassTenant: "t", EnvBypassInContainer: "true"}), ":8080", nil); err == nil {
		t.Fatal("only the exact value 1 opts in")
	}
	if _, err := FromEnv(env(map[string]string{EnvBypassTenant: "t", EnvBypassInContainer: "1"}), ":8080", nil); err != nil {
		t.Fatal(err)
	}
}

func TestFromEnvCognitoConfig(t *testing.T) {
	dir := &fakeDir{}
	ok := map[string]string{EnvCognitoIssuer: "https://cognito-idp.us-west-2.amazonaws.com/pool", EnvCognitoClientID: "client"}
	if res, err := FromEnv(env(ok), ":8080", dir); err != nil {
		t.Fatal(err)
	} else if _, isCognito := res.(*Cognito); !isCognito {
		t.Fatalf("got %T", res)
	}
	for _, bad := range []map[string]string{
		{EnvCognitoIssuer: "https://x"},
		{EnvCognitoIssuer: "http://insecure", EnvCognitoClientID: "c"},
		{EnvCognitoIssuer: "https://x", EnvCognitoClientID: "c", EnvBypassTenant: "t"},
	} {
		if _, err := FromEnv(env(bad), "127.0.0.1:1", dir); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if _, err := FromEnv(env(ok), ":8080", nil); err == nil {
		t.Error("Cognito without a directory accepted")
	}
}

// ---- JWT ----------------------------------------------------------------

type testIdP struct {
	key    *rsa.PrivateKey
	kid    string
	server *httptest.Server
	hits   atomic.Int32
}

func newIdP(t *testing.T) *testIdP {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &testIdP{key: k, kid: "kid-1"}
	idp.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		idp.hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": idp.kid, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
		}}})
	}))
	t.Cleanup(idp.server.Close)
	return idp
}

func (p *testIdP) verifier(now time.Time) *Verifier {
	return &Verifier{Issuer: "https://issuer.example/pool", ClientID: "client-1", JWKSURL: p.server.URL, Now: func() time.Time { return now }}
}

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (p *testIdP) sign(t *testing.T, hdr, claims map[string]any) string {
	t.Helper()
	signing := b64(hdr) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func goodClaims(now time.Time) map[string]any {
	return map[string]any{"sub": "user-sub-1", "iss": "https://issuer.example/pool", "client_id": "client-1",
		"token_use": "access", "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "username": "u"}
}

func with(m map[string]any, k string, v any) map[string]any {
	out := map[string]any{}
	for kk, vv := range m {
		out[kk] = vv
	}
	if v == nil {
		delete(out, k)
	} else {
		out[k] = v
	}
	return out
}

func TestVerifierAcceptsOnlyValidTokens(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	idp := newIdP(t)
	v := idp.verifier(now)
	hdr := map[string]any{"alg": "RS256", "kid": "kid-1", "typ": "JWT"}
	good := idp.sign(t, hdr, goodClaims(now))
	c, err := v.Verify(context.Background(), good)
	if err != nil || c.Subject != "user-sub-1" {
		t.Fatalf("valid token rejected: %v", err)
	}

	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := (&testIdP{key: other}).sign(t, hdr, goodClaims(now))
	parts := strings.Split(good, ".")
	tampered := parts[0] + "." + b64(with(goodClaims(now), "sub", "someone-else")) + "." + parts[2]
	none := b64(map[string]any{"alg": "none", "kid": "kid-1"}) + "." + b64(goodClaims(now)) + "."
	hs := b64(map[string]any{"alg": "HS256", "kid": "kid-1"}) + "." + b64(goodClaims(now)) + "." + parts[2]

	bad := map[string]string{
		"forged signature": forged,
		"tampered claims":  tampered,
		"alg none":         none,
		"alg HS256":        hs,
		"unknown kid":      idp.sign(t, with(hdr, "kid", "kid-2"), goodClaims(now)),
		"wrong issuer":     idp.sign(t, hdr, with(goodClaims(now), "iss", "https://evil.example/pool")),
		"wrong client":     idp.sign(t, hdr, with(goodClaims(now), "client_id", "other-client")),
		"id token":         idp.sign(t, hdr, with(goodClaims(now), "token_use", "id")),
		"expired":          idp.sign(t, hdr, with(goodClaims(now), "exp", now.Add(-2*time.Minute).Unix())),
		"no exp":           idp.sign(t, hdr, with(goodClaims(now), "exp", nil)),
		"issued in future": idp.sign(t, hdr, with(goodClaims(now), "iat", now.Add(time.Hour).Unix())),
		"no subject":       idp.sign(t, hdr, with(goodClaims(now), "sub", "")),
		"garbage":          "not.a.jwt",
		"two parts":        "a.b",
		"oversized":        strings.Repeat("a", maxTokenBytes+1),
	}
	for name, tok := range bad {
		if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: accepted (err=%v)", name, err)
		}
	}
	if within := idp.sign(t, hdr, with(goodClaims(now), "exp", now.Add(-30*time.Second).Unix())); true {
		if _, err := v.Verify(context.Background(), within); err != nil {
			t.Errorf("token within clock-skew leeway rejected: %v", err)
		}
	}
}

func TestVerifierJWKSRefetchIsRateLimited(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	idp := newIdP(t)
	v := idp.verifier(now)
	hdr := map[string]any{"alg": "RS256", "kid": "kid-unknown"}
	tok := idp.sign(t, hdr, goodClaims(now))
	for i := 0; i < 20; i++ {
		_, _ = v.Verify(context.Background(), tok)
	}
	if n := idp.hits.Load(); n > 1 {
		t.Fatalf("unknown kids caused %d JWKS fetches", n)
	}
}

func TestVerifierUnconfiguredRejects(t *testing.T) {
	if _, err := (&Verifier{}).Verify(context.Background(), "a.b.c"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("unconfigured verifier must reject")
	}
}

// ---- Cognito resolver ----------------------------------------------------

type fakeDir struct {
	users, provisions atomic.Int32
}

func (f *fakeDir) EnsureUser(_ context.Context, sub, _ string) (control.User, error) {
	f.users.Add(1)
	return control.User{ID: "usr_" + sub, Sub: sub}, nil
}

func (f *fakeDir) Provision(_ context.Context, u control.User, name string) (control.Membership, error) {
	f.provisions.Add(1)
	return control.Membership{TenantID: "tn_for_" + u.ID, TenantName: name, Role: control.RoleOwner}, nil
}

func TestCognitoResolver(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	idp := newIdP(t)
	dir := &fakeDir{}
	c := &Cognito{Verifier: idp.verifier(now), Directory: dir}
	tok := idp.sign(t, map[string]any{"alg": "RS256", "kid": "kid-1"}, goodClaims(now))

	r := httptest.NewRequest("GET", "/api/runs?tenant=evil", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("X-Bindle-Tenant", "evil")
	p, err := c.Resolve(r)
	if err != nil || p.TenantID != "tn_for_usr_user-sub-1" || p.UserID != "usr_user-sub-1" || !p.CanManageKeys() {
		t.Fatalf("principal %+v %v", p, err)
	}
	for i := 0; i < 5; i++ {
		_, _ = c.Resolve(r)
	}
	if dir.users.Load() != 1 || dir.provisions.Load() != 1 {
		t.Fatalf("cache not used: users=%d provisions=%d", dir.users.Load(), dir.provisions.Load())
	}
	for _, h := range []string{"", "Basic abc", "Bearer ", "Bearer not-a-token"} {
		r := httptest.NewRequest("GET", "/api/runs", nil)
		if h != "" {
			r.Header.Set("Authorization", h)
		}
		if _, err := c.Resolve(r); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("header %q: %v", h, err)
		}
	}
}

func TestDevBypassEmail(t *testing.T) {
	env := map[string]string{EnvBypassTenant: "dev-tenant", EnvBypassEmail: "Owner@Example.com"}
	r, err := FromEnv(func(k string) string { return env[k] }, "127.0.0.1:8080", nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.Resolve(nil)
	if err != nil || p.Email != "Owner@Example.com" || p.UserID != "" {
		t.Fatalf("got %+v, %v", p, err)
	}
	env[EnvBypassEmail] = "not-an-email"
	if _, err := FromEnv(func(k string) string { return env[k] }, "127.0.0.1:8080", nil); err == nil {
		t.Fatal("accepted a malformed bypass email")
	}
	// Without the bypass the email setting does nothing: requests fail closed.
	env = map[string]string{EnvBypassEmail: "owner@example.com"}
	r, err = FromEnv(func(k string) string { return env[k] }, "127.0.0.1:8080", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(nil); err == nil {
		t.Fatal("email without the bypass authenticated a request")
	}
}
