package ledger

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/bindle-ai/bindle/backend/internal/model"
)

// Summary renders one agent's day as plain-English Markdown for audit
// evidence: what it did, what failed, what it cost and which security
// signals were raised, with the chain's hashes for that day. It is computed
// from the entries alone, deterministically: the same entries always give
// the same text. Unknown cost is stated as unknown, never estimated.
func Summary(agent, day string, entries []Entry, check Result) string {
	name := agent
	if name == "" {
		name = "Unnamed agent"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Ledger summary: %s, %s (UTC)\n\n", name, day)
	if len(entries) == 0 {
		b.WriteString("No ledger entries were recorded for this agent on this day.\n")
		return b.String()
	}

	runs := map[string]bool{}
	byType := map[string]int{}
	tools := map[string]int{}
	type failure struct{ name, kind, class string }
	failures := map[failure]int{}
	flags := map[string]int{}
	var flagged, failed, priced, unpriced int
	var total float64
	drift := map[string]int{}
	vault := map[string]int{}
	shipped := map[string]int{}
	for _, e := range entries {
		if e.Kind == KindDrift {
			drift[driftLabel(e.DriftKind())]++
			continue
		}
		if e.Kind == KindVaultChange {
			vault[e.vaultAction()]++
			continue
		}
		if e.Kind == KindSkillChange {
			shipped[e.vaultAction()]++
			continue
		}
		if e.Kind != KindStep {
			continue
		}
		if e.TraceID != "" {
			runs[e.TraceID] = true
		}
		byType[e.StepType]++
		if e.ToolName != "" {
			tools[e.ToolName]++
		}
		if e.Status == model.StatusFailure {
			failed++
			failures[failure{e.StepName, e.StepType, e.ErrorClass}]++
		}
		if len(e.SecurityFlags) > 0 {
			flagged++
		}
		for _, f := range e.SecurityFlags {
			flags[f]++
		}
		switch {
		case e.CostKnown:
			priced++
			if v, err := strconv.ParseFloat(e.CostUSD, 64); err == nil {
				total += v
			}
		case e.StepType == model.StepLLM:
			unpriced++
		}
	}

	steps := 0
	for _, n := range byType {
		steps += n
	}
	fmt.Fprintf(&b, "**What it did.** %s, %s: %s.", plural(len(runs), "run"), plural(steps, "step"), typeList(byType))
	if len(tools) > 0 {
		fmt.Fprintf(&b, " Most-used tools: %s.", topCounts(tools, 5))
	}
	b.WriteString("\n\n")

	if failed == 0 {
		b.WriteString("**What failed.** Nothing: no step failed.\n\n")
	} else {
		fmt.Fprintf(&b, "**What failed.** %s failed:\n\n", plural(failed, "step"))
		keys := make([]failure, 0, len(failures))
		for k := range failures {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if failures[keys[i]] != failures[keys[j]] {
				return failures[keys[i]] > failures[keys[j]]
			}
			if keys[i].name != keys[j].name {
				return keys[i].name < keys[j].name
			}
			return keys[i].class < keys[j].class
		})
		for _, k := range keys {
			class := k.class
			if class == "" {
				class = "no error message"
			}
			fmt.Fprintf(&b, "- %s (%s): \"%s\", %s\n", k.name, typeLabel(k.kind), class, times(failures[k]))
		}
		b.WriteString("\n")
	}

	switch {
	case priced == 0 && unpriced > 0:
		fmt.Fprintf(&b, "**Cost.** Unknown: %s had no known price.\n\n", plural(unpriced, "model call"))
	case priced == 0:
		b.WriteString("**Cost.** No model cost was recorded.\n\n")
	case unpriced > 0:
		fmt.Fprintf(&b, "**Cost.** At least %s across %s; %s had no known price, so this is a lower bound.\n\n",
			money(total), plural(priced, "priced step"), plural(unpriced, "model call"))
	default:
		fmt.Fprintf(&b, "**Cost.** %s across %s.\n\n", money(total), plural(priced, "priced step"))
	}

	if flagged == 0 {
		b.WriteString("**Security signals.** None.\n\n")
	} else {
		fmt.Fprintf(&b, "**Security signals.** %s flagged for review: %s. Signals are leads to check, not verdicts.\n\n",
			plural(flagged, "step"), topCounts(flags, 10))
	}

	if len(drift) > 0 {
		n := 0
		for _, c := range drift {
			n += c
		}
		fmt.Fprintf(&b, "**Passport drift.** %s outside the agent's passport: %s. Observed only; nothing was blocked.\n\n", plural(n, "time"), topCounts(drift, 10))
	}

	if len(vault) > 0 {
		n := 0
		for _, c := range vault {
			n += c
		}
		fmt.Fprintf(&b, "**Knowledge vault.** %s: %s.\n\n", plural(n, "change"), topCounts(vault, 10))
	}
	if len(shipped) > 0 {
		n := 0
		for _, c := range shipped {
			n += c
		}
		fmt.Fprintf(&b, "**Learned skills.** %s by Bindle: %s.\n\n", plural(n, "change"), topCounts(shipped, 10))
	}

	first, last := entries[0], entries[len(entries)-1]
	fmt.Fprintf(&b, "**Evidence.** Ledger entries %d to %d (%s), hash-chained with SHA-256.\n\n", first.Seq, last.Seq, plural(len(entries), "entry"))
	fmt.Fprintf(&b, "- First entry hash: `%s`\n- Last entry hash: `%s`\n", hex.EncodeToString(first.Hash), hex.EncodeToString(last.Hash))
	if check.OK {
		fmt.Fprintf(&b, "- Chain check for this day: intact (%s verified).\n", plural(int(check.Entries), "entry"))
	} else if check.FirstBroken != nil {
		fmt.Fprintf(&b, "- Chain check for this day: **broken at entry %d**: %s.\n", check.FirstBroken.Seq, check.FirstBroken.Reason)
	}
	return b.String()
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if strings.HasSuffix(word, "y") && !strings.HasSuffix(word, "ey") {
		return strconv.Itoa(n) + " " + word[:len(word)-1] + "ies"
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return strconv.Itoa(n) + " times"
}

var typeLabels = map[string][2]string{
	model.StepLLM:       {"LLM call", "LLM calls"},
	model.StepTool:      {"tool call", "tool calls"},
	model.StepRetrieval: {"retrieval", "retrievals"},
	model.StepDecision:  {"decision", "decisions"},
	model.StepHandoff:   {"handoff", "handoffs"},
	model.StepEval:      {"score", "scores"},
	model.StepGuardrail: {"guardrail check", "guardrail checks"},
	model.StepAgent:     {"agent step", "agent steps"},
}

func typeLabel(t string) string {
	if l, ok := typeLabels[t]; ok {
		return l[0]
	}
	return "other step"
}

// typeList lists step counts by type in a fixed, readable order.
func typeList(byType map[string]int) string {
	order := []string{model.StepLLM, model.StepTool, model.StepRetrieval, model.StepDecision, model.StepHandoff,
		model.StepGuardrail, model.StepEval, model.StepAgent}
	var parts []string
	seen := map[string]bool{}
	for _, t := range order {
		if n := byType[t]; n > 0 {
			l := typeLabels[t]
			if n == 1 {
				parts = append(parts, "1 "+l[0])
			} else {
				parts = append(parts, strconv.Itoa(n)+" "+l[1])
			}
		}
		seen[t] = true
	}
	other := 0
	for t, n := range byType {
		if !seen[t] {
			other += n
		}
	}
	if other > 0 {
		parts = append(parts, plural(other, "other step"))
	}
	return strings.Join(parts, ", ")
}

// topCounts renders "a (3), b (1)", most frequent first, ties by name.
func topCounts(m map[string]int, limit int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > limit {
		keys = keys[:limit]
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s (%d)", k, m[k])
	}
	return strings.Join(parts, ", ")
}

func money(v float64) string {
	if v >= 1 {
		return fmt.Sprintf("$%.2f", v)
	}
	if v > 0 && v < 0.0001 {
		return "less than $0.0001"
	}
	return fmt.Sprintf("$%.4f", v)
}
