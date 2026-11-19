// Agent limits form logic. Pure; unit-tested. The API validates
// authoritatively; this shapes and checks input for clear messages.

export interface AgentLimit {
  agent: string;
  paused: boolean;
  max_steps_per_run: number;
  max_tokens_per_run: number;
  daily_budget_usd: number;
  pause_over_budget: boolean;
  /** Same tool with the same arguments more than this many times in one run (0: no limit). */
  max_repeat_calls?: number;
}

export interface LimitsResponse {
  version: number;
  limits: AgentLimit[];
  over_budget: string[];
  spend_today?: Record<string, number>;
  spend_since?: string;
}

/** One editable row: numbers as typed. */
export interface LimitRow {
  agent: string;
  paused: boolean;
  maxSteps: string;
  maxTokens: string;
  budget: string;
  pauseOverBudget: boolean;
  maxRepeats: string;
}

export const MAX_LIMITS = 50;

export function toRow(l: AgentLimit): LimitRow {
  const n = (v: number) => (v ? String(v) : '');
  return {
    agent: l.agent,
    paused: l.paused,
    maxSteps: n(l.max_steps_per_run),
    maxTokens: n(l.max_tokens_per_run),
    budget: n(l.daily_budget_usd),
    pauseOverBudget: l.pause_over_budget,
    maxRepeats: n(l.max_repeat_calls ?? 0),
  };
}

export const emptyRow = (agent = ''): LimitRow => ({ agent, paused: false, maxSteps: '', maxTokens: '', budget: '', pauseOverBudget: false, maxRepeats: '' });

function whole(v: string, what: string, max: number): number | string {
  const t = v.trim();
  if (!t) return 0;
  const n = Number(t);
  if (!Number.isInteger(n) || n < 0 || n > max) return `${what} must be a whole number from 0 to ${max.toLocaleString('en-US')}`;
  return n;
}

/** Rows to API limits. Rows with no agent and nothing set are dropped. */
export function parseRows(rows: LimitRow[]): { limits: AgentLimit[]; error?: string } {
  const limits: AgentLimit[] = [];
  const seen = new Set<string>();
  for (const [i, r] of rows.entries()) {
    const agent = r.agent.trim();
    const blank = !r.paused && !r.pauseOverBudget && !r.maxSteps.trim() && !r.maxTokens.trim() && !r.budget.trim() && !(r.maxRepeats ?? '').trim();
    if (!agent && blank) continue;
    const at = `Row ${i + 1}`;
    if (!agent) return { limits, error: `${at}: enter an agent name, or * for all agents` };
    if (agent.length > 128) return { limits, error: `${at}: the agent name is too long` };
    if (seen.has(agent)) return { limits, error: `${at}: ${agent} is listed twice` };
    seen.add(agent);
    const steps = whole(r.maxSteps, 'Max steps', 1_000_000);
    if (typeof steps === 'string') return { limits, error: `${at}: ${steps}` };
    const tokens = whole(r.maxTokens, 'Max tokens', 1_000_000_000_000);
    if (typeof tokens === 'string') return { limits, error: `${at}: ${tokens}` };
    const repeats = whole(r.maxRepeats ?? '', 'Max repeated calls', 10_000);
    if (typeof repeats === 'string') return { limits, error: `${at}: ${repeats}` };
    const b = r.budget.trim() ? Number(r.budget.trim().replace(/^\$/, '')) : 0;
    if (!Number.isFinite(b) || b < 0 || b > 1_000_000) return { limits, error: `${at}: the daily budget must be between $0 and $1,000,000` };
    if (r.pauseOverBudget && b === 0) return { limits, error: `${at}: set a daily budget to pause over it` };
    limits.push({
      agent,
      paused: r.paused,
      max_steps_per_run: steps,
      max_tokens_per_run: tokens,
      daily_budget_usd: b,
      pause_over_budget: r.pauseOverBudget,
      max_repeat_calls: repeats,
    });
  }
  if (limits.length > MAX_LIMITS) return { limits, error: `At most ${MAX_LIMITS} limits` };
  return { limits };
}

/** Today's state for an agent row, for display. */
export function rowState(l: Pick<AgentLimit, 'agent' | 'paused' | 'daily_budget_usd' | 'pause_over_budget'>, res: Pick<LimitsResponse, 'over_budget' | 'spend_today'>):
  { label: string; tone: 'ok' | 'warning' | 'critical' } {
  if (l.paused) return { label: 'Paused', tone: 'critical' };
  if (l.agent !== '*' && res.over_budget.includes(l.agent)) {
    return l.pause_over_budget ? { label: 'Paused: over budget', tone: 'critical' } : { label: 'Over budget', tone: 'warning' };
  }
  if (l.agent === '*') {
    const over = res.over_budget.length;
    if (over && l.daily_budget_usd) return { label: `${over} agent${over === 1 ? '' : 's'} over budget`, tone: 'warning' };
  }
  return { label: 'Running', tone: 'ok' };
}
