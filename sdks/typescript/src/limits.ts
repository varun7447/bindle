import { context, trace, type Context } from '@opentelemetry/api';
import type { ReadableSpan, Span, SpanProcessor } from '@opentelemetry/sdk-trace';

// Agent limits set in the Bindle console: a kill switch, per-run caps on
// steps and tokens, and daily budgets. Bindle never stops an agent behind
// its back: limits take effect where the agent's code calls checkLimits()
// (for example at the top of each loop iteration or before a tool call).

export const ANY_AGENT = '*';
const MAX_TRACES = 2048;

export interface Limit {
  agent: string;
  paused: boolean;
  maxStepsPerRun: number;
  maxTokensPerRun: number;
  dailyBudgetUsd: number;
  pauseOverBudget: boolean;
}

export type LimitReason = 'paused' | 'over_budget' | 'max_steps_per_run' | 'max_tokens_per_run';

export class LimitExceededError extends Error {
  constructor(
    readonly reason: LimitReason,
    readonly agent: string,
    message: string,
    readonly limit = 0,
    readonly value = 0,
  ) {
    super(message);
    this.name = 'LimitExceededError';
  }
}

const num = (v: unknown): number => {
  const n = Number(v ?? 0);
  return Number.isFinite(n) && n > 0 ? n : 0;
};

/** Tolerant parse of the config's "limits" list; bad rows are skipped. */
export function parseLimits(raw: unknown): Limit[] {
  if (!Array.isArray(raw)) return [];
  const out: Limit[] = [];
  for (const r of raw) {
    if (!r || typeof r !== 'object') continue;
    const d = r as Record<string, unknown>;
    if (typeof d.agent !== 'string' || !d.agent) continue;
    out.push({
      agent: d.agent,
      paused: d.paused === true,
      maxStepsPerRun: Math.floor(num(d.max_steps_per_run)),
      maxTokensPerRun: Math.floor(num(d.max_tokens_per_run)),
      dailyBudgetUsd: num(d.daily_budget_usd),
      pauseOverBudget: d.pause_over_budget === true,
    });
  }
  return out;
}

/** The agent's own limit, else the "*" limit. */
export function limitFor(limits: readonly Limit[], agent: string): Limit | undefined {
  let any: Limit | undefined;
  for (const l of limits) {
    if (l.agent === agent && agent !== ANY_AGENT) return l;
    if (l.agent === ANY_AGENT) any = l;
  }
  return any;
}

const LLM_OPS = new Set(['chat', 'text_completion', 'generate_content', 'embeddings']);
const NOT_LLM_OPS = new Set(['invoke_agent', 'create_agent', 'execute_tool', 'invoke_workflow']);
const NOT_LLM_KINDS = new Set(['AGENT', 'CHAIN', 'TOOL', 'RETRIEVER', 'RERANKER', 'GUARDRAIL', 'EVALUATOR']);

function isLlm(a: Record<string, unknown>): boolean {
  const op = String(a['gen_ai.operation.name'] ?? '').toLowerCase();
  const kind = String(a['openinference.span.kind'] ?? '').toUpperCase();
  if (LLM_OPS.has(op) || kind === 'LLM' || kind === 'EMBEDDING') return true;
  if (NOT_LLM_OPS.has(op) || NOT_LLM_KINDS.has(kind)) return false;
  return Boolean(a['gen_ai.request.model'] || a['llm.model_name']);
}

function tokens(a: Record<string, unknown>): number {
  let t = 0;
  for (const k of ['gen_ai.usage.input_tokens', 'gen_ai.usage.output_tokens', 'llm.token_count.prompt', 'llm.token_count.completion']) t += num(a[k]);
  if (t === 0) t = num(a['gen_ai.usage.total_tokens']) || num(a['llm.token_count.total']);
  return t;
}

/** Counts steps (spans started) and LLM tokens per trace. Bounded. */
export class RunCounter implements SpanProcessor {
  private readonly runs = new Map<string, [number, number]>();
  constructor(private readonly max = MAX_TRACES) {}

  private entry(traceId: string): [number, number] {
    let e = this.runs.get(traceId);
    if (e) {
      this.runs.delete(traceId); // re-insert: most recent last
    } else {
      e = [0, 0];
      if (this.runs.size >= this.max) this.runs.delete(this.runs.keys().next().value as string);
    }
    this.runs.set(traceId, e);
    return e;
  }

  onStart(span: Span, _ctx: Context): void {
    try {
      this.entry(span.spanContext().traceId)[0] += 1;
    } catch {
      /* never break the host app */
    }
  }

  onEnd(span: ReadableSpan): void {
    try {
      const a = (span.attributes ?? {}) as Record<string, unknown>;
      if (!isLlm(a)) return;
      const n = tokens(a);
      if (n) this.entry(span.spanContext().traceId)[1] += n;
    } catch {
      /* never break the host app */
    }
  }

  usage(traceId: string): [number, number] {
    const e = this.runs.get(traceId);
    return e ? [e[0], e[1]] : [0, 0];
  }

  size(): number {
    return this.runs.size;
  }

  forceFlush(): Promise<void> {
    return Promise.resolve();
  }

  shutdown(): Promise<void> {
    return Promise.resolve();
  }
}

export interface LimitsStatus {
  agent: string;
  paused: boolean;
  overBudget: boolean;
  pauseOverBudget: boolean;
  maxStepsPerRun: number;
  maxTokensPerRun: number;
  runSteps: number;
  runTokens: number;
}

/** Holds the current limits and checks them for the active run. */
export class Guard {
  private limits: Limit[] = [];
  private overBudget = new Set<string>();
  constructor(
    readonly agent: string,
    readonly counter: RunCounter,
  ) {}

  update(limits: Limit[], overBudget: string[]): void {
    this.limits = limits;
    this.overBudget = new Set(overBudget);
  }

  status(): LimitsStatus {
    const l = limitFor(this.limits, this.agent);
    const sc = trace.getSpan(context.active())?.spanContext();
    const [steps, toks] = sc && trace.isSpanContextValid(sc) ? this.counter.usage(sc.traceId) : [0, 0];
    return {
      agent: this.agent,
      paused: Boolean(l?.paused),
      overBudget: this.overBudget.has(this.agent),
      pauseOverBudget: Boolean(l?.pauseOverBudget),
      maxStepsPerRun: l?.maxStepsPerRun ?? 0,
      maxTokensPerRun: l?.maxTokensPerRun ?? 0,
      runSteps: steps,
      runTokens: toks,
    };
  }

  check(): void {
    const s = this.status();
    const who = this.agent || 'this agent';
    if (s.paused) throw new LimitExceededError('paused', this.agent, `${who} is paused in Bindle`);
    if (s.overBudget && s.pauseOverBudget) {
      throw new LimitExceededError('over_budget', this.agent, `${who} is over its daily budget in Bindle (paused until 00:00 UTC)`);
    }
    if (s.maxStepsPerRun && s.runSteps > s.maxStepsPerRun) {
      throw new LimitExceededError('max_steps_per_run', this.agent, `run used ${s.runSteps} steps, over the limit of ${s.maxStepsPerRun}`, s.maxStepsPerRun, s.runSteps);
    }
    if (s.maxTokensPerRun && s.runTokens > s.maxTokensPerRun) {
      throw new LimitExceededError('max_tokens_per_run', this.agent, `run used ${s.runTokens} tokens, over the limit of ${s.maxTokensPerRun}`, s.maxTokensPerRun, s.runTokens);
    }
  }
}
