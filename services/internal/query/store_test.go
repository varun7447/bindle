package query

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The console parses these shapes; keep them stable.
func TestJSONShapes(t *testing.T) {
	ts := Time{Time: time.Date(2026, 11, 16, 12, 0, 0, 123456789, time.UTC)}
	b, _ := json.Marshal(StepDetail{StepSummary: StepSummary{StepID: "s", StartTime: ts}, Input: "x"})
	got := string(b)
	for _, want := range []string{`"step_id":"s"`, `"start_time":"2026-11-16T12:00:00.123456789Z"`, `"input":"x"`, `"redacted_kinds":null`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, `"StepSummary"`) {
		t.Error("StepSummary must be flattened into the step object")
	}
}
