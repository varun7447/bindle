// Correlation keys: link a run to business events reported later, without
// the run's trace ID. Only a digest of the key and value leaves the process;
// Bindle binds it to the workspace before storing it and never sees the raw
// value. Same rules and digests as the Python SDK (correlation_vectors.json).
import { createHash } from 'node:crypto';

export const ATTR_PREFIX = 'bindle.correlation.';
export const MAX_KEYS = 8;
export const MAX_VALUE_BYTES = 256;
const KEY_RE = /^[a-z][a-z0-9_]{0,31}$/;
const MAX_TRACKED_RUNS = 1024;

export function validKey(key: unknown): key is string {
  return typeof key === 'string' && KEY_RE.test(key);
}

export function validValue(value: unknown): value is string {
  if (typeof value !== 'string') return false;
  const n = Buffer.byteLength(value, 'utf8');
  return n > 0 && n <= MAX_VALUE_BYTES;
}

export function digest(key: string, value: string): string {
  return 'v1:' + createHash('sha256').update(`bindle.correlation.v1\u0000${key}\u0000${value}`, 'utf8').digest('hex');
}

/** [attribute name, digest] for a valid key and value; integers are correlated as their decimal text. */
export function attribute(key: unknown, value: unknown): [string, string] | undefined {
  const v = typeof value === 'number' && Number.isSafeInteger(value) ? String(value) : value;
  if (!validKey(key) || !validValue(v)) return undefined;
  return [ATTR_PREFIX + key, digest(key, v)];
}

/** Keys already set per run, so a run carries at most MAX_KEYS. */
export class RunKeys {
  private runs = new Map<string, Set<string>>();

  admit(traceId: string, key: string): boolean {
    let keys = this.runs.get(traceId);
    if (!keys) {
      keys = new Set();
      this.runs.set(traceId, keys);
      while (this.runs.size > MAX_TRACKED_RUNS) {
        const oldest = this.runs.keys().next().value;
        if (oldest === undefined) break;
        this.runs.delete(oldest);
      }
    }
    if (keys.has(key)) return true;
    if (keys.size >= MAX_KEYS) return false;
    keys.add(key);
    return true;
  }
}
