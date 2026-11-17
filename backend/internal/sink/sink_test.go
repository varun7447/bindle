package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bindle-ai/bindle/backend/internal/model"
)

func TestStdoutWritesOneJSONRowPerStep(t *testing.T) {
	var buf bytes.Buffer
	steps := []model.Step{
		{TenantID: "t", TraceID: "a", StepID: "1", StartUnixNano: 1_700_000_000_123_456_789},
		{TenantID: "t", TraceID: "a", StepID: "2", Attributes: map[string]string{"k": "v"}, Links: []string{"x:y"}},
	}
	if err := (&Stdout{W: &buf}).Write(context.Background(), steps); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d", len(lines))
	}
	var r Row
	if err := json.Unmarshal([]byte(lines[0]), &r); err != nil {
		t.Fatal(err)
	}
	if r.TenantID != "t" || r.StartTime != "2023-11-14 22:13:20.123456789" || r.Attributes == nil || r.Links == nil {
		t.Errorf("row = %+v", r)
	}
}
