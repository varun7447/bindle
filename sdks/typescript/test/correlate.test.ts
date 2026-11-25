import { readFileSync } from 'node:fs';
import { InMemorySpanExporter } from '@opentelemetry/sdk-trace';
import { afterEach, describe, expect, it } from 'vitest';
import * as bindle from '../src/index.js';
import { digest, MAX_KEYS, MAX_VALUE_BYTES, validKey, validValue } from '../src/correlate.js';

const vectors = JSON.parse(readFileSync(new URL('../../python/tests/correlation_vectors.json', import.meta.url), 'utf8')) as {
  max_keys: number;
  max_value_bytes: number;
  digests: { key: string; value: string; digest: string }[];
  valid_keys: string[];
  invalid_keys: string[];
  invalid_values: string[];
};

describe('correlation vectors', () => {
  it('match the Python SDK and the Go backend', () => {
    expect(vectors.max_keys).toBe(MAX_KEYS);
    expect(vectors.max_value_bytes).toBe(MAX_VALUE_BYTES);
    for (const d of vectors.digests) expect(digest(d.key, d.value)).toBe(d.digest);
    for (const k of vectors.valid_keys) expect(validKey(k)).toBe(true);
    for (const k of vectors.invalid_keys) expect(validKey(k)).toBe(false);
    for (const v of vectors.invalid_values) expect(validValue(v)).toBe(false);
  });
});

describe('correlate', () => {
  let mem: InMemorySpanExporter;
  afterEach(async () => {
    await bindle.shutdown();
  });
  const start = () => {
    mem = new InMemorySpanExporter();
    expect(bindle.init({ apiKey: 'bd_test_' + 'a'.repeat(40), endpoint: 'http://localhost:4318', agent: 'support', remoteConfig: false }, { exporter: () => mem })).toBe(true);
  };

  it('sends only a digest, keeps at most 8 keys and never throws', async () => {
    start();
    expect(bindle.correlate('ticket_id', 'T-123')).toBe(false); // outside a run
    const results = bindle.step('run', { type: 'agent' }, () => {
      const r = [bindle.correlate('ticket_id', 'T-123'), bindle.correlate('order', 981)];
      for (let i = 0; i < 8; i++) r.push(bindle.correlate(`k${i}`, 'v'));
      r.push(bindle.correlate('Bad', 'x'), bindle.correlate('ticket_id', ''), bindle.correlate('ticket_id', 'x'.repeat(257)));
      r.push(bindle.correlate('ticket_id', null as unknown as string), bindle.correlate('order', 1.5));
      return r;
    });
    expect(results).toEqual([true, true, ...Array(6).fill(true), false, false, false, false, false, false, false]);
    await bindle.flush();
    const attrs = mem.getFinishedSpans()[0]!.attributes;
    expect(attrs['bindle.correlation.ticket_id']).toBe(digest('ticket_id', 'T-123'));
    expect(attrs['bindle.correlation.order']).toBe(digest('order', '981'));
    expect(Object.keys(attrs).filter((k) => k.startsWith('bindle.correlation.'))).toHaveLength(8);
    expect(JSON.stringify(attrs)).not.toContain('T-123');
  });
});
