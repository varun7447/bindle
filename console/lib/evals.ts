// Evals: checks, datasets and results. Pure; unit-tested.

export interface Scorer {
  id: string;
  name: string;
  kind: string;
  agent: string;
  value: string;
  number: number;
  case_sensitive: boolean;
  enabled: boolean;
  created_at: string;
}

export interface Tally {
  scorer_id: string;
  name: string;
  passed: number;
  failed: number;
  skipped: number;
}

export interface EvalRun {
  id: string;
  dataset_id: string;
  items: number;
  tallies: Tally[];
  failures: { trace_id: string; scorer_id: string; scorer: string; detail: string }[];
  created_at: string;
}

export interface Dataset {
  id: string;
  name: string;
  description: string;
  items: number;
  created_at: string;
  last_eval?: EvalRun;
}

export interface DatasetItem {
  trace_id: string;
  agent: string;
  version: string;
  input: string;
  output: string;
  tools: string[];
  steps: number;
  cost_usd: number;
  cost_known: boolean;
  duration_ms: number;
  failed: boolean;
}

export interface VersionScore {
  version: string;
  name: string;
  count: number;
  passed: number;
  failed: number;
  average: number;
  valued: number;
}

export interface AgentChecks {
  window: string;
  agent: string;
  versions: { version: string; runs_checked: number; checks: Tally[] }[];
  scores: VersionScore[];
}

export interface KindInfo {
  label: string;
  input: 'text' | 'pattern' | 'tool' | 'number' | 'none';
  unit?: string;
  describe: (s: Pick<Scorer, 'value' | 'number'>) => string;
}

export const KINDS: Record<string, KindInfo> = {
  contains: { label: 'Output contains', input: 'text', describe: (s) => `output contains “${s.value}”` },
  not_contains: { label: "Output doesn't contain", input: 'text', describe: (s) => `output doesn't contain “${s.value}”` },
  regex: { label: 'Output matches a pattern', input: 'pattern', describe: (s) => `output matches /${s.value}/` },
  json_valid: { label: 'Output is valid JSON', input: 'none', describe: () => 'output is valid JSON' },
  tool_called: { label: 'Tool was called', input: 'tool', describe: (s) => `${s.value} was called` },
  tool_not_called: { label: "Tool wasn't called", input: 'tool', describe: (s) => `${s.value} wasn't called` },
  max_steps: { label: 'At most N steps', input: 'number', unit: 'steps', describe: (s) => `at most ${s.number} steps` },
  max_cost_usd: { label: 'Costs at most', input: 'number', unit: 'USD', describe: (s) => `costs at most $${s.number}` },
  max_duration_ms: { label: 'Takes at most', input: 'number', unit: 'ms', describe: (s) => `takes at most ${s.number} ms` },
  run_succeeded: { label: 'No step failed', input: 'none', describe: () => 'no step failed' },
};

export function describeScorer(s: Pick<Scorer, 'kind' | 'value' | 'number' | 'agent'>): string {
  const k = KINDS[s.kind];
  const what = k ? k.describe(s) : s.kind;
  return s.agent ? `${what} (${s.agent} only)` : what;
}

/** Pass rate for a tally, or null when nothing was graded. */
export function passRate(t: Pick<Tally, 'passed' | 'failed'>): number | null {
  const n = t.passed + t.failed;
  return n ? t.passed / n : null;
}

/** Overall pass rate across tallies (every graded result counts once). */
export function overall(tallies: Tally[]): number | null {
  return passRate(tallies.reduce((a, t) => ({ passed: a.passed + t.passed, failed: a.failed + t.failed }), { passed: 0, failed: 0 }));
}

/** Rows for the checks-by-version table: one row per check, one cell per version. */
export function checkMatrix(data: AgentChecks): { versions: string[]; rows: { name: string; cells: (Tally | undefined)[] }[] } {
  const versions = data.versions.map((v) => v.version);
  const names = [...new Set(data.versions.flatMap((v) => v.checks.map((c) => c.name)))];
  const rows = names.map((name) => ({ name, cells: data.versions.map((v) => v.checks.find((c) => c.name === name)) }));
  return { versions, rows };
}

export function parseScorerForm(f: { name: string; kind: string; agent: string; value: string; number: string; case_sensitive: boolean }): {
  scorer?: Omit<Scorer, 'id' | 'created_at' | 'enabled'>;
  error?: string;
} {
  const k = KINDS[f.kind];
  if (!k) return { error: 'Choose what to check.' };
  const name = f.name.trim();
  if (!name || name.length > 80) return { error: 'Give the check a name (up to 80 characters).' };
  let value = '';
  let number = 0;
  if (k.input === 'text' || k.input === 'pattern' || k.input === 'tool') {
    value = f.value;
    if (!value.trim()) return { error: k.input === 'tool' ? 'Enter the tool name.' : k.input === 'pattern' ? 'Enter a pattern.' : 'Enter the text to look for.' };
    if (value.length > 500) return { error: 'The value is longer than 500 characters.' };
    if (k.input === 'tool') value = value.trim();
  }
  if (k.input === 'number') {
    number = Number(f.number.trim().replace(/^\$/, ''));
    if (!Number.isFinite(number) || number <= 0) return { error: `Enter a limit greater than 0 (${k.unit}).` };
  }
  return { scorer: { name, kind: f.kind, agent: f.agent.trim(), value, number, case_sensitive: f.case_sensitive } };
}
