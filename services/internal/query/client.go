// Package query reads tenant data from ClickHouse for the console API.
//
// Every query is a constant SQL string with typed ClickHouse parameters
// ({name:Type}); request values are only ever sent as param_<name> URL
// parameters, never spliced into SQL. Every Store method takes the tenant as
// an explicit argument (resolved from the session by the caller) and refuses
// to run without one.
package query

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ErrNoTenant is returned when a Store method is called without a tenant.
var ErrNoTenant = errors.New("query: tenant is required")

// ErrTooLarge is returned when a response exceeds MaxResponseBytes.
var ErrTooLarge = errors.New("query: response too large")

// Error is a ClickHouse failure. Its message may contain SQL or server
// details, so callers must log it, not return it to clients.
type Error struct {
	Status int
	Code   string // X-ClickHouse-Exception-Code, if present
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("clickhouse: HTTP %d (code %s): %s", e.Status, e.Code, e.Msg)
}

// Client runs read-only queries over the ClickHouse HTTP interface.
type Client struct {
	URL      string // e.g. http://localhost:8123
	User     string
	Password string
	HTTP     *http.Client

	MaxExecutionSeconds int   // per query; default 10
	MaxResultRows       int   // per query; default 10000
	MaxResponseBytes    int64 // default 64 MiB
}

// selectRows runs sql with params and decodes each JSONEachRow line into a
// new T. sql must be a constant; params values are bound by ClickHouse.
func selectRows[T any](ctx context.Context, c *Client, sql string, params map[string]string) ([]T, error) {
	var out []T
	err := c.stream(ctx, sql, params, func(line []byte) error {
		var v T
		if err := json.Unmarshal(line, &v); err != nil {
			return fmt.Errorf("query: decode row: %w", err)
		}
		out = append(out, v)
		return nil
	})
	return out, err
}

func (c *Client) stream(ctx context.Context, sql string, params map[string]string, fn func([]byte) error) error {
	maxExec := c.MaxExecutionSeconds
	if maxExec <= 0 {
		maxExec = 10
	}
	maxRows := c.MaxResultRows
	if maxRows <= 0 {
		maxRows = 10000
	}
	maxBytes := c.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}

	q := url.Values{}
	q.Set("readonly", "1") // reject anything but reads, and setting changes in SQL
	q.Set("max_execution_time", strconv.Itoa(maxExec))
	q.Set("max_result_rows", strconv.Itoa(maxRows))
	q.Set("result_overflow_mode", "throw")
	q.Set("wait_end_of_query", "1") // errors arrive as HTTP status, not mid-stream
	q.Set("output_format_json_quote_64bit_integers", "0")
	q.Set("date_time_output_format", "iso")
	for k, v := range params {
		q.Set("param_"+k, v)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/?"+q.Encode(),
		bytes.NewBufferString(sql+"\nFORMAT JSONEachRow"))
	if err != nil {
		return err
	}
	if c.User != "" {
		req.Header.Set("X-ClickHouse-User", c.User)
		req.Header.Set("X-ClickHouse-Key", c.Password)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: time.Duration(maxExec+5) * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &Error{Status: resp.StatusCode, Code: resp.Header.Get("X-ClickHouse-Exception-Code"), Msg: string(bytes.TrimSpace(msg))}
	}

	// Read one byte past the cap so an oversized response is an error rather
	// than a silently truncated result.
	lr := &io.LimitedReader{R: resp.Body, N: maxBytes + 1}
	sc := bufio.NewScanner(lr)
	sc.Buffer(make([]byte, 0, 64<<10), int(maxBytes)+1)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte(`{"exception"`)) {
			return &Error{Status: resp.StatusCode, Msg: string(line)}
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if lr.N <= 0 {
		return ErrTooLarge
	}
	return nil
}
