// Model price overrides form logic. Pure; unit-tested.

export interface PriceOverride {
  provider: string;
  model: string;
  input_per_mtok: number;
  output_per_mtok: number;
  cached_input_per_mtok: number | null;
  note: string;
}

export interface PricesResponse {
  version: number;
  prices: PriceOverride[];
}

export interface ModelUsage {
  provider: string;
  model: string;
  calls: number;
  unpriced_calls: number;
  cost_usd: number;
  input_tokens: number;
  output_tokens: number;
  cached_tokens: number;
}

export interface AWSPrice {
  model: string;
  provider: string;
  tier: string;
  input_per_mtok: number | null;
  output_per_mtok: number | null;
  cache_read_per_mtok: number | null;
  cache_write_per_mtok: number | null;
  region: string;
  source: string;
  as_of: string;
}

/** A workspace price row from an AWS list price; the user adds the Bedrock model ID. */
export function fromAWS(p: AWSPrice): PriceRow {
  const n = (v: number | null) => (v === null ? '' : String(v));
  const tier = p.tier || 'standard';
  return {
    provider: '',
    model: '',
    input: n(p.input_per_mtok),
    output: n(p.output_per_mtok),
    cached: n(p.cache_read_per_mtok),
    note: `AWS list price: ${p.provider} ${p.model}, ${tier}, ${p.region}, as of ${p.as_of}`.slice(0, 200),
  };
}

export interface PriceRow {
  provider: string;
  model: string;
  input: string;
  output: string;
  cached: string;
  note: string;
}

export const MAX_PRICES = 100;

const n = (v: number | null) => (v === null || v === undefined ? '' : String(v));

export const toRow = (p: PriceOverride): PriceRow => ({
  provider: p.provider, model: p.model, input: n(p.input_per_mtok), output: n(p.output_per_mtok), cached: n(p.cached_input_per_mtok), note: p.note,
});

export const emptyRow = (provider = '', model = ''): PriceRow => ({ provider, model, input: '', output: '', cached: '', note: '' });

function price(v: string, what: string, required: boolean): number | null | string {
  const t = v.trim().replace(/^\$/, '');
  if (!t) return required ? `enter the ${what} price` : null;
  const x = Number(t);
  if (!Number.isFinite(x) || x < 0 || x > 10_000) return `the ${what} price must be between 0 and 10,000 USD per million tokens`;
  return x;
}

export function parseRows(rows: PriceRow[]): { prices: PriceOverride[]; error?: string } {
  const prices: PriceOverride[] = [];
  const seen = new Set<string>();
  for (const [i, r] of rows.entries()) {
    const model = r.model.trim();
    const provider = r.provider.trim().toLowerCase();
    if (!model && !provider && !r.input.trim() && !r.output.trim() && !r.cached.trim() && !r.note.trim()) continue;
    const at = `Row ${i + 1}`;
    if (!model) return { prices, error: `${at}: enter a model ID` };
    if (/\s/.test(model) || model.length > 200) return { prices, error: `${at}: model IDs have no spaces (max 200 characters)` };
    if (provider && !/^[a-z0-9][a-z0-9._-]{0,63}$/.test(provider)) return { prices, error: `${at}: provider uses lowercase letters, digits, . _ or -` };
    const key = `${provider}/${model.toLowerCase()}`;
    if (seen.has(key)) return { prices, error: `${at}: ${model} is listed twice` };
    seen.add(key);
    const input = price(r.input, 'input', true);
    if (typeof input === 'string') return { prices, error: `${at}: ${input}` };
    const output = price(r.output, 'output', true);
    if (typeof output === 'string') return { prices, error: `${at}: ${output}` };
    const cached = price(r.cached, 'cached input', false);
    if (typeof cached === 'string') return { prices, error: `${at}: ${cached}` };
    if (r.note.length > 200) return { prices, error: `${at}: the note is longer than 200 characters` };
    prices.push({ provider, model, input_per_mtok: input!, output_per_mtok: output!, cached_input_per_mtok: cached, note: r.note.trim() });
  }
  if (prices.length > MAX_PRICES) return { prices, error: `At most ${MAX_PRICES} model prices` };
  return { prices };
}

/** Whether an override covers a model (same rule as the server, simplified for display). */
export function covers(p: Pick<PriceOverride, 'provider' | 'model'>, m: Pick<ModelUsage, 'provider' | 'model'>): boolean {
  if (p.provider && p.provider !== m.provider.toLowerCase()) return false;
  const key = p.model.toLowerCase();
  const model = m.model.toLowerCase();
  if (model === key) return true;
  return model.startsWith(key) && /^(?:[-@](?:\d{4}-?\d{2}-?\d{2}|\d{8}|v\d+(?::\d+)?|latest|preview)\b)/.test(model.slice(key.length));
}
