import { readFileSync } from 'node:fs';
import { InMemorySpanExporter } from '@opentelemetry/sdk-trace';
import { afterEach, describe, expect, it } from 'vitest';
import * as bindle from '../src/index.js';
import { makeToken, parseToken } from '../src/links.js';

// Shared with the Python SDK (sdks/python/tests/link_token_vectors.json).
const vectors = JSON.parse(readFileSync(new URL('../../python/tests/link_token_vectors.json', import.meta.url), 'utf8')) as {
  valid: { token: string; trace_id: string; span_id: string; sampled: boolean; agent: string | null; session: string | null; tracestate: string }[];
  invalid: string[];
};

describe('context tokens', () => {
  it('parses the shared valid vectors and round-trips them', () => {
    for (const v of vectors.valid) {
      const p = parseToken(v.token);
      expect(p, v.token).toBeDefined();
      expect(p!.spanContext.traceId).toBe(v.trace_id);
      expect(p!.spanContext.spanId).toBe(v.span_id);
      expect((p!.spanContext.traceFlags & 1) === 1).toBe(v.sampled);
      expect(p!.agent ?? null).toBe(v.agent);
      expect(p!.session ?? null).toBe(v.session);
      expect(p!.spanContext.traceState?.serialize() ?? '').toBe(v.tracestate);
      // Same bytes as the Python SDK makes for the same context.
      expect(makeToken(p!.spanContext, p!.agent, p!.session)).toBe(v.token);
    }
  });
  it('rejects the shared invalid vectors', () => {
    for (const t of [...vectors.invalid, undefined, 42]) expect(parseToken(t), String(t)).toBeUndefined();
  });
});

describe('resume and MCP propagation', () => {
  let mem: InMemorySpanExporter;
  afterEach(async () => {
    await bindle.shutdown();
  });
  const start = () => {
    mem = new InMemorySpanExporter();
    expect(bindle.init({ apiKey: 'bd_test_' + 'a'.repeat(40), endpoint: 'http://localhost:4318', agent: 'triage', remoteConfig: false }, { exporter: () => mem })).toBe(true);
  };
  const spans = async () => {
    await bindle.flush();
    const byName = new Map(mem.getFinishedSpans().map((x) => [x.name, x]));
    return (n: string) => {
      const x = byName.get(n);
      if (!x) throw new Error(`no span ${n}`);
      return x;
    };
  };

  it('links a resumed run to the original and keeps its session', async () => {
    start();
    expect(bindle.contextToken()).toBeUndefined();
    const token = bindle.withSession('conv-7', () => bindle.step('ask for approval', {}, () => bindle.contextToken()));
    expect(token).toMatch(/^b1\.00-/);
    expect(parseToken(token)!.agent).toBe('triage');

    await bindle.resume(token, { name: 'after approval' }, async () => {
      await bindle.step('refund', { type: 'tool_call' }, async () => 'ok');
    });
    const s = await spans();
    const original = s('ask for approval'), resumed = s('after approval'), child = s('refund');
    expect(resumed.spanContext().traceId).not.toBe(original.spanContext().traceId);
    expect(resumed.parentSpanContext).toBeUndefined();
    expect(resumed.links).toHaveLength(1);
    expect(resumed.links[0]!.context.spanId).toBe(original.spanContext().spanId);
    expect(resumed.attributes['bindle.link.kind']).toBe('resumed');
    expect(resumed.attributes['bindle.link.trace_id']).toBe(original.spanContext().traceId);
    expect(resumed.attributes['bindle.link.from_agent']).toBe('triage');
    expect(resumed.attributes['bindle.session.id']).toBe('conv-7');
    expect(child.attributes['bindle.session.id']).toBe('conv-7');
    expect(child.spanContext().traceId).toBe(resumed.spanContext().traceId);
  });

  it('joins the original trace with sameTrace, normalizes kinds and tolerates bad tokens', async () => {
    start();
    const token = bindle.step('caller', {}, () => bindle.contextToken());
    bindle.resume(token, { name: 'worker', kind: 'handoff', sameTrace: true }, () => {});
    bindle.resume(token, { name: 'odd', kind: 'bogus' as bindle.LinkKind }, () => {});
    bindle.resume('nope', { name: 'unlinked' }, () => {});
    expect(() => bindle.resume(undefined, { name: 'fails' }, () => { throw new Error('boom'); })).toThrow('boom');
    const s = await spans();
    expect(s('worker').spanContext().traceId).toBe(s('caller').spanContext().traceId);
    expect(s('worker').parentSpanContext?.spanId).toBe(s('caller').spanContext().spanId);
    expect(s('worker').attributes['bindle.link.kind']).toBe('handoff');
    expect(s('odd').attributes['bindle.link.kind']).toBe('resumed');
    expect(s('unlinked').links).toHaveLength(0);
    expect(s('fails').status.code).toBe(2);
  });

  it('carries the trace context through MCP params._meta', async () => {
    start();
    expect(bindle.injectMcpMeta({ name: 'lookup' })).toEqual({ name: 'lookup' });
    const params = bindle.step('client call', {}, () => bindle.injectMcpMeta({ name: 'lookup', _meta: { progressToken: 3 } }));
    expect((params._meta as Record<string, unknown>).progressToken).toBe(3);
    bindle.withMcpContext(params, () => bindle.step('server tool', {}, () => {}));
    bindle.withMcpContext({ _meta: { traceparent: 'garbage' } }, () => bindle.step('no context', {}, () => {}));
    const s = await spans();
    expect(s('server tool').spanContext().traceId).toBe(s('client call').spanContext().traceId);
    expect(s('server tool').parentSpanContext?.spanId).toBe(s('client call').spanContext().spanId);
    expect(s('no context').parentSpanContext).toBeUndefined();
  });
});
