// Parsing for the SDK settings form. Pure; unit-tested. The API validates
// patterns authoritatively (RE2, no nested repetition); this only shapes input.

export interface Pattern {
  name: string;
  pattern: string;
}

export interface SDKConfig {
  version: number;
  sample_rate: number;
  capture_content: boolean;
  redact_patterns: Pattern[];
  poll_interval_seconds?: number;
  updated_at?: string;
}

/** "NAME=regex" per line; blank lines and # comments ignored. */
export function parsePatterns(text: string): { patterns: Pattern[]; error?: string } {
  const patterns: Pattern[] = [];
  const lines = text.split(/\r?\n/);
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]!.trim();
    if (!line || line.startsWith('#')) continue;
    const eq = line.indexOf('=');
    if (eq <= 0) return { patterns, error: `Line ${i + 1}: use NAME=regex` };
    const name = line.slice(0, eq).trim().toUpperCase();
    const pattern = line.slice(eq + 1).trim();
    if (!/^[A-Z][A-Z0-9_]{0,31}$/.test(name)) return { patterns, error: `Line ${i + 1}: names use A-Z, 0-9 and _ (max 32)` };
    if (!pattern) return { patterns, error: `Line ${i + 1}: the pattern is empty` };
    patterns.push({ name, pattern });
  }
  return { patterns };
}

export function formatPatterns(patterns: Pattern[]): string {
  return patterns.map((p) => `${p.name}=${p.pattern}`).join('\n');
}

/** Percent text (0-100, decimals allowed) to a 0-1 rate, or null. */
export function parsePercent(v: unknown): number | null {
  if (typeof v !== 'string' || v.trim() === '') return null;
  const n = Number(v);
  if (!Number.isFinite(n) || n < 0 || n > 100) return null;
  return Math.round(n * 1000) / 100000;
}
