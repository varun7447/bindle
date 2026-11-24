package skilljobs

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/evals"
	"github.com/bindle-ai/bindle/backend/internal/outcomes"
	"github.com/bindle-ai/bindle/backend/internal/query"
	"github.com/bindle-ai/bindle/backend/internal/search"
	"github.com/bindle-ai/bindle/backend/internal/skillship"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const tr1 = "0123456789abcdef0123456789abcdef"

type fakeReader struct{ result string }

func (f fakeReader) GetTrace(_ context.Context, _, id string, _ int) (*query.Trace, error) {
	if id != tr1 {
		return nil, nil
	}
	return &query.Trace{Run: query.Run{TraceID: id, Agent: "refunds"},
		Steps: []query.StepSummary{{StepID: "s1", Type: "tool_call", ToolName: "refund_payment", Status: "failure", ErrorMessage: "timed out"}}}, nil
}
func (f fakeReader) GetStep(context.Context, string, string, string) (*query.StepDetail, error) {
	return &query.StepDetail{Input: `{"order":"o_1"}`, Output: f.result}, nil
}
func (f fakeReader) RunsForEval(context.Context, string, []string) ([]evals.Run, error) {
	return []evals.Run{{Input: "refund o_1", Output: "sorry"}}, nil
}

func TestCassettesFill(t *testing.T) {
	stored := map[string][]byte{}
	c := &Cassettes{
		Missing: func(context.Context, string, int) ([]control.LessonTrace, error) {
			return []control.LessonTrace{{Lesson: "les_1", TraceID: tr1}, {Lesson: "les_1", TraceID: strings.Repeat("f", 32)}}, nil
		},
		Reader: fakeReader{result: strings.Repeat("r", 600<<10)}, // too large: clipped to fit
		Put: func(_ context.Context, _, lesson, trace string, b []byte) error {
			stored[lesson+"/"+trace] = b
			return nil
		},
		Log: quiet,
	}
	if n := c.Fill(context.Background(), "t_1"); n != 1 {
		t.Fatalf("stored %d cassettes, want 1 (the other run is past retention)", n)
	}
	b := stored["les_1/"+tr1]
	if len(b) == 0 || len(b) > skillship.MaxCassetteBytes {
		t.Fatalf("cassette size %d", len(b))
	}
	var got struct {
		Format     string   `json:"format"`
		Input      string   `json:"input"`
		Incomplete []string `json:"incomplete"`
		ToolCalls  []struct {
			Tool, Status string
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Format != "bindle.cassette" || got.Input != "refund o_1" || len(got.ToolCalls) != 1 ||
		got.ToolCalls[0].Status != "failure" || !contains(got.Incomplete, "content_truncated") {
		t.Fatalf("cassette = %+v", got)
	}
}

type fakeShip struct {
	releases    []control.ShippingRelease
	measured    []skillship.Measurement
	rolledBack  []string
	live        []string
	rollbackErr error
}

func (f *fakeShip) Releases(context.Context) ([]control.ShippingRelease, error) {
	return f.releases, nil
}
func (f *fakeShip) Lessons(context.Context, string) (map[string]control.LessonBrief, error) {
	return map[string]control.LessonBrief{"les_0123456789abcdef": {ID: "les_0123456789abcdef", Tool: "refund_payment", Pattern: "timed out"}}, nil
}
func (f *fakeShip) PutMeasurement(_ context.Context, _ string, m skillship.Measurement) error {
	f.measured = append(f.measured, m)
	return nil
}
func (f *fakeShip) Rollback(_ context.Context, _, _, path string, rel int, _ string) (bool, error) {
	f.rolledBack = append(f.rolledBack, skillship.Ref(path, rel))
	return f.rollbackErr == nil, f.rollbackErr
}
func (f *fakeShip) GoLive(_ context.Context, _, _, path, _ string) (int, error) {
	f.live = append(f.live, path)
	return 7, nil
}

const body = "---\nname: retry-refund\ndescription: Retry a refund once on a timeout. Use when refund_payment times out.\nmetadata:\n  bindle-lesson: les_0123456789abcdef\n---\n\nRetry once.\n"

func rel(path string, release int, noteStatus string) control.ShippingRelease {
	r := control.ShippingRelease{Tenant: "t_1", NoteStatus: noteStatus, NoteSHA: "sha"}
	r.Agent, r.Path, r.Release.Release, r.SHA256, r.Body = "refunds", path, release, "sha", body
	r.ReleasedAt = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	return r
}

func runs(loaded bool, n, ok int) []skillship.RunFact {
	out := make([]skillship.RunFact, n)
	for i := range out {
		out[i] = skillship.RunFact{Loaded: loaded, Success: i < ok}
	}
	return out
}

func TestMeasureRollsBackAWorseRelease(t *testing.T) {
	ship := &fakeShip{releases: []control.ShippingRelease{
		rel("skills/worse/SKILL.md", 3, "live"),
		rel("skills/better/SKILL.md", 1, "approved"),
	}}
	var lessonSeen search.SkillLesson
	j := &Measure{Store: ship, Log: quiet, Now: func() time.Time { return time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC) },
		Facts: func(_ context.Context, _, _, path string, _ int, l search.SkillLesson, from, to time.Time, _ map[string][]outcomes.OutcomeFact, _ outcomes.Settings) ([]skillship.RunFact, error) {
			lessonSeen = l
			if !from.Equal(time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)) {
				t.Errorf("window starts %v, want a week before the release", from)
			}
			if path == "skills/worse/SKILL.md" {
				return append(runs(true, 30, 15), runs(false, 30, 27)...), nil
			}
			return append(runs(true, 30, 29), runs(false, 30, 20)...), nil
		}}
	res := j.RunOnce(context.Background())
	if res.Measured != 2 || res.RolledBack != 1 || res.Live != 1 {
		t.Fatalf("result = %+v", res)
	}
	if len(ship.rolledBack) != 1 || ship.rolledBack[0] != "skills/worse/SKILL.md@3" {
		t.Fatalf("rolled back %v", ship.rolledBack)
	}
	if len(ship.live) != 1 || ship.live[0] != "skills/better/SKILL.md" {
		t.Fatalf("went live %v (a rolled-back release must not)", ship.live)
	}
	if lessonSeen.Tool != "refund_payment" || lessonSeen.Pattern != "timed out" {
		t.Fatalf("recurrence measured against %+v, want the skill's lesson", lessonSeen)
	}
	if ship.measured[0].Decision != "rollback" || ship.measured[1].Decision != "keep" {
		t.Fatalf("decisions %s, %s", ship.measured[0].Decision, ship.measured[1].Decision)
	}
}

func TestMeasureWaitsForEnoughRuns(t *testing.T) {
	ship := &fakeShip{releases: []control.ShippingRelease{rel("skills/new/SKILL.md", 1, "approved")}}
	j := &Measure{Store: ship, Log: quiet, Facts: func(context.Context, string, string, string, int, search.SkillLesson, time.Time, time.Time, map[string][]outcomes.OutcomeFact, outcomes.Settings) ([]skillship.RunFact, error) {
		return append(runs(true, 3, 0), runs(false, 3, 3)...), nil // looks worse, but too few runs
	}}
	res := j.RunOnce(context.Background())
	if res.RolledBack != 0 || ship.measured[0].Decision != "too_few_runs" || res.Live != 1 {
		t.Fatalf("result = %+v, decision %s", res, ship.measured[0].Decision)
	}
}
