// Linked runs: runs connected by OpenTelemetry span links (bindle.resume(),
// the MCP helpers, or any OTel SDK). Backend: GET /api/traces/{id}/links.
import { formatDuration } from './format';
import type { Run } from './types';

export type LinkKind = 'resumed' | 'handoff' | 'called' | '';

export interface TraceLink {
  direction: 'outgoing' | 'incoming';
  kind: LinkKind;
  step_id: string; // the step carrying the link (this run for outgoing, the other run for incoming)
  step_name: string;
  trace_id: string; // the other run
  span_id: string;
  run: Run | null; // null when the other run isn't stored
}

// Outgoing links point back to the run this one continues; incoming links
// come from runs that continue this one.
const LABELS: Record<TraceLink['direction'], Record<LinkKind, string>> = {
  outgoing: { resumed: 'Continued from', handoff: 'Handed off from', called: 'Called by', '': 'Linked to' },
  incoming: { resumed: 'Continued in', handoff: 'Handed off to', called: 'Calls', '': 'Linked from' },
};

// Earlier runs first, then later ones, then plain links.
const ORDER = ['Continued from', 'Handed off from', 'Called by', 'Continued in', 'Handed off to', 'Calls', 'Linked to', 'Linked from'];

export function linkLabel(l: Pick<TraceLink, 'direction' | 'kind'>): string {
  return LABELS[l.direction]?.[l.kind] ?? LABELS[l.direction]?.[''] ?? 'Linked';
}

export interface LinkGroup {
  label: string;
  links: TraceLink[];
}

/** Links grouped by label, one entry per other run in each group. */
export function groupLinks(links: TraceLink[]): LinkGroup[] {
  const groups = new Map<string, TraceLink[]>();
  for (const l of links) {
    const label = linkLabel(l);
    const g = groups.get(label) ?? [];
    if (!g.some((x) => x.trace_id === l.trace_id)) g.push(l);
    groups.set(label, g);
  }
  return [...groups.entries()]
    .sort((a, b) => ORDER.indexOf(a[0]) - ORDER.indexOf(b[0]))
    .map(([label, ls]) => ({ label, links: ls }));
}

function agentName(l: TraceLink): string {
  return l.run?.agent || 'another agent';
}

/**
 * A plain sentence for the run summary when this run continues, takes over
 * from or serves another run; undefined otherwise. `started` is this run's
 * start time, used to say how much later it continued.
 */
export function linkSentence(links: TraceLink[], started?: string): string | undefined {
  const out = links.find((l) => l.direction === 'outgoing' && l.kind !== '');
  if (!out) return undefined;
  let gap = '';
  if (out.run && started) {
    const ms = Date.parse(started) - Date.parse(out.run.started_at);
    if (Number.isFinite(ms) && ms >= 1000) gap = `, ${formatDuration(ms)} after it started`;
  }
  switch (out.kind) {
    case 'resumed':
      return `This run continues an earlier run of ${agentName(out)}${gap}.`;
    case 'handoff':
      return `This run took over from ${agentName(out)}${gap}.`;
    case 'called':
      return `This run was started to serve a call from ${agentName(out)}.`;
  }
  return undefined;
}
