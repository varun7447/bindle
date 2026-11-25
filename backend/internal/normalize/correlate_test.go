package normalize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bindle-ai/bindle/backend/internal/correlate"
	"github.com/bindle-ai/bindle/backend/internal/model"
)

func TestCorrelationsAreHashedAndNeverStored(t *testing.T) {
	sp := model.Span{TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("b", 16), Name: "support run",
		Attributes: map[string]any{
			"gen_ai.operation.name":        "invoke_agent",
			"bindle.correlation.ticket_id": correlate.Digest("ticket_id", "T-123"),
			"bindle.correlation.email":     "jane.doe@example.com", // raw, sent without the SDK
		}}
	st := newN().Normalize("t_1", sp)
	want := []string{correlate.FromValue("t_1", "email", "jane.doe@example.com"), correlate.FromValue("t_1", "ticket_id", "T-123")}
	if len(st.Correlations) != 2 || st.Correlations[0] != want[0] || st.Correlations[1] != want[1] {
		t.Fatalf("correlations = %v, want %v", st.Correlations, want)
	}
	b, _ := json.Marshal(st)
	for _, leak := range []string{"jane.doe@example.com", "bindle.correlation.", correlate.Digest("ticket_id", "T-123")} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("step still carries %q", leak)
		}
	}
	if other := newN().Normalize("t_2", sp); other.Correlations[1] == st.Correlations[1] {
		t.Fatal("correlations must be bound to the tenant")
	}
}
