// Compare two runs' paths: align their steps (longest common subsequence
// over step signatures), find where they first diverge, and list what
// changed for steps both runs took. Pure; unit-tested.

import { formatCost, formatCount, formatDuration } from './format';
import type { StepSummary } from './types';

export type AlignKind = 'same' | 'only_a' | 'only_b';

export interface Change {
  field: 'status' | 'duration' | 'tokens' | 'cost' | 'model' | 'arguments';
  text: string;
}

export interface AlignedStep {
  kind: AlignKind;
  a?: StepSummary;
  b?: StepSummary;
  /** For "same": what differs between the two (empty when nothing notable). */
  changes: Change[];
}

export interface Trajectory {
  rows: AlignedStep[];
  /** Index into rows of the first row that isn't a match, or -1 when the paths are identical. */
  divergeAt: number;
  same: number;
  onlyA: number;
  onlyB: number;
  /** Plain sentences, most important first. */
  summary: string[];
}

/** Steps per run compared; longer runs are cut (the LCS table is O(n·m)). */
export const MAX_COMPARE_STEPS = 1500;

/** What makes two steps "the same step": its type and what it called. */
export function signature(s: StepSummary): string {
  // LLM calls match regardless of model, so a model switch shows as a change, not a new path.
  if (s.type === 'llm_call') return 'llm_call';
  if (s.type === 'tool_call' || s.type === 'retrieval') return `${s.type}:${s.tool_name || s.name}`;
  return `${s.type}:${s.name}`;
}

/** A short human label for a step. */
export function stepLabel(s: StepSummary): string {
  if (s.type === 'llm_call') return s.model ? `an LLM call (${s.model})` : `the LLM call ${s.name}`;
  if (s.type === 'tool_call') return s.tool_name || s.name;
  return s.name;
}

/** Steps that make up a run's path: everything but agent wrappers and score steps, in start order. */
export function pathSteps(steps: StepSummary[]): StepSummary[] {
  return steps
    .filter((s) => s.type !== 'agent' && s.type !== 'eval')
    .map((s, i) => ({ s, i }))
    .sort((x, y) => Date.parse(x.s.start_time) - Date.parse(y.s.start_time) || x.i - y.i)
    .map((x) => x.s)
    .slice(0, MAX_COMPARE_STEPS);
}

function lcs(a: string[], b: string[]): Array<[number, number]> {
  const n = a.length;
  const m = b.length;
  const w = m + 1;
  const t = new Uint16Array((n + 1) * w);
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      t[i * w + j] = a[i] === b[j] ? t[(i + 1) * w + j + 1]! + 1 : Math.max(t[(i + 1) * w + j]!, t[i * w + j + 1]!);
    }
  }
  const pairs: Array<[number, number]> = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      pairs.push([i, j]);
      i++;
      j++;
    } else if (t[(i + 1) * w + j]! >= t[i * w + j + 1]!) i++;
    else j++;
  }
  return pairs;
}

const ratio = (x: number, y: number) => (y > 0 ? x / y : x > 0 ? Infinity : 1);

/** Notable differences between two steps that both runs took. */
export function stepChanges(a: StepSummary, b: StepSummary): Change[] {
  const out: Change[] = [];
  if (a.status !== b.status) out.push({ field: 'status', text: `${a.status === 'failure' ? 'failed' : 'succeeded'} in A, ${b.status === 'failure' ? 'failed' : 'succeeded'} in B` });
  const d = ratio(b.duration_ms, a.duration_ms);
  if (Math.abs(b.duration_ms - a.duration_ms) >= 200 && (d >= 2 || d <= 0.5)) {
    out.push({ field: 'duration', text: `${formatDuration(a.duration_ms)} → ${formatDuration(b.duration_ms)}` });
  }
  const ta = a.input_tokens + a.output_tokens;
  const tb = b.input_tokens + b.output_tokens;
  const tr = ratio(tb, ta);
  if (Math.abs(tb - ta) >= 100 && (tr >= 1.5 || tr <= 2 / 3)) out.push({ field: 'tokens', text: `${formatCount(ta)} → ${formatCount(tb)} tokens` });
  if (a.model && b.model && a.model !== b.model) out.push({ field: 'model', text: `${a.model} → ${b.model}` });
  if (a.cost_known && b.cost_known) {
    const cr = ratio(b.cost_usd, a.cost_usd);
    if (Math.abs(b.cost_usd - a.cost_usd) >= 0.001 && (cr >= 1.5 || cr <= 2 / 3)) out.push({ field: 'cost', text: `${formatCost(a.cost_usd)} → ${formatCost(b.cost_usd)}` });
  }
  return out;
}

/** Tool arguments as comparable text: JSON is re-serialized with sorted keys, other text has whitespace collapsed. */
export function normalizeArgs(input: string): string {
  const t = input.trim();
  try {
    return JSON.stringify(sortKeys(JSON.parse(t)));
  } catch {
    return t.replace(/\s+/g, ' ');
  }
}

function sortKeys(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(sortKeys);
  if (v && typeof v === 'object') {
    return Object.fromEntries(
      Object.keys(v as Record<string, unknown>)
        .sort()
        .map((k) => [k, sortKeys((v as Record<string, unknown>)[k])]),
    );
  }
  return v;
}

/** Whether two tool calls were given different arguments (inputs as captured). */
export function argsDiffer(a: string, b: string): boolean {
  return normalizeArgs(a) !== normalizeArgs(b);
}

const plural = (n: number, one: string, many = one + 's') => `${n} ${n === 1 ? one : many}`;

export function compareRuns(aSteps: StepSummary[], bSteps: StepSummary[]): Trajectory {
  const a = pathSteps(aSteps);
  const b = pathSteps(bSteps);
  const pairs = lcs(a.map(signature), b.map(signature));
  const rows: AlignedStep[] = [];
  let i = 0;
  let j = 0;
  const flush = (ui: number, uj: number) => {
    while (i < ui) rows.push({ kind: 'only_a', a: a[i++], changes: [] });
    while (j < uj) rows.push({ kind: 'only_b', b: b[j++], changes: [] });
  };
  for (const [pi, pj] of pairs) {
    flush(pi, pj);
    rows.push({ kind: 'same', a: a[i], b: b[j], changes: stepChanges(a[i]!, b[j]!) });
    i++;
    j++;
  }
  flush(a.length, b.length);

  const divergeAt = rows.findIndex((r) => r.kind !== 'same');
  const same = pairs.length;
  const onlyA = a.length - same;
  const onlyB = b.length - same;

  const summary: string[] = [];
  if (!a.length && !b.length) summary.push('Neither run has steps to compare.');
  else if (divergeAt === -1) {
    const changed = rows.filter((r) => r.changes.length).length;
    summary.push(`Both runs took the same ${plural(same, 'step')} in the same order${changed ? `; ${plural(changed, 'step')} behaved differently` : ''}.`);
  } else {
    const r = rows[divergeAt]!;
    const lead = divergeAt === 0 ? 'The runs diverge at the first step' : `The runs match for the first ${plural(divergeAt, 'step')}, then diverge`;
    const next = rows.slice(divergeAt);
    const nextA = next.find((x) => x.kind === 'only_a')?.a;
    const nextB = next.find((x) => x.kind === 'only_b')?.b;
    const what =
      nextA && nextB && r.kind !== 'same'
        ? `A calls ${stepLabel(nextA)}, B calls ${stepLabel(nextB)}`
        : r.kind === 'only_a'
          ? `A calls ${stepLabel(r.a!)}, which B skips`
          : `B calls ${stepLabel(r.b!)}, which A doesn't`;
    summary.push(`${lead}: ${what}.`);
    const parts: string[] = [];
    if (onlyB) parts.push(`B has ${plural(onlyB, 'step')} A doesn't`);
    if (onlyA) parts.push(`A has ${plural(onlyA, 'step')} B doesn't`);
    if (parts.length) summary.push(`${parts.join('; ')} (${same} in common).`);
  }

  // Per-tool call counts that differ.
  const counts = (xs: StepSummary[]) => {
    const m = new Map<string, number>();
    for (const s of xs) if (s.type === 'tool_call') m.set(s.tool_name || s.name, (m.get(s.tool_name || s.name) ?? 0) + 1);
    return m;
  };
  const ca = counts(a);
  const cb = counts(b);
  const toolDiffs = [...new Set([...ca.keys(), ...cb.keys()])]
    .map((t) => ({ t, x: ca.get(t) ?? 0, y: cb.get(t) ?? 0 }))
    .filter((d) => d.x !== d.y)
    .sort((p, q) => Math.abs(q.y - q.x) - Math.abs(p.y - p.x) || p.t.localeCompare(q.t))
    .slice(0, 3);
  for (const d of toolDiffs) summary.push(`${d.t}: called ${plural(d.x, 'time')} in A, ${plural(d.y, 'time')} in B.`);

  const flipped = rows.filter((r) => r.kind === 'same' && r.changes.some((c) => c.field === 'status'));
  if (flipped.length) {
    const s = flipped[0]!;
    summary.push(`${stepLabel(s.b!)} ${s.b!.status === 'failure' ? 'fails in B but not in A' : 'fails in A but not in B'}${flipped.length > 1 ? `, and ${plural(flipped.length - 1, 'other step')} changed status` : ''}.`);
  }
  return { rows, divergeAt, same, onlyA, onlyB, summary };
}
