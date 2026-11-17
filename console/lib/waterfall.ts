// Lays out a trace's steps as a waterfall: depth-first tree order, each row
// positioned on a shared time axis. Pure; unit-tested.

import type { StepSummary } from './types';
import { diffMs } from './format';

export interface WaterfallRow {
  step: StepSummary;
  depth: number;
  /** Offset from trace start and width, as % of the trace span. */
  leftPct: number;
  widthPct: number;
  startOffsetMs: number;
}

export interface Waterfall {
  rows: WaterfallRow[];
  spanMs: number;
}

const MIN_WIDTH_PCT = 0.4;
const MAX_DEPTH = 64;

export function buildWaterfall(steps: StepSummary[]): Waterfall {
  if (steps.length === 0) return { rows: [], spanMs: 0 };

  // Offsets are relative to the first step's timestamp (t0 = 0).
  const ref = steps[0]!.start_time;
  const start = new Map<string, number>();
  let t0 = Infinity;
  let t1 = -Infinity;
  for (const s of steps) {
    const st = diffMs(s.start_time, ref);
    start.set(s.step_id, st);
    t0 = Math.min(t0, st);
    t1 = Math.max(t1, st + Math.max(0, s.duration_ms));
  }
  const spanMs = Math.max(t1 - t0, 0);

  const ids = new Set(steps.map((s) => s.step_id));
  const children = new Map<string, StepSummary[]>();
  const roots: StepSummary[] = [];
  for (const s of steps) {
    // Orphans (parent not in this trace or truncated away) render as roots.
    if (s.parent_step_id && s.parent_step_id !== s.step_id && ids.has(s.parent_step_id)) {
      const list = children.get(s.parent_step_id) ?? [];
      list.push(s);
      children.set(s.parent_step_id, list);
    } else {
      roots.push(s);
    }
  }
  const byStart = (a: StepSummary, b: StepSummary) =>
    (start.get(a.step_id) ?? 0) - (start.get(b.step_id) ?? 0) || a.step_id.localeCompare(b.step_id);

  const rows: WaterfallRow[] = [];
  const seen = new Set<string>();
  const visit = (s: StepSummary, depth: number) => {
    if (seen.has(s.step_id)) return; // guards against parent cycles
    seen.add(s.step_id);
    const off = (start.get(s.step_id) ?? t0) - t0;
    const leftPct = spanMs > 0 ? (off / spanMs) * 100 : 0;
    const rawWidth = spanMs > 0 ? (Math.max(0, s.duration_ms) / spanMs) * 100 : 100;
    rows.push({
      step: s,
      depth: Math.min(depth, MAX_DEPTH),
      leftPct: Math.min(leftPct, 100 - MIN_WIDTH_PCT),
      widthPct: Math.min(Math.max(rawWidth, MIN_WIDTH_PCT), 100 - Math.min(leftPct, 100 - MIN_WIDTH_PCT)),
      startOffsetMs: off,
    });
    for (const c of (children.get(s.step_id) ?? []).sort(byStart)) visit(c, depth + 1);
  };
  for (const r of roots.sort(byStart)) visit(r, 0);
  // Steps only reachable through a cycle were never visited from a root.
  for (const s of [...steps].sort(byStart)) visit(s, 0);
  return { rows, spanMs };
}

/** Default selection: first failed step, else the first row. */
export function defaultStepId(rows: WaterfallRow[]): string | undefined {
  return (rows.find((r) => r.step.status === 'failure') ?? rows[0])?.step.step_id;
}
