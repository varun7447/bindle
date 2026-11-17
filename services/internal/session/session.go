// Package session resolves who is calling the console API: the user, their
// tenant and role.
//
// In production the console signs people in with Amazon Cognito and sends
// the Cognito access token as a Bearer token; Cognito verifies it and the
// control plane maps the user to their tenant. Without Cognito configured,
// the API is fail-closed unless a developer explicitly opts into DevBypass
// for a local, loopback-only server.
package session

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bindle-ai/bindle/services/internal/control"
)

// ErrUnauthenticated means the request has no valid session.
var ErrUnauthenticated = errors.New("session: unauthenticated")

// Principal is the verified caller. TenantID comes only from verified
// credentials and the control plane, never from request parameters.
type Principal struct {
	TenantID   string
	TenantName string
	UserID     string // "" for the dev bypass
	Role       string
}

// CanManageKeys reports whether the caller may create, rotate or revoke keys.
func (p Principal) CanManageKeys() bool {
	return p.Role == control.RoleOwner || p.Role == control.RoleAdmin
}

// Resolver identifies the caller of a request.
type Resolver interface {
	Resolve(r *http.Request) (Principal, error)
}

// FailClosed rejects every request. Used when no verifier is configured.
type FailClosed struct{}

// Resolve implements Resolver.
func (FailClosed) Resolve(*http.Request) (Principal, error) { return Principal{}, ErrUnauthenticated }

// DevBypass maps every request to one tenant as its owner. Local
// development only: FromEnv refuses it on a non-loopback listener unless
// running in a container whose port is published to loopback only.
type DevBypass struct{ TenantID string }

// Resolve implements Resolver.
func (d DevBypass) Resolve(*http.Request) (Principal, error) {
	if d.TenantID == "" {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{TenantID: d.TenantID, TenantName: "Local dev", Role: control.RoleOwner}, nil
}

// Directory is the part of the control plane the Cognito resolver needs.
// Implemented by *control.Store.
type Directory interface {
	EnsureUser(ctx context.Context, sub, email string) (control.User, error)
	Provision(ctx context.Context, u control.User, tenantName string) (control.Membership, error)
}

// Cognito resolves Bearer access tokens. A user's first verified request
// creates their user record and their tenant (they become its owner).
type Cognito struct {
	Verifier  *Verifier
	Directory Directory
	CacheTTL  time.Duration // principal cache per subject; default 60s

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	p  Principal
	at time.Time
}

// DefaultTenantName names a tenant created on first sign-in.
const DefaultTenantName = "My workspace"

// Resolve implements Resolver.
func (c *Cognito) Resolve(r *http.Request) (Principal, error) {
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "bearer ") {
		return Principal{}, ErrUnauthenticated
	}
	claims, err := c.Verifier.Verify(r.Context(), strings.TrimSpace(h[7:]))
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	now := c.Verifier.now()
	ttl := c.CacheTTL
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	c.mu.Lock()
	e, ok := c.cache[claims.Subject]
	c.mu.Unlock()
	if ok && now.Sub(e.at) < ttl {
		return e.p, nil
	}
	u, err := c.Directory.EnsureUser(r.Context(), claims.Subject, "")
	if err != nil {
		return Principal{}, fmt.Errorf("session: user: %w", err)
	}
	m, err := c.Directory.Provision(r.Context(), u, DefaultTenantName)
	if err != nil {
		return Principal{}, fmt.Errorf("session: tenant: %w", err)
	}
	p := Principal{TenantID: m.TenantID, TenantName: m.TenantName, UserID: u.ID, Role: m.Role}
	c.mu.Lock()
	if c.cache == nil || len(c.cache) > 50_000 {
		c.cache = map[string]cached{}
	}
	c.cache[claims.Subject] = cached{p: p, at: now}
	c.mu.Unlock()
	return p, nil
}

// Env var names. Both bypass switches are opt-in and default to deny.
const (
	EnvBypassTenant      = "BINDLE_API_DEV_AUTH_BYPASS_TENANT"
	EnvBypassInContainer = "BINDLE_API_DEV_AUTH_BYPASS_IN_CONTAINER"
	EnvCognitoIssuer     = "BINDLE_COGNITO_ISSUER"
	EnvCognitoClientID   = "BINDLE_COGNITO_CLIENT_ID"
)

// FromEnv picks the resolver. dir may be nil when Cognito is not configured.
func FromEnv(getenv func(string) string, listenAddr string, dir Directory) (Resolver, error) {
	issuer, client := getenv(EnvCognitoIssuer), getenv(EnvCognitoClientID)
	tenant := getenv(EnvBypassTenant)
	if issuer != "" || client != "" {
		if tenant != "" {
			return nil, fmt.Errorf("session: %s and Cognito are both configured; use one", EnvBypassTenant)
		}
		if issuer == "" || client == "" {
			return nil, fmt.Errorf("session: set both %s and %s", EnvCognitoIssuer, EnvCognitoClientID)
		}
		if !strings.HasPrefix(issuer, "https://") {
			return nil, fmt.Errorf("session: %s must be an https URL", EnvCognitoIssuer)
		}
		if dir == nil {
			return nil, errors.New("session: Cognito needs the control-plane database")
		}
		return &Cognito{Verifier: &Verifier{Issuer: strings.TrimRight(issuer, "/"), ClientID: client}, Directory: dir}, nil
	}
	if tenant == "" {
		return FailClosed{}, nil
	}
	if !IsLoopback(listenAddr) && getenv(EnvBypassInContainer) != "1" {
		return nil, fmt.Errorf("session: %s is set but the API listens on %q; the dev auth bypass "+
			"is allowed only on a loopback address (or in a container published to 127.0.0.1 "+
			"with %s=1)", EnvBypassTenant, listenAddr, EnvBypassInContainer)
	}
	return DevBypass{TenantID: tenant}, nil
}

// IsLoopback reports whether a host:port listen address binds loopback only.
// An empty host (":8080") binds all interfaces and is not loopback.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
