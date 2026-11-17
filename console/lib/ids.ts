// IDs are validated before they are put in an API path, so a crafted value
// such as "../overview" can never reach a different endpoint.

export function isTraceId(v: unknown): v is string {
  return typeof v === 'string' && /^[0-9a-f]{32}$/.test(v);
}

export function isStepId(v: unknown): v is string {
  return typeof v === 'string' && /^[0-9a-f]{16}$/.test(v);
}

/** First value of a search param, or undefined. */
export function one(v: string | string[] | undefined): string | undefined {
  return Array.isArray(v) ? v[0] : v;
}

export const WINDOWS = ['24h', '7d', '30d'] as const;
export type Window = (typeof WINDOWS)[number];

export function asWindow(v: string | undefined): Window {
  return (WINDOWS as readonly string[]).includes(v ?? '') ? (v as Window) : '24h';
}

export const WINDOW_MS: Record<Window, number> = {
  '24h': 24 * 3600_000,
  '7d': 7 * 24 * 3600_000,
  '30d': 30 * 24 * 3600_000,
};

/**
 * Validates a "since" timestamp for the first-trace poll: must parse, be in
 * the past, and be within the API's 30-day range. Returns ISO or null.
 */
export function validSince(v: string | null | undefined, now = Date.now()): string | null {
  if (!v) return null;
  const t = Date.parse(v);
  if (Number.isNaN(t) || t > now || now - t > WINDOW_MS['30d']) return null;
  return new Date(t).toISOString();
}
