import type { APIKey } from './types';

export type KeyStatus = 'active' | 'rotating' | 'expired' | 'revoked';

/** Display status of a key at time now. Pure; unit-tested. */
export function keyStatus(k: APIKey, now = Date.now()): KeyStatus {
  if (k.revoked_at) return 'revoked';
  if (k.expires_at) return Date.parse(k.expires_at) <= now ? 'expired' : 'rotating';
  return 'active';
}

export const GRACE_CHOICES = [
  { hours: 0, label: 'Immediately' },
  { hours: 1, label: 'After 1 hour' },
  { hours: 24, label: 'After 24 hours' },
  { hours: 168, label: 'After 7 days' },
] as const;

export function validGrace(v: unknown): number | null {
  // Missing or blank must not become 0 ("revoke the old key now").
  if ((typeof v !== 'string' && typeof v !== 'number') || String(v).trim() === '') return null;
  const n = Number(v);
  return GRACE_CHOICES.some((g) => g.hours === n) ? n : null;
}
