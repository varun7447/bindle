package passport

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/query"
)

var known = map[string]bool{"EMAIL": true, "CARD": true, "SSN": true, "SECRET": true, "PHONE": true, "PRIVATE_KEY": true, "ACCOUNT_ID": true}

const full = `{
  "version": "bindle.passport/v1",
  "agent": "refund-agent",
  "owner": "payments-team@example.com",
  "identity": {"kind": "iam_role", "id": "refund-agent-role"},
  "autonomy_tier": 1,
  "tools": {"allow": ["lookup_order", "crm_*", "mcp:orders/*", "send_email"]},
  "data_classes": {"allow": ["EMAIL"], "egress_block": ["CARD", "SSN"]},
  "budget": {"per_run_usd": 0.5, "per_day_usd": 200},
  "limits": [{"tool": "refund_payment", "max": 3, "window": "5m"}],
  "release_rights": "none",
  "release_tools": ["deploy_*", "git_push"]
}`

func mustParse(t *testing.T, s string) *Passport {
	t.Helper()
	p, err := Parse([]byte(s), known)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseFull(t *testing.T) {
	p := mustParse(t, full)
	if p.Agent != "refund-agent" || *p.AutonomyTier != 1 || p.Limits[0].Duration() != 5*time.Minute || p.Budget.PerDayUSD != 200 {
		t.Fatalf("parsed %+v", p)
	}
	// Round trip keeps the difference between "not declared" and "none allowed".
	b, _ := json.Marshal(p)
	again := mustParse(t, string(b))
	if !reflect.DeepEqual(p, again) {
		t.Fatalf("round trip changed the passport:\n%+v\n%+v", p, again)
	}
	empty := mustParse(t, `{"version":"bindle.passport/v1","agent":"a","owner":"o","tools":{"allow":[]},"data_classes":{"allow":[]}}`)
	b, _ = json.Marshal(empty)
	again = mustParse(t, string(b))
	if again.Tools.Allow == nil || again.DataClasses.Allow == nil {
		t.Fatalf("empty lists became undeclared: %s", b)
	}
}

func TestParseRejects(t *testing.T) {
	base := func(extra string) string {
		return `{"version":"bindle.passport/v1","agent":"a","owner":"o"` + extra + `}`
	}
	cases := map[string]string{
		"unknown field":      base(`,"tool":{"allow":[]}`),
		"version":            `{"version":"bindle.passport/v2","agent":"a","owner":"o"}`,
		"no agent":           `{"version":"bindle.passport/v1","owner":"o"}`,
		"padded agent":       `{"version":"bindle.passport/v1","agent":" a","owner":"o"}`,
		"no owner":           `{"version":"bindle.passport/v1","agent":"a"}`,
		"identity kind":      base(`,"identity":{"kind":"robot","id":"x"}`),
		"tier":               base(`,"autonomy_tier":4`),
		"tools without list": base(`,"tools":{}`),
		"tool regex":         base(`,"tools":{"allow":["a b"]}`),
		"tool question":      base(`,"tools":{"allow":["a?"]}`),
		"label case":         base(`,"data_classes":{"allow":["email"]}`),
		"unknown label":      base(`,"data_classes":{"egress_block":["PASSPORT_NO"]}`),
		"negative budget":    base(`,"budget":{"per_run_usd":-1}`),
		"limit max":          base(`,"limits":[{"tool":"x","max":0,"window":"5m"}]`),
		"limit window":       base(`,"limits":[{"tool":"x","max":1,"window":"25h"}]`),
		"limit window unit":  base(`,"limits":[{"tool":"x","max":1,"window":"5d"}]`),
		"release rights":     base(`,"release_rights":"some"`),
		"trailing data":      base(``) + `{}`,
		"not json":           `version: bindle.passport/v1`,
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc), known); err == nil || !IsInvalid(err) {
			t.Errorf("%s: want a validation error, got %v", name, err)
		}
	}
	big := base(`,"owner2":"` + strings.Repeat("x", MaxBytes) + `"`)
	if _, err := Parse([]byte(big), known); err == nil {
		t.Error("oversized passport accepted")
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"lookup_order", "lookup_order", true},
		{"lookup_order", "lookup_orders", false},
		{"crm_*", "crm_find", true},
		{"crm_*", "crm_", true},
		{"crm_*", "xcrm_find", false},
		{"mcp:orders/*", "mcp:orders/get", true},
		{"*config_push*", "app_config_push_v2", true},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXcYYb", false},
		{"*", "anything", true},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v", c.pattern, c.name, got)
		}
	}
}

func TestStepDrift(t *testing.T) {
	p := mustParse(t, full)
	tool := func(name string, in []string, all []string) model.Step {
		return model.Step{Type: model.StepTool, ToolName: name, InputDataClasses: in, DataClasses: all}
	}
	cases := []struct {
		name string
		step model.Step
		want []string
	}{
		{"allowed tool", tool("lookup_order", nil, nil), nil},
		{"wildcard tool", tool("crm_find", nil, nil), nil},
		{"unlisted tool", tool("refund_payment", nil, nil), []string{"tool_not_allowed:refund_payment"}},
		{"allowed class", tool("lookup_order", []string{"EMAIL"}, []string{"EMAIL"}), nil},
		{"unlisted class", tool("lookup_order", nil, []string{"PHONE"}), []string{"data_class_not_allowed:PHONE"}},
		{"blocked egress", tool("send_email", []string{"CARD"}, []string{"CARD"}),
			[]string{"data_class_not_allowed:CARD", "egress_blocked:CARD"}},
		{"blocked class, not outbound", tool("lookup_order", []string{"CARD"}, []string{"CARD"}), []string{"data_class_not_allowed:CARD"}},
		{"release action", tool("deploy_prod", nil, nil), []string{"release_not_allowed:deploy_prod", "tool_not_allowed:deploy_prod"}},
		{"llm step with class", model.Step{Type: model.StepLLM, DataClasses: []string{"SSN"}}, []string{"data_class_not_allowed:SSN"}},
		{"llm step has no tool drift", model.Step{Type: model.StepLLM, Name: "chat"}, nil},
	}
	for _, c := range cases {
		if got := p.StepDrift(c.step); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	// Undeclared sections are not checked.
	loose := mustParse(t, `{"version":"bindle.passport/v1","agent":"a","owner":"o"}`)
	if d := loose.StepDrift(tool("anything", []string{"CARD"}, []string{"CARD"})); d != nil {
		t.Fatalf("passport without sections reported %v", d)
	}
	// Default release tools when the passport lists none.
	rel := mustParse(t, `{"version":"bindle.passport/v1","agent":"a","owner":"o","release_rights":"none"}`)
	if d := rel.StepDrift(tool("git_push", nil, nil)); !reflect.DeepEqual(d, []string{"release_not_allowed:git_push"}) {
		t.Fatalf("default release tools: %v", d)
	}
	if d := rel.StepDrift(tool("lookup_order", nil, nil)); d != nil {
		t.Fatalf("non-release tool flagged: %v", d)
	}
	var none *Passport
	if none.StepDrift(tool("x", nil, nil)) != nil {
		t.Fatal("nil passport reported drift")
	}
}

type fakeCounter struct {
	calls map[string]uint64
	runs  []query.Run
	spend map[string]float64
	froms []time.Time
}

func (f *fakeCounter) ToolCalls(_ context.Context, _, _, pattern string, from, _ time.Time) (uint64, string, error) {
	f.froms = append(f.froms, from)
	return f.calls[pattern], "trace-rate", nil
}
func (f *fakeCounter) CostlyRuns(_ context.Context, _ string, _, _ time.Time, _ float64, _ int) ([]query.Run, error) {
	return f.runs, nil
}
func (f *fakeCounter) CostByAgent(_ context.Context, _ string, _, _ time.Time) (map[string]float64, error) {
	return f.spend, nil
}

func TestStateful(t *testing.T) {
	p := mustParse(t, full)
	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	c := &fakeCounter{
		calls: map[string]uint64{"refund_payment": 4},
		runs:  []query.Run{{TraceID: "t1", Agent: "refund-agent", CostUSD: 0.9}, {TraceID: "t2", Agent: "other", CostUSD: 5}, {TraceID: "t3", Agent: "refund-agent", CostUSD: 0.5}},
		spend: map[string]float64{"refund-agent": 201.5},
	}
	got, err := Stateful(context.Background(), c, "t", p, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, f := range got {
		kinds = append(kinds, f.Reason())
	}
	want := []string{"rate_over_limit:refund_payment", "run_over_budget:t1", "day_over_budget:2026-11-24"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("findings %v, want %v", kinds, want)
	}
	if !c.froms[0].Equal(now.Add(-5 * time.Minute)) {
		t.Fatalf("rate window starts at %v", c.froms[0])
	}
	if got[0].Summary != "refund-agent called refund_payment 4 times in the last 5m (passport limit 3)" {
		t.Fatalf("summary %q", got[0].Summary)
	}
	c.calls["refund_payment"] = 3
	c.spend["refund-agent"] = 200
	c.runs = nil
	if got, _ := Stateful(context.Background(), c, "t", p, now.Add(-time.Hour), now); len(got) != 0 {
		t.Fatalf("at the limit is not over it: %v", got)
	}
}

// The CLI's Dogwood golden test uses this passport; Bindle must accept it.
func TestCLIExamplePassportIsValid(t *testing.T) {
	b, err := os.ReadFile("../../../sdks/python/tests/testdata/passport_full.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(b, known); err != nil {
		t.Fatal(err)
	}
}

// The published JSON Schema accepts the example and names every field the
// Go type has, so editors and Bindle agree.
func TestSchemaFileMatchesType(t *testing.T) {
	b, err := os.ReadFile("../../schema/bindle.passport.v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	typ := reflect.TypeOf(Passport{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if _, ok := schema.Properties[name]; !ok {
			t.Errorf("schema has no property %q", name)
		}
	}
	if len(schema.Properties) != typ.NumField() {
		t.Errorf("schema has %d properties, the type %d", len(schema.Properties), typ.NumField())
	}
}
