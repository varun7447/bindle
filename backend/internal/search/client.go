// Package search stores and queries Bindle traces in OpenSearch over its
// REST API (net/http only, no client library).
//
// Layout (Prefix defaults to "bindle"):
//   - <prefix>-steps-YYYY.MM.DD: one document per step, daily indices,
//     deleted by an ISM policy after the retention period.
//   - <prefix>-traces: one rollup document per trace for the runs list
//     and overview, recomputed from the steps after each write.
//
// Every document carries tenant_id and is routed by it. Every read goes
// through Store.search, which adds the tenant filter itself, so no query
// can omit it.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one OpenSearch endpoint with HTTP basic auth (the
// security plugin's internal users, or Amazon OpenSearch Service
// fine-grained access control).
type Client struct {
	URL      string // e.g. http://opensearch:9200 or https://vpc-...es.amazonaws.com
	User     string
	Password string
	HTTP     *http.Client
	// MaxResponseBytes caps response bodies; default 64 MiB.
	MaxResponseBytes int64
}

// Error is a non-2xx OpenSearch response. Its text can contain index
// names and query details, so log it; don't return it to API clients.
type Error struct {
	Status int
	Type   string
	Reason string
}

func (e *Error) Error() string {
	return fmt.Sprintf("opensearch: HTTP %d %s: %s", e.Status, e.Type, e.Reason)
}

// IsStatus reports whether err is an OpenSearch error with that status.
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

// ErrTooLarge is returned when a response exceeds MaxResponseBytes.
var ErrTooLarge = errors.New("opensearch: response too large")

// Do sends one request. body may be nil, []byte (sent as-is, for NDJSON)
// or any JSON-encodable value. out, if non-nil, receives the decoded body
// (numbers as json.Number to keep 64-bit integers exact).
func (c *Client) Do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	var rdr io.Reader
	contentType := "application/json"
	switch b := body.(type) {
	case nil:
	case []byte:
		rdr = bytes.NewReader(b)
		contentType = "application/x-ndjson"
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	u := strings.TrimRight(c.URL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	if rdr != nil {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Password)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	limit := c.MaxResponseBytes
	if limit <= 0 {
		limit = 64 << 20
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return ErrTooLarge
	}
	if resp.StatusCode/100 != 2 {
		e := &Error{Status: resp.StatusCode}
		var eb struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(data, &eb) == nil && len(eb.Error) > 0 {
			var obj struct{ Type, Reason string }
			if json.Unmarshal(eb.Error, &obj) == nil && obj.Type != "" {
				e.Type, e.Reason = obj.Type, obj.Reason
			} else {
				e.Reason = strings.Trim(string(eb.Error), `"`)
			}
		} else {
			e.Reason = truncate(string(data), 512)
		}
		return e
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
