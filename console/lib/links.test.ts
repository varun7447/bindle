import { describe, expect, it } from 'vitest';
import { groupLinks, linkLabel, linkSentence, type TraceLink } from './links';
import type { Run } from './types';

const run = (over: Partial<Run> = {}): Run => ({
  trace_id: 'b'.repeat(32), started_at: '2026-11-19T10:00:00Z', duration_ms: 10, status: 'ok', root_status: 'success', steps: 3,
  failed_steps: 0, input_tokens: 0, output_tokens: 0, cost_usd: 0, cost_partial: false, cost_unknown_steps: 0, agent: 'triage',
  agent_version: '', framework: '', environment: '', session_id: '', ...over,
});

const link = (over: Partial<TraceLink>): TraceLink => ({
  direction: 'outgoing', kind: 'resumed', step_id: '1'.repeat(16), step_name: 'resume', trace_id: 'b'.repeat(32), span_id: '2'.repeat(16),
  run: run(), ...over,
});

describe('linked runs', () => {
  it('labels each direction and kind', () => {
    expect(linkLabel({ direction: 'outgoing', kind: 'resumed' })).toBe('Continued from');
    expect(linkLabel({ direction: 'incoming', kind: 'resumed' })).toBe('Continued in');
    expect(linkLabel({ direction: 'outgoing', kind: 'handoff' })).toBe('Handed off from');
    expect(linkLabel({ direction: 'incoming', kind: 'handoff' })).toBe('Handed off to');
    expect(linkLabel({ direction: 'outgoing', kind: 'called' })).toBe('Called by');
    expect(linkLabel({ direction: 'incoming', kind: '' })).toBe('Linked from');
  });

  it('groups earlier runs first and lists each run once per group', () => {
    const groups = groupLinks([
      link({ direction: 'incoming', kind: 'handoff', trace_id: 'c'.repeat(32) }),
      link({ direction: 'outgoing' }),
      link({ direction: 'outgoing', span_id: '3'.repeat(16) }), // same run, another span
      link({ direction: 'incoming', kind: '', trace_id: 'd'.repeat(32) }),
    ]);
    expect(groups.map((g) => [g.label, g.links.length])).toEqual([
      ['Continued from', 1],
      ['Handed off to', 1],
      ['Linked from', 1],
    ]);
    expect(groupLinks([])).toEqual([]);
  });

  it('says in the summary what this run continues', () => {
    expect(linkSentence([link({})], '2026-11-19T13:30:00Z')).toBe('This run continues an earlier run of triage, 3h 30m after it started.');
    expect(linkSentence([link({ kind: 'handoff', run: null })])).toBe('This run took over from another agent.');
    expect(linkSentence([link({ kind: 'called' })])).toBe('This run was started to serve a call from triage.');
    // Only outgoing links with a known kind describe this run.
    expect(linkSentence([link({ direction: 'incoming' }), link({ kind: '' })])).toBeUndefined();
  });
});
