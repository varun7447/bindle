import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import * as bindle from '../src/index.js';
import { Cassette, RecordedToolError, UnrecordedCallError } from '../src/cassette.js';

const recording = (over: Record<string, unknown> = {}) => ({
  format: 'bindle.cassette',
  version: 1,
  trace_id: '0af7651916cd43dd8448eb211c80319c',
  agent: 'refunds',
  input: 'Where is order A1?',
  output: 'It shipped.',
  tool_calls: [
    { step_id: '1', tool: 'lookup_order', arguments: '{"orderId":"A1","verbose":false}', result: '{"status":"shipped"}', status: 'success' },
    { step_id: '2', tool: 'lookup_order', arguments: '{"verbose":false,"orderId":"A1"}', result: '{"status":"delivered"}', status: 'success' },
    { step_id: '3', tool: 'refund', arguments: '{"orderId":"A1"}', result: '', status: 'failure', error: 'refund service timed out' },
  ],
  incomplete: [],
  ...over,
});

describe('cassette', () => {
  it('loads from a file and validates the format', () => {
    const dir = mkdtempSync(join(tmpdir(), 'bindle-cassette-'));
    const p = join(dir, 'run.cassette.json');
    writeFileSync(p, JSON.stringify(recording()));
    const c = Cassette.load(p);
    expect(c.input).toBe('Where is order A1?');
    expect(c.calls).toHaveLength(3);
    expect(() => new Cassette({ format: 'x' })).toThrow(/not a Bindle cassette/);
    expect(() => new Cassette(recording({ version: 2 }))).toThrow(/newer/);
    expect(bindle.Cassette).toBe(Cassette);
  });

  it('answers by arguments, in order, and replays failures', () => {
    const c = new Cassette(recording());
    expect(c.result('lookup_order', { verbose: false, orderId: 'A1' })).toEqual({ status: 'shipped' });
    expect(c.result('lookup_order', '{"orderId":"A1","verbose":false}', { raw: true })).toBe('{"status":"delivered"}');
    expect(() => c.result('refund', { orderId: 'A1' })).toThrow(RecordedToolError);
    expect(() => c.result('send_email', { to: 'x' })).toThrow(UnrecordedCallError);
    const r = c.report();
    expect(r.matchedByArguments).toBe(3);
    expect(r.missing).toEqual(['send_email']);
    expect(r.unused).toEqual([]);
  });

  it('falls back to call order unless matchOrder is false', () => {
    expect(new Cassette(recording()).result('lookup_order', { orderId: 'B2' })).toEqual({ status: 'shipped' });
    expect(() => new Cassette(recording(), { matchOrder: false }).result('lookup_order', { orderId: 'B2' })).toThrow(UnrecordedCallError);
  });

  it('wraps sync and async tools, calling through when asked', async () => {
    const c = new Cassette(recording(), { onMissing: 'call' });
    const lookup = c.wrap('lookup_order', (args: { orderId: string; verbose: boolean }) => ({ status: `real ${args.orderId}` }));
    const refund = c.wrap('refund', async (args: { orderId: string }) => `real ${args.orderId}`);
    const email = c.wrap('send_email', (to: string) => `sent to ${to}`);
    expect(lookup({ orderId: 'A1', verbose: false })).toEqual({ status: 'shipped' });
    await expect(refund({ orderId: 'A1' })).rejects.toThrow(RecordedToolError);
    expect(email('ops@example.com')).toBe('sent to ops@example.com');
    expect(c.unused().map((x) => x.stepId)).toEqual(['2']);
    c.reset();
    expect(c.unused()).toHaveLength(3);
    const strict = new Cassette(recording());
    const asyncLookup = strict.wrap('lookup_order', async (_: { orderId: string; verbose: boolean }) => ({ status: 'real' }));
    await expect(asyncLookup({ orderId: 'A1', verbose: false })).resolves.toEqual({ status: 'shipped' });
    expect(() => strict.wrap('send_email', (to: string) => to)('x')).toThrow(UnrecordedCallError);
  });
});
