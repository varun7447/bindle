package alerts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/query"
	"github.com/bindle-ai/bindle/backend/internal/security"
)

// SignalDetector is the optional part of the Detector behind security
// signals and topics (search.Store implements it).
type SignalDetector interface {
	SecuritySignals(ctx context.Context, tenant string, from, to time.Time) ([]query.SecuritySignal, error)
	Topics(ctx context.Context, tenant, agent string, from, to time.Time) (*query.Topics, error)
}

// flagNames are the security flags in alert text.
var flagNames = map[string]string{
	security.PromptInjection: "possible prompt injection",
	security.SecretExposed:   "an exposed credential",
	security.Exfiltration:    "possible data exfiltration",
}

type eventFn func(fp, summary, traceID string, details map[string]any) Event

// detectMore handles the quality and digest rule kinds.
func (e *Evaluator) detectMore(ctx context.Context, tenant string, r Rule, now, from time.Time, limit int, ev eventFn) ([]Event, error) {
	switch r.Kind {
	case KindNegativeFeedback:
		fb, err := e.Detect.RecentFeedback(ctx, tenant, from, now)
		if err != nil {
			return nil, err
		}
		var neg, total uint64
		for _, f := range fb {
			neg += f.Negative
			total += f.Positive + f.Negative
		}
		min := uint64(r.MinRuns)
		if min == 0 {
			min = 5
		}
		if total < min || float64(neg)/float64(total) < r.Threshold {
			return nil, nil
		}
		window := time.Duration(r.WindowMinutes) * time.Minute
		slot := now.Truncate(window).Unix()
		return []Event{ev(fmt.Sprintf("feedback:%d", slot),
			fmt.Sprintf("Users gave negative feedback %.0f%% of the time over the last %d minutes (%d of %d; threshold %.0f%%)",
				100*float64(neg)/float64(total), r.WindowMinutes, neg, total, r.Threshold*100), "",
			map[string]any{"negative": neg, "total": total})}, nil

	case KindCostRegression:
		spend, err := e.Detect.CostByAgent(ctx, tenant, from, now)
		if err != nil {
			return nil, err
		}
		agents := make([]string, 0, len(spend))
		for a := range spend {
			agents = append(agents, a)
		}
		sort.Strings(agents)
		min := uint64(r.MinRuns)
		if min == 0 {
			min = 5
		}
		var out []Event
		for _, agent := range agents {
			if len(out) >= limit {
				break
			}
			vs, err := e.Detect.AgentVersions(ctx, tenant, agent, from, now)
			if err != nil {
				return nil, err
			}
			if len(vs) < 2 || vs[0].Runs < min || vs[1].Runs < min || vs[1].CostUSD <= 0 || vs[0].CostPartial || vs[1].CostPartial {
				continue
			}
			newer, older := vs[0].CostUSD/float64(vs[0].Runs), vs[1].CostUSD/float64(vs[1].Runs)
			if newer < older*(1+r.Threshold) {
				continue
			}
			out = append(out, ev("costreg:"+agent+":"+vs[0].Version+":"+vs[1].Version,
				fmt.Sprintf("%s %s costs %s per run, %.0f%% more than %s (%s)", orDash(agent), name(vs[0].Version), usd(newer), 100*(newer/older-1), name(vs[1].Version), usd(older)), "",
				map[string]any{"agent": agent, "version": vs[0].Version, "previous": vs[1].Version, "cost_per_run": newer, "previous_cost_per_run": older}))
		}
		return out, nil

	case KindNewError:
		cur, err := e.Detect.ErrorPatterns(ctx, tenant, from, now)
		if err != nil {
			return nil, err
		}
		before, err := e.Detect.ErrorPatterns(ctx, tenant, from.Add(-7*24*time.Hour), from)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, p := range before {
			seen[p.Pattern] = true
		}
		var out []Event
		for _, p := range cur {
			if len(out) >= limit {
				break
			}
			if seen[p.Pattern] || float64(p.Failures) < r.Threshold {
				continue
			}
			h := sha256.Sum256([]byte(p.Pattern))
			out = append(out, ev("newerr:"+hex.EncodeToString(h[:8]),
				fmt.Sprintf("New error in %s, %d time%s: %s", p.StepName, p.Failures, plural(p.Failures), truncate(p.Pattern, 160)), p.SampleTrace,
				map[string]any{"pattern": p.Pattern, "step": p.StepName, "failures": p.Failures}))
		}
		return out, nil

	case KindSecuritySignal:
		sd, ok := e.Detect.(SignalDetector)
		if !ok {
			return nil, fmt.Errorf("security signals are not available")
		}
		sigs, err := sd.SecuritySignals(ctx, tenant, from, now)
		if err != nil {
			return nil, err
		}
		window := time.Duration(r.WindowMinutes) * time.Minute
		slot := now.Truncate(window).Unix()
		var out []Event
		for _, s := range sigs {
			if len(out) >= limit {
				break
			}
			if float64(s.Steps) < r.Threshold {
				continue
			}
			why := topReason(s.Reasons)
			trace := ""
			if len(s.Examples) > 0 {
				trace = s.Examples[0].TraceID
			}
			summary := fmt.Sprintf("%s: %d step%s flagged for %s in the last %d minutes (%d run%s)",
				orDash(s.Agent), s.Steps, plural(s.Steps), flagNames[s.Flag], r.WindowMinutes, s.Runs, plural(s.Runs))
			if text := security.Explain[why]; text != "" {
				summary += ": " + text
			}
			out = append(out, ev(fmt.Sprintf("security:%s:%s:%d", s.Flag, s.Agent, slot), summary, trace,
				map[string]any{"flag": s.Flag, "agent": s.Agent, "steps": s.Steps, "runs": s.Runs, "reasons": s.Reasons}))
		}
		return out, nil

	case KindWeeklyDigest:
		// Mondays from 09:00 UTC, once per ISO week.
		if now.Weekday() != time.Monday || now.Hour() < 9 {
			return nil, nil
		}
		year, week := now.ISOWeek()
		weekFrom := now.Add(-7 * 24 * time.Hour)
		ins, err := e.Detect.Insights(ctx, tenant, weekFrom, now)
		if err != nil {
			return nil, err
		}
		ov, err := e.Detect.Overview(ctx, tenant, weekFrom, now, 24*time.Hour)
		if err != nil {
			return nil, err
		}
		text := digest(ov.Runs, ov.FailedRuns, ov.CostUSD, ov.CostUnknownSteps > 0, ins.Previous.Runs, ins.Previous.FailedRuns, ins.Previous.CostUSD, ins.Issues)
		details := map[string]any{"runs": ov.Runs, "failed_runs": ov.FailedRuns, "cost_usd": ov.CostUSD, "week": fmt.Sprintf("%d-W%02d", year, week)}
		// What users asked about: topic names only, never their messages.
		if sd, ok := e.Detect.(SignalDetector); ok {
			if t, err := sd.Topics(ctx, tenant, "", weekFrom, now); err == nil && t != nil {
				text += topicsDigest(t)
				details["rising_topics"], details["new_topics"] = topicTerms(t.Rising), topicTerms(t.New)
			} else if err != nil && e.Log != nil {
				e.Log.Warn("weekly digest: topics unavailable", "tenant", tenant, "err", err)
			}
		}
		return []Event{ev(fmt.Sprintf("digest:%d-W%02d", year, week), text, "", details)}, nil
	}
	return nil, fmt.Errorf("unknown rule kind %q", r.Kind)
}

func name(v string) string {
	if v == "" {
		return "unversioned"
	}
	return v
}

func plural(n uint64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func rate(failed, runs uint64) float64 {
	if runs == 0 {
		return 0
	}
	return float64(failed) / float64(runs)
}

// digest is the weekly summary text: counts only, never content.
func digest(runs, failed uint64, cost float64, costPartial bool, prevRuns, prevFailed uint64, prevCost float64, issues []query.Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Weekly summary: %d runs (%d the week before), %.0f%% failed (%.0f%% before), ", runs, prevRuns, 100*rate(failed, runs), 100*rate(prevFailed, prevRuns))
	if costPartial {
		b.WriteString("at least ")
	}
	fmt.Fprintf(&b, "%s spent (%s before).", usd(cost), usd(prevCost))
	n := 0
	for _, is := range issues {
		if is.StepType == "agent" {
			continue
		}
		if n == 0 {
			b.WriteString(" Failing most: ")
		} else {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s (%d failures)", is.StepName, is.Failures)
		n++
		if n == 3 {
			break
		}
	}
	if n > 0 {
		b.WriteString(".")
	}
	return b.String()
}

// topReason is the most frequent reason (ties: alphabetical).
func topReason(reasons map[string]uint64) string {
	best, n := "", uint64(0)
	for r, c := range reasons {
		if c > n || (c == n && r < best) {
			best, n = r, c
		}
	}
	return best
}

func topicTerms(ts []query.Topic) []string {
	out := []string{}
	for _, t := range ts {
		out = append(out, t.Term)
	}
	return out
}

// topicsDigest is the digest's "what users asked about" sentence.
func topicsDigest(t *query.Topics) string {
	var b strings.Builder
	if len(t.Rising) > 0 {
		b.WriteString(" Users asked more about: " + strings.Join(firstN(topicTerms(t.Rising), 5), ", ") + ".")
	}
	if len(t.New) > 0 {
		b.WriteString(" New this week: " + strings.Join(firstN(topicTerms(t.New), 5), ", ") + ".")
	}
	if t.PreviousRuns > 0 && t.Runs > 0 {
		if change := float64(t.Runs)/float64(t.PreviousRuns) - 1; change >= 0.5 || change <= -0.33 {
			fmt.Fprintf(&b, " User requests %s %.0f%%.", map[bool]string{true: "rose", false: "fell"}[change > 0], 100*abs(change))
		}
	}
	return b.String()
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
