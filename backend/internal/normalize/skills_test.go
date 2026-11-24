package normalize

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bindle-ai/bindle/backend/internal/model"
)

func TestSkillUseEvents(t *testing.T) {
	ev := func(path string, release any) model.Event {
		return model.Event{Name: SkillEvent, Attributes: map[string]any{"bindle.skill.path": path, "bindle.skill.release": release}}
	}
	sp := model.Span{TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("b", 16), Name: "plan",
		Attributes: map[string]any{"gen_ai.operation.name": "chat"},
		Events: []model.Event{
			ev("skills/retry-refund/SKILL.md", int64(2)),
			ev("skills/retry-refund/SKILL.md", int64(2)), // loaded twice: once
			ev("skills/check-address/SKILL.md", int64(1)),
			ev("../etc/passwd", int64(1)),                     // not a skill path
			ev("skills/no-release/SKILL.md", int64(0)),        // no release
			{Name: "exception", Attributes: map[string]any{}}, // other events ignored
		}}
	st := newN().Normalize("t_1", sp)
	want := []string{"skills/retry-refund/SKILL.md@2", "skills/check-address/SKILL.md@1"}
	if !reflect.DeepEqual(st.SkillsUsed, want) {
		t.Fatalf("skills used = %v, want %v", st.SkillsUsed, want)
	}
}
