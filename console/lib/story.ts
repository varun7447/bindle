// A run, explained in plain sentences: what happened, where it failed, where
// the time went. Deterministic (no model call); unit-tested.

import { formatCost, formatDuration, formatPercent } from './format';
import { securityLines } from './security';
import { stepKind } from './steps';
import type { Run, StepSummary } from './types';

export type Tone = 'good' | 'warning' | 'critical' | 'neutral';

export interface StoryLine {
  tone: Tone;
  text: string;
  /** Step to jump to, if the line is about one. */
  stepId?: string;
}

export interface KindTime {
  type: string;
  ms: number;
  share: number; // 0-1 of leaf time
  steps: number;
}

export interface RunStory {
  headline: string;
  tone: Tone;
  lines: StoryLine[];
  /** Where the time went, by step type, over leaf steps (no double counting of parents). */
  time: KindTime[];
  firstFailureId?: string;
}

/** Tools called this often in one run are flagged as a possible loop. */
export const LOOP_CALLS = 5;

const PLURAL: Record<string, [string, string]> = {
  llm_call: ['LLM call', 'LLM calls'],
  tool_call: ['tool call', 'tool calls'],
  retrieval: ['retrieval', 'retrievals'],
  decision: ['decision', 'decisions'],
  eval: ['eval', 'evals'],
  handoff: ['handoff', 'handoffs'],
  guardrail: ['guardrail check', 'guardrail checks'],
};

function count(n: number, type: string): string {
  const [one, many] = PLURAL[type] ?? [stepKind(type).label.toLowerCase() + ' step', stepKind(type).label.toLowerCase() + ' steps'];
  return `${n} ${n === 1 ? one : many}`;
}

export function joinList(parts: string[]): string {
  if (parts.length <= 1) return parts.join('');
  return `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}`;
}

function quote(s: string, max = 140): string {
  const t = s.replace(/\s+/g, ' ').trim();
  return `“${t.length > max ? t.slice(0, max - 1) + '…' : t}”`;
}

export function buildStory(run: Run, steps: StepSummary[]): RunStory {
  const failed = run.status === 'failed';
  const agent = run.agent || 'This agent';
  const lines: StoryLine[] = [];

  // 1. What it did.
  const work = steps.filter((s) => s.type !== 'agent');
  const byType = new Map<string, number>();
  for (const s of work) byType.set(s.type, (byType.get(s.type) ?? 0) + 1);
  const order = ['llm_call', 'tool_call', 'retrieval', 'decision', 'eval', 'handoff', 'guardrail'];
  const kinds = [...byType.keys()].sort((a, b) => (order.indexOf(a) + 1 || 99) - (order.indexOf(b) + 1 || 99));
  if (kinds.length) lines.push({ tone: 'neutral', text: `It made ${joinList(kinds.map((k) => count(byType.get(k)!, k)))}.` });

  // 2. Root cause: the earliest failed step that isn't an agent wrapper.
  const failures = steps.filter((s) => s.status === 'failure');
  const rootCause = failures.find((s) => s.type !== 'agent') ?? failures[0];
  if (rootCause) {
    const err = rootCause.error_message ? `: ${quote(rootCause.error_message)}` : '';
    const recovered = steps.some((s) => s.name === rootCause.name && s.status !== 'failure' && s.attempt > rootCause.attempt);
    lines.push({
      tone: recovered && !failed ? 'warning' : 'critical',
      text: `${rootCause.name} failed${err}.${recovered ? ' A retry succeeded.' : ''}`,
      stepId: rootCause.step_id,
    });
    const others = new Set(failures.filter((s) => s.type !== 'agent' && s.name !== rootCause.name).map((s) => s.name));
    if (others.size) lines.push({ tone: 'critical', text: `Also failed: ${joinList([...others].slice(0, 4))}.` });
  }

  // Security signals on steps: leads to review, not verdicts.
  lines.push(...securityLines(steps));

  // 3. Possible loops: the same tool called many times.
  const tools = new Map<string, StepSummary[]>();
  for (const s of steps) if (s.type === 'tool_call') tools.set(s.tool_name || s.name, [...(tools.get(s.tool_name || s.name) ?? []), s]);
  for (const [name, calls] of [...tools.entries()].sort((a, b) => b[1].length - a[1].length)) {
    if (calls.length < LOOP_CALLS) break;
    lines.push({ tone: 'warning', text: `${name} was called ${calls.length} times, which may be a loop.`, stepId: calls[0]!.step_id });
  }

  // 4. Where the time went (leaf steps only, so parents aren't counted twice).
  const parents = new Set(steps.map((s) => s.parent_step_id).filter(Boolean));
  const leaves = steps.filter((s) => !parents.has(s.step_id) && s.type !== 'agent');
  const total = leaves.reduce((n, s) => n + Math.max(0, s.duration_ms), 0);
  const timeMap = new Map<string, { ms: number; steps: number }>();
  for (const s of leaves) {
    const t = timeMap.get(s.type) ?? { ms: 0, steps: 0 };
    t.ms += Math.max(0, s.duration_ms);
    t.steps += 1;
    timeMap.set(s.type, t);
  }
  const time = [...timeMap.entries()]
    .map(([type, t]) => ({ type, ms: t.ms, steps: t.steps, share: total > 0 ? t.ms / total : 0 }))
    .sort((a, b) => b.ms - a.ms);
  const slowest = [...leaves].sort((a, b) => b.duration_ms - a.duration_ms)[0];
  if (time.length && total > 0 && slowest) {
    const top = time[0]!;
    lines.push({
      tone: 'neutral',
      text: `Most time went to ${count(top.steps, top.type)} (${formatPercent(top.share)}). The slowest step was ${slowest.name} at ${formatDuration(slowest.duration_ms)}.`,
      stepId: slowest.step_id,
    });
  }

  // 5. Cost.
  if (run.cost_usd > 0 || run.cost_partial) {
    lines.push({
      tone: 'neutral',
      text: run.cost_partial
        ? run.cost_usd > 0
          ? `It cost at least ${formatCost(run.cost_usd)}; ${run.cost_unknown_steps} model step${run.cost_unknown_steps === 1 ? ' has' : 's have'} no known price.`
          : `Its cost is unknown: ${run.cost_unknown_steps} model step${run.cost_unknown_steps === 1 ? ' has' : 's have'} no known price.`
        : `It cost ${formatCost(run.cost_usd)}.`,
    });
  }

  const outcome = failed ? 'failed' : run.status === 'ok' ? 'succeeded' : 'finished';
  return {
    headline: `${agent} ${outcome} after ${formatDuration(run.duration_ms)}.`,
    tone: failed ? 'critical' : lines.some((l) => l.tone === 'warning') ? 'warning' : 'good',
    lines,
    time,
    firstFailureId: rootCause?.step_id,
  };
}
