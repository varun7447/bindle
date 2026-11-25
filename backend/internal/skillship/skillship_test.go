package skillship

import (
	"strings"
	"testing"
	"time"
)

func trace(i int) string { return strings.Repeat("0", 31) + string(rune('a'+i%6)) }

func report(fixed, uncovered, failed, regressions int) Report {
	r := Report{Agent: "refunds", Path: "skills/retry-refund/SKILL.md", SHA256: strings.Repeat("a", 64), Lesson: "les_0000000000000000",
		EvalRuns: 10, Regressions: regressions}
	n := 0
	add := func(run RunResult) {
		run.TraceID = strings.Repeat("0", 30) + string(rune('a'+n/10)) + string(rune('a'+n%10))
		n++
		r.Runs = append(r.Runs, run)
	}
	for i := 0; i < fixed; i++ {
		add(RunResult{Fixed: true, Covered: true})
	}
	for i := 0; i < uncovered; i++ {
		add(RunResult{Covered: false, Detail: "tool lookup not in the recording"})
	}
	for i := 0; i < failed; i++ {
		add(RunResult{Covered: true, Detail: "failed the same way"})
	}
	return r
}

func TestJudge(t *testing.T) {
	cases := []struct {
		name      string
		r         Report
		threshold int
		pass      bool
		reason    string
	}{
		{"fixes enough", report(4, 0, 1, 0), 80, true, "fixed 4 of 5"},
		{"uncovered counts as not fixed", report(3, 1, 1, 0), 80, false, "fixed 3 of 5 replayed failures (60%); 80% needed"},
		{"eval regression fails it", report(5, 0, 0, 1), 80, false, "1 eval result(s) regressed"},
		{"stricter threshold", report(4, 0, 1, 0), 100, false, "100% needed"},
		{"bad threshold uses the default", report(4, 0, 1, 0), 0, true, "threshold 80%"},
	}
	for _, c := range cases {
		v := Judge(c.r, c.threshold)
		if v.Passed != c.pass || !strings.Contains(v.Reason, c.reason) {
			t.Errorf("%s: passed=%v reason=%q", c.name, v.Passed, v.Reason)
		}
	}
}

func TestValidate(t *testing.T) {
	good := report(1, 0, 0, 0)
	if err := Validate(&good); err != nil {
		t.Fatal(err)
	}
	bad := []func(r *Report){
		func(r *Report) { r.Agent = "" },
		func(r *Report) { r.Path = "notes/x.md" },
		func(r *Report) { r.SHA256 = "nope" },
		func(r *Report) { r.Runs = nil },
		func(r *Report) { r.Runs = append(r.Runs, r.Runs[0]) },         // duplicate trace
		func(r *Report) { r.Runs[0].Covered = false },                  // fixed but uncovered
		func(r *Report) { r.Regressions = -1 },                         // not a count
		func(r *Report) { r.Runs = make([]RunResult, MaxProofRuns+1) }, // too many
	}
	for i, f := range bad {
		r := report(1, 0, 0, 0)
		f(&r)
		if err := Validate(&r); err == nil {
			t.Errorf("case %d: accepted an invalid report", i)
		}
	}
	long := report(1, 0, 0, 0)
	long.Gaps = make([]string, MaxGaps+5)
	long.Gaps[0] = strings.Repeat("x", 500)
	if err := Validate(&long); err != nil || len(long.Gaps) != MaxGaps || len(long.Gaps[0]) != 300 {
		t.Fatalf("gaps not capped: %v %d %d", err, len(long.Gaps), len(long.Gaps[0]))
	}
}

const skillMD = `---
name: retry-refund
description: Retry a refund once when the payments API times out. Use when refund_payment fails with a timeout.
metadata:
  bindle-lesson: les_0123456789abcdef
%s---

When refund_payment times out, wait a second and retry it once.
`

func TestServeScopesByVersion(t *testing.T) {
	all := Release{Path: "skills/retry-refund/SKILL.md", Release: 2, SHA256: "s1", Body: strings.Replace(skillMD, "%s", "", 1)}
	scoped := Release{Path: "skills/other-skill/SKILL.md", Release: 1, SHA256: "s2",
		Body: strings.Replace(strings.Replace(skillMD, "retry-refund", "other-skill", 1), "%s", "  bindle-agent-versions: \"v12 v13\"\n", 1)}
	broken := Release{Path: "skills/broken/SKILL.md", Release: 1, Body: "not a skill"}
	got := Serve([]Release{scoped, all, broken}, "v14")
	if len(got) != 1 || got[0].Name != "retry-refund" || got[0].Release != 2 || got[0].Body == "" {
		t.Fatalf("v14: %+v", got)
	}
	got = Serve([]Release{scoped, all}, "v13")
	if len(got) != 2 || got[0].Path != "skills/other-skill/SKILL.md" || len(got[0].Versions) != 2 {
		t.Fatalf("v13: %+v", got)
	}
	if got := Serve([]Release{scoped}, ""); len(got) != 1 {
		t.Fatalf("no version: scoped skills are offered (%+v)", got)
	}
	if Ref("skills/a/SKILL.md", 3) != "skills/a/SKILL.md@3" {
		t.Fatal("ref")
	}
}

func facts(loaded bool, n, successes, recurred int) []RunFact {
	out := make([]RunFact, n)
	for i := range out {
		out[i] = RunFact{TraceID: trace(i), Loaded: loaded, Success: i < successes, Recurred: i < recurred, Tokens: 1000, Ms: 200}
		if loaded {
			out[i].Tokens, out[i].Ms = 1300, 260
		}
	}
	return out
}

func TestMeasureDecisions(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		with     []RunFact
		without  []RunFact
		decision string
		reason   string
	}{
		{"too few runs", facts(true, 10, 2, 0), facts(false, 50, 40, 10), "too_few_runs", "needs 20 runs"},
		{"better: keep", facts(true, 30, 28, 0), facts(false, 30, 20, 10), "keep", "success 93% with it"},
		{"success fell: roll back", facts(true, 30, 18, 0), facts(false, 30, 24, 0), "rollback", "success fell from 80% to 60%"},
		{"small dip: keep", facts(true, 30, 22, 0), facts(false, 30, 24, 0), "keep", ""},
		{"recurs more: roll back", facts(true, 30, 30, 6), facts(false, 30, 30, 3), "rollback", "happened in 20% of runs with it, 10% without"},
	}
	for _, c := range cases {
		m := Measure("refunds", "skills/retry-refund/SKILL.md", 2, append(c.with, c.without...), now)
		if m.Decision != c.decision || !strings.Contains(m.Reason, c.reason) {
			t.Errorf("%s: %s (%s)", c.name, m.Decision, m.Reason)
		}
	}
	m := Measure("a", "p", 1, append(facts(true, 20, 20, 0), facts(false, 20, 20, 0)...), now)
	if m.With.AvgTokens != 1300 || m.Without.AvgTokens != 1000 || m.With.AvgMs != 260 {
		t.Fatalf("overhead not measured: %+v", m)
	}
	// Runs that loaded another release of the same skill are left out.
	other := facts(false, 40, 0, 40)
	for i := range other {
		other[i].Other = true
	}
	m = Measure("a", "p", 1, append(append(facts(true, 25, 25, 0), facts(false, 25, 25, 0)...), other...), now)
	if m.Without.Runs != 25 || m.Decision != "keep" {
		t.Fatalf("other releases counted: %+v", m)
	}
}
