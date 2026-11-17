// First-pass redaction inside the customer's process, before data leaves it.
// Mirrors sdks/python/src/bindle/_redact.py and backend/internal/redact;
// ingest redacts again.

import type { AttributeValue, Attributes } from '@opentelemetry/api';

type Rule = { label: string; re: RegExp; check?: (s: string) => boolean };

function luhnOk(s: string): boolean {
  const digits = [...s].filter((c) => c >= '0' && c <= '9').map(Number);
  if (digits.length < 13 || digits.length > 19) return false;
  let total = 0;
  digits.reverse().forEach((d, i) => {
    if (i % 2 === 1) {
      d *= 2;
      if (d > 9) d -= 9;
    }
    total += d;
  });
  return total % 10 === 0;
}

export const DEFAULT_RULES: readonly Rule[] = [
  { label: 'PRIVATE_KEY', re: /-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----/g },
  { label: 'SECRET', re: /\bAKIA[0-9A-Z]{16}\b/g },
  { label: 'SECRET', re: /\bsk-[A-Za-z0-9_-]{20,}/g },
  { label: 'SECRET', re: /\bgh[pousr]_[A-Za-z0-9]{36,}\b/g },
  { label: 'SECRET', re: /\bxox[baprs]-[A-Za-z0-9-]{10,}/g },
  { label: 'SECRET', re: /\bbearer\s+[A-Za-z0-9._~+/-]{20,}=*/gi },
  { label: 'CARD', re: /\b(?:\d[ -]?){12,18}\d\b/g, check: luhnOk },
  { label: 'SSN', re: /\b\d{3}-\d{2}-\d{4}\b/g },
  { label: 'EMAIL', re: /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g },
  { label: 'PHONE', re: /(?:\+?1[-. ]?)?\(?\b\d{3}\)?[-. ]?\d{3}[-. ]?\d{4}\b/g },
];

const NAME_RE = /^[A-Z][A-Z0-9_]{0,31}$/;

/** Replaces sensitive substrings with [REDACTED:<LABEL>]. */
export class Redactor {
  private readonly rules: Rule[];

  constructor(opts: { builtIn?: boolean; extra?: Record<string, string>; onInvalid?: (name: string) => void } = {}) {
    this.rules = opts.builtIn === false ? [] : [...DEFAULT_RULES];
    for (const [name, pattern] of Object.entries(opts.extra ?? {})) {
      const label = name.toUpperCase();
      try {
        if (!NAME_RE.test(label) || pattern.length === 0 || pattern.length > 200) throw new Error('invalid');
        this.rules.push({ label, re: new RegExp(pattern, 'g') });
      } catch {
        // A bad pattern must not disable tracing; ingest redacts again.
        opts.onInvalid?.(name);
      }
    }
  }

  apply(value: string): string {
    if (!value) return value;
    for (const r of this.rules) {
      const marker = `[REDACTED:${r.label}]`;
      r.re.lastIndex = 0;
      value = value.replace(r.re, (m) => (r.check && !r.check(m) ? m : marker));
    }
    return value;
  }

  applyValue(v: AttributeValue | undefined): AttributeValue | undefined {
    if (typeof v === 'string') return this.apply(v);
    if (Array.isArray(v)) return v.map((x) => (typeof x === 'string' ? this.apply(x) : x)) as AttributeValue;
    return v;
  }

  applyAttributes(attrs: Attributes | undefined): Attributes {
    const out: Attributes = {};
    for (const [k, v] of Object.entries(attrs ?? {})) out[k] = this.applyValue(v);
    return out;
  }
}
