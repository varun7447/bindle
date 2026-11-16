// Package session resolves the tenant for a console API request.
//
// The real resolver (verifying a session from the chosen auth provider) is
// wired in step 3. Until then the API is fail-closed: every request is
// rejected unless a developer explicitly opts into DevBypass for a local,
// loopback-only server.
package session

import (
	"errors"
	"fmt"
	"net"
	"net/http"
)

// ErrUnauthenticated means the request has no valid session.
var ErrUnauthenticated = errors.New("session: unauthenticated")

// Resolver returns the tenant for an authenticated request. The tenant must
// come only from verified credentials, never from request parameters.
type Resolver interface {
	Tenant(r *http.Request) (string, error)
}

// FailClosed rejects every request. It is the default until a real
// session verifier is configured.
type FailClosed struct{}

// Tenant implements Resolver.
func (FailClosed) Tenant(*http.Request) (string, error) { return "", ErrUnauthenticated }

// DevBypass maps every request to one fixed tenant. Local development only:
// FromEnv refuses it on a non-loopback listener unless running in a
// container whose port is published to loopback only.
type DevBypass struct{ TenantID string }

// Tenant implements Resolver.
func (d DevBypass) Tenant(*http.Request) (string, error) {
	if d.TenantID == "" {
		return "", ErrUnauthenticated
	}
	return d.TenantID, nil
}

// Env var names. Both bypass switches are opt-in and default to deny.
const (
	EnvBypassTenant      = "BINDLE_API_DEV_AUTH_BYPASS_TENANT"
	EnvBypassInContainer = "BINDLE_API_DEV_AUTH_BYPASS_IN_CONTAINER"
)

// FromEnv picks the resolver. listenAddr is the server's listen address.
func FromEnv(getenv func(string) string, listenAddr string) (Resolver, error) {
	tenant := getenv(EnvBypassTenant)
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
