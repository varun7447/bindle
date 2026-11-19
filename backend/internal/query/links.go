package query

import (
	"context"
	"strings"
)

// Link kinds, from the bindle.link.kind attribute the SDKs set on the step
// that carries the link (bindle.resume(), MCP and A2A helpers). Links from
// other OpenTelemetry sources have no kind.
const (
	LinkResumed = "resumed" // the run continues an earlier one (queue, human approval)
	LinkHandoff = "handoff" // another agent took over
	LinkCalled  = "called"  // the run was started to serve another run's call
)

// LinkKind normalizes a bindle.link.kind value: one of the kinds above, or
// "" for anything else.
func LinkKind(v string) string {
	switch k := strings.ToLower(strings.TrimSpace(v)); k {
	case LinkResumed, LinkHandoff, LinkCalled:
		return k
	}
	return ""
}

// TraceLink is one link between a run and another run.
//
// Outgoing: a step of this run links to a span of another run (this run
// continues, takes over from, or serves that run). Incoming: a step of
// another run links to a span of this run.
type TraceLink struct {
	Direction string `json:"direction"` // "outgoing" | "incoming"
	Kind      string `json:"kind"`      // LinkResumed | LinkHandoff | LinkCalled | ""
	// The step that carries the link: in this run for outgoing links, in the
	// other run for incoming ones.
	StepID   string `json:"step_id"`
	StepName string `json:"step_name"`
	// The other run, and the span of it the link points to (outgoing) or
	// comes from (incoming).
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
	// Run is the other run's summary; nil when it is not stored (sampled
	// out, outside retention, or not yet ingested).
	Run *Run `json:"-"`
}

// LinkStore is implemented by stores that can follow links between runs.
// Kept apart from Store so existing implementations need no change.
type LinkStore interface {
	TraceLinks(ctx context.Context, tenant, traceID string, limit int) ([]TraceLink, error)
}

// ParseLink splits a stored "traceid:spanid" link. ok is false for
// malformed values.
func ParseLink(v string) (traceID, spanID string, ok bool) {
	t, s, found := strings.Cut(v, ":")
	if !found || !isHex(t, 32) || !isHex(s, 16) {
		return "", "", false
	}
	return t, s, true
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
