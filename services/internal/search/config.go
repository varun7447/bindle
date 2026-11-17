package search

import (
	"errors"
	"net/http"
	"net/url"
	"time"
)

// FromEnv builds a client from BINDLE_SEARCH_URL and the given user and
// password. Plain http:// is refused unless BINDLE_SEARCH_ALLOW_HTTP=1
// (local docker-compose only), so credentials never cross a network
// unencrypted by accident.
func FromEnv(getenv func(string) string, user, password string) (*Client, error) {
	raw := getenv("BINDLE_SEARCH_URL")
	if raw == "" {
		return nil, errors.New("BINDLE_SEARCH_URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("BINDLE_SEARCH_URL must be an http(s) URL")
	}
	if u.User != nil {
		return nil, errors.New("BINDLE_SEARCH_URL must not contain credentials; use the user/password variables")
	}
	if u.Scheme == "http" && getenv("BINDLE_SEARCH_ALLOW_HTTP") != "1" {
		return nil, errors.New("BINDLE_SEARCH_URL uses plain http; set BINDLE_SEARCH_ALLOW_HTTP=1 only for a local stack")
	}
	if user == "" || password == "" {
		return nil, errors.New("search user and password are required")
	}
	return &Client{URL: raw, User: user, Password: password, HTTP: &http.Client{Timeout: 30 * time.Second}}, nil
}
