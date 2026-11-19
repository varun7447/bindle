import { createHash } from 'node:crypto';
import { context, trace, type Context } from '@opentelemetry/api';
import type { ReadableSpan, Span, SpanProcessor } from '@opentelemetry/sdk-trace';

// Agent limits set in the Bindle console: a kill switch, per-run caps on
// steps, tokens and repeated tool calls (a loop guard), daily budgets, and
// runs stopped from the console. Bindle never stops an agent behind its
// back: limits take effect where the agent's code calls checkLimits() (for
// example at the top of each loop iteration or before a tool call).

export const ANY_AGENT = '*';
const MAX_TRACES = 2048;
const MAX_CALL_KEYS = 1024; // distinct (tool, arguments) pairs tracked per run

export interface Limit {
  agent: string;
  paused: boolean;
  maxStepsPerRun: number;
  maxTokensPerRun: number;
  dailyBudgetUsd: number;
  pauseOverBudget: boolean;
  maxRepeatCalls: number;
}

export type LimitReason = 'paused' | 'over_budget' | 'stopped' | 'max_steps_per_run' | 'max_tokens_per_run' | 'max_repeat_calls';

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
      maxRepeatCalls: Math.floor(num(d.max_repeat_calls)),
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

/**
 * Arguments as canonical JSON (sorted keys), so {"a":1,"b":2} and
 * {"b":2,"a":1} are the same call. Text that is not JSON is kept as is.
 */
export function canonicalArgs(raw: unknown): string {
  let v: unknown = raw;
  if (typeof raw === 'string') {
    try {
      v = JSON.parse(raw);
    } catch {
      return raw;
    }
  }
  const sort = (x: unknown): unknown => {
    if (Array.isArray(x)) return x.map(sort);
    if (x && typeof x === 'object') {
      const o = x as Record<string, unknown>;
      return Object.fromEntries(Object.keys(o).sort().map((k) => [k, sort(o[k])]));
    }
    return x;
  };
  try {
    return JSON.stringify(sort(v)) ?? String(raw);
  } catch {
    return String(raw);
  }
}

function toolCall(a: Record<string, unknown>): [string, string] | undefined {
  const op = String(a['gen_ai.operation.name'] ?? '').toLowerCase();
  const kind = String(a['openinference.span.kind'] ?? '').toUpperCase();
  const name = a['gen_ai.tool.name'] ?? a['tool.name'];
  if (!name || (op !== 'execute_tool' && kind !== 'TOOL')) return undefined;
  const raw = a['gen_ai.tool.call.arguments'] ?? a['tool.parameters'] ?? a['input.value'] ?? '';
  return [String(name), canonicalArgs(raw)];
}

interface RunEntry {
  steps: number;
  tokens: number;
  calls: Map<string, number>;
  maxRepeats: number;
  repeatedTool: string;
}

/** Counts steps (spans started), LLM tokens and repeated tool calls per trace. Bounded. */
export class RunCounter implements SpanProcessor {
  private readonly runs = new Map<string, RunEntry>();
  constructor(private readonly max = MAX_TRACES) {}

  private entry(traceId: string): RunEntry {
    let e = this.runs.get(traceId);
    if (e) {
      this.runs.delete(traceId); // re-insert: most recent last
    } else {
      e = { steps: 0, tokens: 0, calls: new Map(), maxRepeats: 0, repeatedTool: '' };
      if (this.runs.size >= this.max) this.runs.delete(this.runs.keys().next().value as string);
    }
    this.runs.set(traceId, e);
    return e;
  }

  onStart(span: Span, _ctx: Context): void {
    try {
      this.entry(span.spanContext().traceId).steps += 1;
    } catch {
      /* never break the host app */
    }
  }

  onEnd(span: ReadableSpan): void {
    try {
      const a = (span.attributes ?? {}) as Record<string, unknown>;
      if (isLlm(a)) {
        const n = tokens(a);
        if (n) this.entry(span.spanContext().traceId).tokens += n;
        return;
      }
      const call = toolCall(a);
      if (!call) return;
      const key = createHash('sha256').update(`${call[0]}\u0000${call[1]}`).digest('hex').slice(0, 32);
      const e = this.entry(span.spanContext().traceId);
      if (!e.calls.has(key) && e.calls.size >= MAX_CALL_KEYS) return;
      const n = (e.calls.get(key) ?? 0) + 1;
      e.calls.set(key, n);
      if (n > e.maxRepeats) {
        e.maxRepeats = n;
        e.repeatedTool = call[0];
      }
    } catch {
      /* never break the host app */
    }
  }

  usage(traceId: string): [number, number] {
    const e = this.runs.get(traceId);
    return e ? [e.steps, e.tokens] : [0, 0];
  }

  /** The most times one tool was called with the same arguments in the run, and that tool. */
  repeats(traceId: string): [number, string] {
    const e = this.runs.get(traceId);
    return e ? [e.maxRepeats, e.repeatedTool] : [0, ''];
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
  maxRepeatCalls: number;
  runSteps: number;
  runTokens: number;
  runMaxRepeats: number;
  runRepeatedTool: string;
  stopped: boolean;
}

/** Holds the current limits and checks them for the active run. */
export class Guard {
  private limits: Limit[] = [];
  private overBudget = new Set<string>();
  private stopped = new Set<string>();
  constructor(
    readonly agent: string,
    readonly counter: RunCounter,
  ) {}

  update(limits: Limit[], overBudget: string[], stoppedRuns: string[] = []): void {
    this.limits = limits;
    this.overBudget = new Set(overBudget);
    this.stopped = new Set(stoppedRuns);
  }

  status(): LimitsStatus {
    const l = limitFor(this.limits, this.agent);
    const sc = trace.getSpan(context.active())?.spanContext();
    const valid = Boolean(sc && trace.isSpanContextValid(sc));
    const [steps, toks] = valid && sc ? this.counter.usage(sc.traceId) : [0, 0];
    const [repeats, repeatedTool] = valid && sc ? this.counter.repeats(sc.traceId) : [0, ''];
    return {
      agent: this.agent,
      paused: Boolean(l?.paused),
      overBudget: this.overBudget.has(this.agent),
      pauseOverBudget: Boolean(l?.pauseOverBudget),
      maxStepsPerRun: l?.maxStepsPerRun ?? 0,
      maxTokensPerRun: l?.maxTokensPerRun ?? 0,
      maxRepeatCalls: l?.maxRepeatCalls ?? 0,
      runSteps: steps,
      runTokens: toks,
      runMaxRepeats: repeats,
      runRepeatedTool: repeatedTool,
      stopped: valid && sc ? this.stopped.has(sc.traceId) : false,
    };
  }

  check(): void {
    const s = this.status();
    const who = this.agent || 'this agent';
    if (s.paused) throw new LimitExceededError('paused', this.agent, `${who} is paused in Bindle`);
    if (s.overBudget && s.pauseOverBudget) {
      throw new LimitExceededError('over_budget', this.agent, `${who} is over its daily budget in Bindle (paused until 00:00 UTC)`);
    }
    if (s.stopped) throw new LimitExceededError('stopped', this.agent, 'this run was stopped from the Bindle console');
    if (s.maxStepsPerRun && s.runSteps > s.maxStepsPerRun) {
      throw new LimitExceededError('max_steps_per_run', this.agent, `run used ${s.runSteps} steps, over the limit of ${s.maxStepsPerRun}`, s.maxStepsPerRun, s.runSteps);
    }
    if (s.maxTokensPerRun && s.runTokens > s.maxTokensPerRun) {
      throw new LimitExceededError('max_tokens_per_run', this.agent, `run used ${s.runTokens} tokens, over the limit of ${s.maxTokensPerRun}`, s.maxTokensPerRun, s.runTokens);
    }
    if (s.maxRepeatCalls && s.runMaxRepeats > s.maxRepeatCalls) {
      throw new LimitExceededError(
        'max_repeat_calls',
        this.agent,
        `run called ${s.runRepeatedTool} with the same arguments ${s.runMaxRepeats} times, over the limit of ${s.maxRepeatCalls}`,
        s.maxRepeatCalls,
        s.runMaxRepeats,
      );
    }
  }
}
