package lessons

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func group(runs int, pattern string) Group {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	return Group{Agent: "refund-agent", StepName: "lookup_order", StepType: "tool_call", Tool: "lookup_order", Pattern: pattern,
		Runs: runs, Failures: runs + 1, FirstSeen: now.Add(-time.Hour), LastSeen: now,
		Traces: []string{"0123456789abcdef0123456789abcdef"},
		Samples: []Evidence{{TraceID: "0123456789abcdef0123456789abcdef", StepID: "0123456789abcdef", StepType: "tool_call", Name: "lookup_order",
			Tool: "lookup_order", At: now, Input: `{"email": "jane.doe@example.com", "key": "AKIAABCDEFGHIJKLMNOP"}`,
			Output: "order <N> not found; token sk-abcdefghijklmnopqrstuvwxyz1234"}}}
}

func TestBuildThresholdAndCause(t *testing.T) {
	ls := Build([]Group{group(4, "order <N> not found"), group(6, "invalid argument order_id"), group(9, "")}, 5, true)
	if len(ls) != 1 {
		t.Fatalf("lessons %+v", ls)
	}
	l := ls[0]
	if l.Cause != CauseBadArguments || l.Runs != 6 || !strings.HasPrefix(l.ID, "les_") {
		t.Fatalf("lesson %+v", l)
	}
	if l.ID != ID("refund-agent", "lookup_order", "lookup_order", "invalid argument order_id") {
		t.Fatal("ID not stable")
	}
	if !strings.Contains(l.Title, "lookup_order failed in 6 runs") {
		t.Fatalf("title %q", l.Title)
	}
	// minRuns below the floor falls back to the default.
	if got := Build([]Group{group(4, "x timed out")}, 0, true); len(got) != 0 {
		t.Fatal("default threshold not applied")
	}
}

func TestCauses(t *testing.T) {
	cases := map[string]string{
		"request timed out after <N>s": CauseTimeout,
		"HTTP 429 Too Many Requests":   CauseRateLimited,
		"403 forbidden":                CausePermission,
		"order <N> not found":          CauseNotFound,
		"missing required field 'id'":  CauseBadArguments,
		"service unavailable":          CauseUnavailable,
		"something odd":                CauseError,
	}
	for in, want := range cases {
		if got := Cause(in); got != want {
			t.Errorf("Cause(%q) = %s, want %s", in, got, want)
		}
	}
}

// Raw secrets and personal data that slipped past ingest never reach a pack.
func TestPackNeverCarriesRawSecrets(t *testing.T) {
	ls := Build([]Group{group(6, "order <N> not found for jane.doe@example.com")}, 5, true)
	raw, err := json.Marshal(ls[0].Pack)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"AKIAABCDEFGHIJKLMNOP", "sk-abcdefghijklmnopqrstuvwxyz1234", "jane.doe@example.com"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("pack carries %q: %s", secret, raw)
		}
	}
	if !strings.Contains(string(raw), "[REDACTED:SECRET]") || ls[0].Pack.Format != PackFormat {
		t.Fatalf("pack %s", raw)
	}
}

func TestPackHonorsCaptureOff(t *testing.T) {
	ls := Build([]Group{group(6, "order <N> not found")}, 5, false)
	p := ls[0].Pack
	if p.Content || p.Evidence[0].Input != "" || p.Evidence[0].Output != "" || p.Evidence[0].Name != "lookup_order" {
		t.Fatalf("pack %+v", p)
	}
}

func TestClip(t *testing.T) {
	s := strings.Repeat("é", PackField)
	if got := clip(s, PackField); len(got) > PackField+len("…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("clip %d", len(got))
	}
}
