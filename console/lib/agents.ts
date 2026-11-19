// The Agents page: compare an agent's versions, and tool health. Pure;
// unit-tested.

import { type Delta, delta } from './insights';

export interface VersionStats {
  version: string;
  runs: number;
  failed_runs: number;
  cost_usd: number;
  cost_partial: boolean;
  p50_run_ms: number;
  p95_run_ms: number;
  avg_steps: number;
  avg_llm_calls: number;
  input_tokens: number;
  output_tokens: number;
  cached_tokens: number;
  tool_calls: number;
  tool_failures: number;
  first_run_start: string;
  last_run_start: string;
}

export interface ToolHealth {
  tool_name: string;
  framework: string;
  calls: number;
  failures: number;
  argument_errors: number;
  runs: number;
  p50_ms: number;
  p95_ms: number;
  avg_output_bytes: number;
  max_output_bytes: number;
  top_error: string;
  sample_trace_id: string;
  sample_step_id: string;
}

export interface VersionRow {
  v: VersionStats;
  failureRate: number;
  costPerRun: number;
  cacheRate: number;
  toolErrorRate: number;
  /** Change versus the next older version (undefined for the oldest). */
  vs?: { version: string; failureRate: Delta; costPerRun: Delta; p95: Delta; steps: Delta; toolErrorRate: Delta };
}

/** Fewer runs than this and a comparison is noise. */
export const MIN_COMPARE_RUNS = 5;

const rate = (a: number, b: number) => (b ? a / b : 0);

export function versionRows(versions: VersionStats[]): VersionRow[] {
  const rows: VersionRow[] = versions.map((v) => ({
    v,
    failureRate: rate(v.failed_runs, v.runs),
    costPerRun: rate(v.cost_usd, v.runs),
    cacheRate: rate(v.cached_tokens, v.input_tokens),
    toolErrorRate: rate(v.tool_failures, v.tool_calls),
  }));
  // versions arrive most recent first; compare each with the next older one
  for (let i = 0; i < rows.length - 1; i++) {
    const a = rows[i]!;
    const b = rows[i + 1]!;
    if (a.v.runs < MIN_COMPARE_RUNS || b.v.runs < MIN_COMPARE_RUNS) continue;
    a.vs = {
      version: b.v.version,
      failureRate: delta(a.failureRate, b.failureRate, { higherIsBetter: false, percentPoints: true }),
      costPerRun: delta(a.costPerRun, b.costPerRun, { higherIsBetter: false }),
      p95: delta(a.v.p95_run_ms, b.v.p95_run_ms, { higherIsBetter: false }),
      steps: delta(a.v.avg_steps, b.v.avg_steps, { higherIsBetter: false }),
      toolErrorRate: delta(a.toolErrorRate, b.toolErrorRate, { higherIsBetter: false, percentPoints: true }),
    };
  }
  return rows;
}

/** One sentence on the newest version against the one before. */
export function verdict(rows: VersionRow[]): string | null {
  const top = rows[0];
  if (!top?.vs) return null;
  const parts: string[] = [];
  const say = (label: string, d: Delta) => {
    if (d.direction === 'flat' || d.direction === 'new') return;
    parts.push(`${label} ${d.text}${d.good === undefined ? '' : d.good ? ' (better)' : ' (worse)'}`);
  };
  say('failure rate', top.vs.failureRate);
  say('cost per run', top.vs.costPerRun);
  say('p95 duration', top.vs.p95);
  say('steps per run', top.vs.steps);
  say('tool errors', top.vs.toolErrorRate);
  const name = (v: string) => v || 'unversioned';
  if (!parts.length) return `${name(top.v.version)} performs like ${name(top.vs.version)}.`;
  return `${name(top.v.version)} vs ${name(top.vs.version)}: ${parts.join(', ')}.`;
}

export interface ToolUse {
  tool_name: string;
  calls: number;
  runs: number;
  failures: number;
}

export interface VersionToolUsage {
  version: string;
  runs: number;
  tools: ToolUse[];
}

export interface ToolUsageRow {
  tool: string;
  /** Average calls per run in the newer version. */
  now: number;
  /** Average calls per run in the older version. */
  before: number;
  /** Plain sentence when the change is notable. */
  note?: string;
}

/** Changes smaller than this many calls per run are noise. */
const MIN_CALLS_DELTA = 0.3;

const fmtPer = (x: number) => (x >= 10 ? x.toFixed(0) : x.toFixed(1));

/**
 * Per tool, average calls per run in a version against the version before
 * it (versions arrive most recent first). Notable changes get a sentence,
 * e.g. "v2 calls search 3.1× as often per run (2.4 vs 0.8)". Null when there
 * is no older version or either has fewer than MIN_COMPARE_RUNS runs.
 */
export function toolUsageChanges(usage: VersionToolUsage[], version?: string): { version: string; previous: string; rows: ToolUsageRow[] } | null {
  const at = version === undefined ? 0 : usage.findIndex((u) => u.version === version);
  const cur = usage[at];
  const prev = usage[at + 1];
  if (at < 0 || !cur || !prev || cur.runs < MIN_COMPARE_RUNS || prev.runs < MIN_COMPARE_RUNS) return null;
  const name = (v: string) => v || 'unversioned';
  const per = (u: VersionToolUsage) => new Map(u.tools.map((t) => [t.tool_name, t.calls / u.runs]));
  const a = per(cur);
  const b = per(prev);
  const rows: ToolUsageRow[] = [...new Set([...a.keys(), ...b.keys()])].map((tool) => {
    const now = a.get(tool) ?? 0;
    const before = b.get(tool) ?? 0;
    const row: ToolUsageRow = { tool, now, before };
    if (Math.abs(now - before) < MIN_CALLS_DELTA) return row;
    const v = name(cur.version);
    const p = name(prev.version);
    if (before === 0) row.note = `${v} calls ${tool}, which ${p} never called (${fmtPer(now)} per run).`;
    else if (now === 0) row.note = `${v} no longer calls ${tool} (${fmtPer(before)} per run in ${p}).`;
    else if (now / before >= 1.5) row.note = `${v} calls ${tool} ${(now / before).toFixed(1)}× as often per run (${fmtPer(now)} vs ${fmtPer(before)}).`;
    else if (now / before <= 2 / 3) row.note = `${v} calls ${tool} ${Math.round((1 - now / before) * 100)}% less often per run (${fmtPer(now)} vs ${fmtPer(before)}).`;
    return row;
  });
  rows.sort((x, y) => Number(!!y.note) - Number(!!x.note) || Math.abs(y.now - y.before) - Math.abs(x.now - x.before) || x.tool.localeCompare(y.tool));
  return { version: cur.version, previous: prev.version, rows };
}

/** Tools that need a look, worst first. */
export function toolFlags(t: ToolHealth): string[] {
  const flags: string[] = [];
  if (t.calls && t.failures / t.calls >= 0.1) flags.push('fails often');
  if (t.argument_errors >= 2) flags.push('bad arguments');
  if (t.max_output_bytes >= 50_000) flags.push('large results');
  if (t.p95_ms >= 10_000) flags.push('slow');
  return flags;
}
