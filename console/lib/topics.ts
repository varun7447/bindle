// "What users ask about": topics rising or new compared with the period
// before, from the end user's input of each run. Pure; unit-tested.

import { formatCount, formatPercent } from './format';

export interface Topic {
  term: string;
  runs: number;
  previous_runs: number;
  share: number;
  previous_share: number;
  example_trace_ids: string[];
}

export interface Topics {
  rising: Topic[];
  new: Topic[];
  runs: number;
  previous_runs: number;
  avg_chars: number;
  previous_avg_chars: number;
}

export interface TopicsResponse {
  window: string;
  agent: string;
  topics: Topics;
}

export interface TopicRow {
  term: string;
  kind: 'new' | 'rising';
  /** "in 40% of runs (was 10%)" or "in 12 runs, not asked before". */
  detail: string;
  /** For a bar: share of runs now and before (0-1). */
  share: number;
  previousShare: number;
  examples: string[];
}

export function topicRows(t: Topics): TopicRow[] {
  const rows: TopicRow[] = [];
  for (const n of t.new) {
    rows.push({
      term: n.term,
      kind: 'new',
      detail: `${formatCount(n.runs)} run${n.runs === 1 ? '' : 's'} (${formatPercent(n.share)}), not asked about before`,
      share: n.share,
      previousShare: 0,
      examples: n.example_trace_ids,
    });
  }
  for (const r of t.rising) {
    rows.push({
      term: r.term,
      kind: 'rising',
      detail: `${formatPercent(r.share)} of runs, up from ${formatPercent(r.previous_share)}`,
      share: r.share,
      previousShare: r.previous_share,
      examples: r.example_trace_ids,
    });
  }
  return rows;
}

/** One sentence about volume and length, or null when there's nothing to compare. */
export function topicsSummary(t: Topics, previousLabel: string): string | null {
  if (t.runs === 0) return null;
  const parts = [`${formatCount(t.runs)} run${t.runs === 1 ? '' : 's'} with a user message`];
  if (t.previous_runs > 0) {
    parts.push(`${formatCount(t.previous_runs)} in the ${previousLabel}`);
    if (t.previous_avg_chars > 0 && t.avg_chars > 0) {
      const ch = t.avg_chars / t.previous_avg_chars - 1;
      if (Math.abs(ch) >= 0.25) parts.push(`messages are ${formatPercent(Math.abs(ch))} ${ch > 0 ? 'longer' : 'shorter'} on average`);
    }
  } else {
    parts.push(`none in the ${previousLabel} to compare with`);
  }
  return parts.join(', ') + '.';
}
