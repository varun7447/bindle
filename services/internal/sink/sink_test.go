package sink

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bindle-ai/bindle/services/internal/model"
)

func TestClickHouseInsert(t *testing.T) {
	var gotQuery, gotUser string
	var rows []Row
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		gotUser = r.Header.Get("X-ClickHouse-User")
		b, _ := io.ReadAll(r.Body)
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var row Row
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Errorf("bad row: %v", err)
			}
			rows = append(rows, row)
		}
	}))
	defer srv.Close()

	ch := &ClickHouse{URL: srv.URL, User: "bindle", Password: "x"}
	err := ch.Write(context.Background(), []model.Step{{TenantID: "acme", TraceID: "t", StepID: "s", StartUnixNano: 1_700_000_000_123_456_789}})
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "INSERT INTO bindle.steps FORMAT JSONEachRow" || gotUser != "bindle" {
		t.Fatalf("query %q user %q", gotQuery, gotUser)
	}
	if len(rows) != 1 || rows[0].StartTime != "2023-11-14 22:13:20.123456789" || rows[0].Attributes == nil || rows[0].Links == nil {
		t.Fatalf("rows: %+v", rows)
	}
}

func TestClickHouseErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Code: 60. Table does not exist", http.StatusNotFound)
	}))
	defer srv.Close()
	err := (&ClickHouse{URL: srv.URL}).Write(context.Background(), []model.Step{{}})
	if err == nil || !strings.Contains(err.Error(), "Table does not exist") {
		t.Fatalf("want surfaced error, got %v", err)
	}
}
