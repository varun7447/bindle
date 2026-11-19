// Bindle: observability for AI agents.
//
//   import * as bindle from '@bindle-ai/sdk';
//   bindle.init({ agent: 'support-triage', version: 'v12' });
//
// Every step your agent takes is traced with OpenTelemetry and sent to
// Bindle. Design rules (same as the Python SDK):
//  - One line to start, no config file; everything has a safe default.
//  - Never break the agent: export is async and bounded, errors are
//    swallowed, and a missing API key makes the SDK a no-op with one warning.
//  - Redaction is on by default and runs before data leaves the process.

import { AsyncLocalStorage } from 'node:async_hooks';
import { context, SpanStatusCode, trace, type Attributes, type Span } from '@opentelemetry/api';
import { AsyncLocalStorageContextManager } from '@opentelemetry/context-async-hooks';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-proto';
import { CompressionAlgorithm } from '@opentelemetry/otlp-exporter-base';
import { resourceFromAttributes } from '@opentelemetry/resources';
import { BatchSpanProcessor, ParentBasedSampler, TracerProvider, type SpanProcessor } from '@opentelemetry/sdk-trace';
import { RedactingSpanExporter } from './export.js';
import { Redactor } from './redact.js';
import { Poller, type RemoteSettings } from './remote.js';
import { DynamicRatioSampler } from './sampling.js';

import { Guard, LimitExceededError, type LimitsStatus, RunCounter } from './limits.js';
export { Redactor } from './redact.js';
export { LimitExceededError, type LimitReason, type LimitsStatus } from './limits.js';
export { sampled } from './sampling.js';

export const VERSION = '0.1.0';
export const DEFAULT_ENDPOINT = 'https://ingest.bindle.ai';
const TRACER = 'bindle';
const MAX_CAPTURE = 16 * 1024;

export interface InitOptions {
  /** Ingest key (bd_live_...). Default: BINDLE_API_KEY. */
  apiKey?: string;
  /** Default: BINDLE_ENDPOINT or https://ingest.bindle.ai */
  endpoint?: string;
  agent?: string;
  version?: string;
  environment?: string;
  /** 0-1. Default: BINDLE_SAMPLE_RATE or 1. */
  sampleRate?: number;
  /** Built-in redaction. Default true (BINDLE_REDACT). */
  redact?: boolean;
  /** Send prompts, outputs and reasoning. Default true (BINDLE_CAPTURE_CONTENT). */
  captureContent?: boolean;
  /** Extra NAME -> regex patterns, redacted as [REDACTED:NAME]. */
  redactPatterns?: Record<string, string>;
  /** Poll console settings. Default true (BINDLE_REMOTE_CONFIG). */
  remoteConfig?: boolean;
  debug?: boolean;
}

interface Resolved {
  apiKey: string;
  endpoint: string;
  agent?: string;
  version?: string;
  environment?: string;
  sampleRate: number;
  redact: boolean;
  captureContent: boolean;
  redactPatterns: Record<string, string>;
  remoteConfig: boolean;
  debug: boolean;
}

function envBool(v: string | undefined, def: boolean): boolean {
  if (v === undefined) return def;
  return !['0', 'false', 'no', 'off', ''].includes(v.trim().toLowerCase());
}

function envNum(v: string | undefined, def: number): number {
  const n = v === undefined ? NaN : Number(v);
  return Number.isFinite(n) ? n : def;
}

export function resolveOptions(o: InitOptions = {}, env: NodeJS.ProcessEnv = process.env): Resolved {
  return {
    apiKey: (o.apiKey ?? env.BINDLE_API_KEY ?? '').trim(),
    endpoint: (o.endpoint ?? env.BINDLE_ENDPOINT ?? DEFAULT_ENDPOINT).replace(/\/+$/, ''),
    agent: o.agent ?? env.BINDLE_AGENT,
    version: o.version ?? env.BINDLE_AGENT_VERSION,
    environment: o.environment ?? env.BINDLE_ENVIRONMENT,
    sampleRate: Math.min(Math.max(o.sampleRate ?? envNum(env.BINDLE_SAMPLE_RATE, 1), 0), 1),
    redact: o.redact ?? envBool(env.BINDLE_REDACT, true),
    captureContent: o.captureContent ?? envBool(env.BINDLE_CAPTURE_CONTENT, true),
    redactPatterns: o.redactPatterns ?? {},
    remoteConfig: o.remoteConfig ?? envBool(env.BINDLE_REMOTE_CONFIG, true),
    debug: o.debug ?? envBool(env.BINDLE_DEBUG, false),
  };
}

interface State {
  cfg?: Resolved;
  provider?: TracerProvider;
  sampler?: DynamicRatioSampler;
  exporter?: RedactingSpanExporter;
  poller?: Poller;
  capture: boolean;
  remote?: RemoteSettings;
  guard?: Guard;
}

const state: State = { capture: true };
const session = new AsyncLocalStorage<string | undefined>();

/** Adds bindle.session.id to every span started inside withSession(), framework spans included. */
const sessionTagger: SpanProcessor = {
  onStart(span) {
    try {
      const sid = session.getStore();
      if (sid && span.attributes?.['bindle.session.id'] === undefined) span.setAttribute('bindle.session.id', sid.slice(0, 256));
    } catch {
      /* never break the host app */
    }
  },
  onEnd() {},
  forceFlush: () => Promise.resolve(),
  shutdown: () => Promise.resolve(),
};

function debug(msg: string): void {
  if (state.cfg?.debug) console.debug(msg);
}

function localRedactor(cfg: Resolved, remote: Record<string, string> = {}): Redactor {
  // Remote patterns always add, never remove.
  const extra = { ...(cfg.redact ? cfg.redactPatterns : {}), ...remote };
  return new Redactor({ builtIn: cfg.redact, extra, onInvalid: (n) => console.warn(`bindle: skipping invalid redaction pattern ${n}`) });
}

function buildExporter(cfg: Resolved, exporterFactory?: () => import('@opentelemetry/sdk-trace').SpanExporter): RedactingSpanExporter {
  const inner =
    exporterFactory?.() ??
    new OTLPTraceExporter({
      url: `${cfg.endpoint}/v1/traces`,
      headers: { authorization: `Bearer ${cfg.apiKey}` },
      timeoutMillis: 5000,
      compression: CompressionAlgorithm.GZIP,
    });
  return new RedactingSpanExporter(inner, localRedactor(cfg), cfg.captureContent);
}

function buildProcessor(exporter: RedactingSpanExporter): SpanProcessor {
  // Bounded queue: when full, spans are dropped instead of blocking the agent.
  return new BatchSpanProcessor({ exporter, maxQueueSize: 4096, scheduledDelayMillis: 1000, maxExportBatchSize: 512, exportTimeoutMillis: 5000 });
}

/** Merge remote settings with local ones: the stricter value wins. */
function applyRemote(s: RemoteSettings): void {
  const cfg = state.cfg;
  if (!cfg) return;
  const capture = cfg.captureContent && s.captureContent;
  state.sampler?.setRate(Math.min(cfg.sampleRate, s.sampleRate));
  state.exporter?.update(capture, localRedactor(cfg, s.redactPatterns), cfg.captureContent ? 'workspace' : 'sdk');
  state.capture = capture;
  state.remote = s;
  state.guard?.update(s.limits, s.overBudget);
  debug(`bindle: remote config v${s.version} applied`);
}

function startRemote(cfg: Resolved): void {
  if (!cfg.remoteConfig) return;
  state.poller = new Poller(`${cfg.endpoint}/v1/config`, cfg.apiKey, applyRemote, `bindle-sdk-typescript/${VERSION}`, fetch, debug);
  state.poller.start();
}

/**
 * Start sending traces to Bindle. Returns true if tracing is active.
 * Console settings (sampling, content capture, redaction patterns) are
 * fetched in the background and act as a ceiling: code can only be stricter.
 */
export function init(options: InitOptions = {}, internal: { exporter?: () => import('@opentelemetry/sdk-trace').SpanExporter } = {}): boolean {
  const cfg = resolveOptions(options);
  if (!cfg.apiKey) {
    console.warn('bindle: no API key (set BINDLE_API_KEY); tracing is disabled');
    return false;
  }
  if (state.provider) return true;
  try {
    if (!/^https:\/\//.test(cfg.endpoint) && !/^http:\/\/(localhost|127\.0\.0\.1|\[::1\])(:\d+)?$/.test(cfg.endpoint)) {
      console.warn('bindle: the endpoint must use https (the ingest key is sent with every request); tracing is disabled');
      return false;
    }
    const attrs: Attributes = { 'bindle.sdk.version': VERSION, 'telemetry.sdk.language': 'nodejs' };
    if (cfg.agent) {
      attrs['service.name'] = cfg.agent;
      attrs['bindle.agent.name'] = cfg.agent;
    }
    if (cfg.version) attrs['bindle.agent.version'] = cfg.version;
    if (cfg.environment) attrs['deployment.environment.name'] = cfg.environment;

    state.cfg = cfg;
    state.capture = cfg.captureContent;
    const exporter = buildExporter(cfg, internal.exporter);
    const sampler = new DynamicRatioSampler(cfg.sampleRate);
    const counter = new RunCounter();
    const provider = new TracerProvider({
      resource: resourceFromAttributes(attrs),
      sampler: new ParentBasedSampler({ root: sampler }),
      spanProcessors: [sessionTagger, buildProcessor(exporter), counter],
    });
    // Only if the app hasn't set its own context manager.
    context.setGlobalContextManager(new AsyncLocalStorageContextManager().enable());
    if (!trace.setGlobalTracerProvider(provider)) {
      console.warn('bindle: another OpenTelemetry tracer provider is already registered; add bindle.spanProcessor() to it instead');
      void provider.shutdown();
      state.cfg = undefined;
      return false;
    }
    Object.assign(state, { provider, sampler, exporter, guard: new Guard(cfg.agent ?? '', counter) });
    startRemote(cfg);
    process.once('beforeExit', () => void shutdown());
    debug(`bindle: initialized (endpoint=${cfg.endpoint})`);
    return true;
  } catch (e) {
    console.warn(`bindle: init failed, tracing disabled: ${(e as Error).message}`);
    state.cfg = undefined;
    return false;
  }
}

/**
 * A span processor for apps that already have their own OpenTelemetry
 * tracer provider: pass it to that provider's spanProcessors. The app's
 * sampler applies; ingest still enforces the console's sample rate.
 */
export function spanProcessor(options: InitOptions = {}): SpanProcessor | undefined {
  const cfg = resolveOptions(options);
  if (!cfg.apiKey) {
    console.warn('bindle: no API key (set BINDLE_API_KEY); tracing is disabled');
    return undefined;
  }
  state.cfg ??= cfg;
  const exporter = buildExporter(cfg);
  state.exporter ??= exporter;
  const counter = new RunCounter();
  state.guard ??= new Guard(cfg.agent ?? '', counter);
  if (!state.poller) startRemote(cfg);
  const batch = buildProcessor(exporter);
  // One processor for the app's provider: export plus run counting.
  return {
    onStart: (span, ctx) => {
      sessionTagger.onStart(span, ctx);
      counter.onStart(span, ctx);
      batch.onStart(span, ctx);
    },
    onEnd: (span) => {
      counter.onEnd(span);
      batch.onEnd(span);
    },
    forceFlush: () => batch.forceFlush(),
    shutdown: () => batch.shutdown(),
  };
}

/**
 * Throw LimitExceededError if this agent is paused in the Bindle console,
 * over its daily budget (when its limit pauses it), or the current run has
 * more steps or tokens than its limit allows. Call it where stopping is
 * safe: at the top of a loop iteration, before a tool or LLM call. Does
 * nothing if Bindle isn't initialized or no limits are set.
 */
export function checkLimits(): void {
  const g = state.guard;
  if (!g) return;
  try {
    g.check();
  } catch (e) {
    if (e instanceof LimitExceededError) throw e;
    debug(`bindle: limit check failed: ${(e as Error).message}`); // a bug here must not stop the agent
  }
}

/** The limits that apply to this agent and the current run's usage. */
export function limitsStatus(): LimitsStatus | undefined {
  try {
    return state.guard?.status();
  } catch {
    return undefined;
  }
}

/** Send buffered spans now. Await it at the end of serverless handlers. */
export async function flush(): Promise<boolean> {
  try {
    await state.provider?.forceFlush();
    return true;
  } catch {
    return false;
  }
}

export async function shutdown(): Promise<void> {
  state.poller?.stop();
  const provider = state.provider;
  Object.assign(state, { poller: undefined, provider: undefined, sampler: undefined, exporter: undefined, remote: undefined, cfg: undefined, capture: true, guard: undefined });
  if (!provider) return;
  try {
    await provider.shutdown();
  } catch {
    // never throw into the host app
  }
  trace.disable();
  context.disable();
}

/** Run fn with every step inside it tagged with a session/conversation ID. */
export function withSession<T>(sessionId: string, fn: () => T): T {
  return session.run(sessionId, fn);
}

// ---- manual instrumentation ------------------------------------------------

export type StepType = 'agent' | 'llm_call' | 'tool_call' | 'retrieval' | 'decision' | 'handoff' | 'other';

function preview(v: unknown): string {
  let text: string;
  try {
    text = typeof v === 'string' ? v : (JSON.stringify(v, (_k, x) => (typeof x === 'bigint' ? x.toString() : x)) ?? String(v));
  } catch {
    text = String(v);
  }
  return text.slice(0, MAX_CAPTURE);
}

/**
 * Trace a block of code (sync or async) as one agent step.
 *   await bindle.step('plan', { type: 'decision' }, async (span) => { ... })
 */
export function step<T>(name: string, opts: { type?: StepType; attributes?: Record<string, unknown> }, fn: (span: Span) => T): T {
  const tracer = trace.getTracer(TRACER, VERSION);
  return tracer.startActiveSpan(name, (span) => {
    span.setAttribute('bindle.step.type', opts.type ?? 'other');
    const sid = session.getStore();
    if (sid) span.setAttribute('bindle.session.id', sid);
    for (const [k, v] of Object.entries(opts.attributes ?? {})) {
      if (v === undefined || v === null) continue;
      const key = k.includes('.') ? k : `bindle.${k}`;
      span.setAttribute(key, typeof v === 'string' || typeof v === 'number' || typeof v === 'boolean' ? v : preview(v));
    }
    const fail = (e: unknown) => {
      span.recordException(e as Error);
      span.setStatus({ code: SpanStatusCode.ERROR, message: e instanceof Error ? e.message : String(e) });
    };
    let result: T;
    try {
      result = fn(span);
    } catch (e) {
      fail(e);
      span.end();
      throw e;
    }
    if (result instanceof Promise) {
      return result.then(
        (v) => {
          span.end();
          return v;
        },
        (e) => {
          fail(e);
          span.end();
          throw e;
        },
      ) as T;
    }
    span.end();
    return result;
  });
}

function wrap<A extends unknown[], R>(fn: (...args: A) => R, name: string, type: StepType, attributes: Record<string, unknown>) {
  return (...args: A): R =>
    step(name, { type, attributes }, (span) => {
      if (state.capture) span.setAttribute('input.value', preview(args.length === 1 ? args[0] : args));
      const out = fn(...args);
      const record = (v: unknown) => {
        if (state.capture) span.setAttribute('output.value', preview(v));
        return v;
      };
      return (out instanceof Promise ? out.then(record) : record(out)) as R;
    });
}

/** Wrap a function (sync or async) as an agent step; args are input, the result is output. */
export function traceStep<A extends unknown[], R>(fn: (...args: A) => R, opts: { name?: string; type?: StepType } = {}): (...args: A) => R {
  return wrap(fn, opts.name ?? (fn.name || 'step'), opts.type ?? 'other', {});
}

/** Wrap a tool: a tool_call step tagged with the tool's name. */
export function traceTool<A extends unknown[], R>(fn: (...args: A) => R, opts: { name?: string } = {}): (...args: A) => R {
  const name = opts.name ?? (fn.name || 'tool');
  return wrap(fn, name, 'tool_call', { 'bindle.tool.name': name });
}

/** Record a routing/planning decision as its own step. */
export function decision(name: string, options: string[], chosen: string, rationale?: string): void {
  step(name, { type: 'decision' }, (span) => {
    span.setAttribute('bindle.decision.options', options);
    span.setAttribute('bindle.decision.chosen', chosen);
    if (rationale && state.capture) span.setAttribute('bindle.reasoning', rationale.slice(0, MAX_CAPTURE));
  });
}

export interface ScoreOptions {
  value?: number;
  passed?: boolean;
  comment?: string;
  /** The run to score, from outside it (CI, a judge over an exported dataset). */
  traceId?: string;
}

/**
 * Record a score for a run: a number, pass/fail, or both. Inside a run the
 * score joins the current trace; elsewhere pass the run's traceId (32 hex
 * characters). Returns true if recorded. Never throws.
 */
export function score(name: string, opts: ScoreOptions & { source?: 'user' } = {}): boolean {
  try {
    if (!state.cfg || typeof name !== 'string' || !name.trim() || name.length > 80) return false;
    const { value, passed, comment, traceId } = opts;
    if (value === undefined && passed === undefined) return false;
    const attrs: Attributes = { 'bindle.step.type': 'eval', 'bindle.score.name': name.trim() };
    if (value !== undefined) {
      if (typeof value !== 'number' || !Number.isFinite(value)) return false;
      attrs['bindle.score.value'] = value;
    }
    if (passed !== undefined) attrs['bindle.score.passed'] = Boolean(passed);
    if (comment && state.capture) attrs['output.value'] = String(comment).slice(0, MAX_CAPTURE);
    if (opts.source === 'user') attrs['bindle.score.source'] = 'user';
    let ctx = context.active();
    if (traceId !== undefined) {
      const tid = String(traceId).toLowerCase();
      if (!/^[0-9a-f]{32}$/.test(tid) || /^0+$/.test(tid)) return false;
      const spanId = Array.from({ length: 16 }, () => Math.floor(Math.random() * 16).toString(16)).join('').replace(/^0{16}$/, '0000000000000001');
      ctx = trace.setSpanContext(ctx, { traceId: tid, spanId, traceFlags: 1, isRemote: true });
      attrs['bindle.score.external'] = true;
    } else if (!trace.getSpan(ctx)?.spanContext() || !trace.isSpanContextValid(trace.getSpan(ctx)!.spanContext())) {
      debug('bindle: score() outside a run needs traceId');
      return false;
    }
    const tracer = (state.provider ?? trace.getTracerProvider()).getTracer('bindle', VERSION);
    tracer.startSpan(`score ${name.trim()}`, { attributes: attrs }, ctx).end();
    return true;
  } catch (e) {
    debug(`bindle: score failed: ${(e as Error).message}`);
    return false;
  }
}

/**
 * Record a user's feedback on a run (thumbs up/down, a rating, or both, with
 * an optional comment), usually from your app's feedback handler with the
 * trace ID the run returned (see currentTraceId()). Returns true if recorded.
 */
export function feedback(opts: { traceId?: string; thumbsUp?: boolean; rating?: number; comment?: string }): boolean {
  const value = opts.rating ?? (opts.thumbsUp === undefined ? undefined : opts.thumbsUp ? 1 : 0);
  return score('feedback', { value, passed: opts.thumbsUp, comment: opts.comment, traceId: opts.traceId, source: 'user' });
}

/** The current run's trace ID, to hand to your UI for feedback later; undefined outside a run. */
export function currentTraceId(): string | undefined {
  const sc = trace.getSpan(context.active())?.spanContext();
  return sc && trace.isSpanContextValid(sc) ? sc.traceId : undefined;
}

/** Record a handoff to another agent or a human. */
export function handoff(toAgent: string, opts: { reason?: string; fromAgent?: string } = {}): void {
  step(`handoff -> ${toAgent}`, { type: 'handoff' }, (span) => {
    span.setAttribute('bindle.handoff.to', toAgent);
    if (opts.fromAgent) span.setAttribute('bindle.handoff.from', opts.fromAgent);
    if (opts.reason) span.setAttribute('bindle.handoff.reason', opts.reason);
  });
}

/** For tests: the effective settings after remote config. */
export function _effective(): { sampleRate?: number; captureContent: boolean; remoteVersion?: number } {
  return { sampleRate: state.sampler?.rate, captureContent: state.capture, remoteVersion: state.remote?.version };
}

export { applyRemote as _applyRemote };
