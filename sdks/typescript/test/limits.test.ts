import { trace } from '@opentelemetry/api';
import { InMemorySpanExporter, SimpleSpanProcessor, TracerProvider as BasicTracerProvider } from '@opentelemetry/sdk-trace';
import type { Span } from '@opentelemetry/api';
import { AsyncLocalStorageContextManager } from '@opentelemetry/context-async-hooks';
import { context } from '@opentelemetry/api';
import { afterEach, beforeAll, describe, expect, it } from 'vitest';
import * as bindle from '../src/index.js';
import { Guard, LimitExceededError, RunCounter, limitFor, parseLimits, type Limit } from '../src/limits.js';
import { parseSettings } from '../src/remote.js';

const lim = (o: Partial<Limit> & { agent: string }): Limit => ({ paused: false, maxStepsPerRun: 0, maxTokensPerRun: 0, dailyBudgetUsd: 0, pauseOverBudget: false, maxRepeatCalls: 0, ...o });

beforeAll(() => {
  context.setGlobalContextManager(new AsyncLocalStorageContextManager().enable());
});

function setup(agent = 'refunds') {
  const counter = new RunCounter();
  const provider = new BasicTracerProvider({ spanProcessors: [counter, new SimpleSpanProcessor({ exporter: new InMemorySpanExporter() })] });
  return { tracer: provider.getTracer('t'), guard: new Guard(agent, counter), counter };
}

const expectReason = (fn: () => void, reason: string) => {
  try {
    fn();
  } catch (e) {
    expect(e).toBeInstanceOf(LimitExceededError);
    expect((e as LimitExceededError).reason).toBe(reason);
    return e as LimitExceededError;
  }
  throw new Error(`expected ${reason}`);
};

describe('limits', () => {
  afterEach(async () => {
    await bindle.shutdown();
  });

  it('parses tolerantly and picks the agent row, else *', () => {
    const ls = parseLimits([{ agent: '*', max_steps_per_run: 50 }, { agent: 'refunds', max_steps_per_run: 10 }, { agent: '' }, 'x', { agent: 'n', max_tokens_per_run: -3 }]);
    expect(ls.map((l) => l.agent)).toEqual(['*', 'refunds', 'n']);
    expect(ls[2]!.maxTokensPerRun).toBe(0);
    expect(limitFor(ls, 'refunds')!.maxStepsPerRun).toBe(10);
    expect(limitFor(ls, 'other')!.maxStepsPerRun).toBe(50);
    expect(limitFor(ls.slice(1), 'other')).toBeUndefined();
    const s = parseSettings(JSON.stringify({ version: 1, sample_rate: 1, capture_content: true, limits: [{ agent: 'a', paused: true }], over_budget: ['a', 3] }))!;
    expect(s.limits[0]!.paused).toBe(true);
    expect(s.overBudget).toEqual(['a']);
  });

  it('pauses and stops over-budget agents only when the limit says so', () => {
    const { guard } = setup();
    guard.update([lim({ agent: 'refunds', paused: true })], []);
    expectReason(() => guard.check(), 'paused');
    guard.update([lim({ agent: 'refunds', dailyBudgetUsd: 5 })], ['refunds']);
    guard.check();
    guard.update([lim({ agent: '*', dailyBudgetUsd: 5, pauseOverBudget: true })], ['refunds']);
    expectReason(() => guard.check(), 'over_budget');
  });

  it('counts steps and LLM tokens in the active trace', () => {
    const { tracer, guard } = setup();
    guard.update([lim({ agent: '*', maxStepsPerRun: 3, maxTokensPerRun: 100 })], []);
    tracer.startActiveSpan('run', (root: Span) => {
      tracer.startSpan('chat', { attributes: { 'gen_ai.operation.name': 'chat', 'gen_ai.usage.input_tokens': 60, 'gen_ai.usage.output_tokens': 30 } }).end();
      tracer.startSpan('agent', { attributes: { 'gen_ai.operation.name': 'invoke_agent', 'gen_ai.usage.input_tokens': 900 } }).end();
      guard.check(); // 3 steps, 90 tokens
      tracer.startSpan('tool').end();
      const e = expectReason(() => guard.check(), 'max_steps_per_run');
      expect(e.value).toBe(4);
      root.end();
    });
    tracer.startActiveSpan('next', (s: Span) => {
      guard.check(); // a new run starts from zero
      s.end();
    });
    guard.update([lim({ agent: '*', maxTokensPerRun: 100 })], []);
    tracer.startActiveSpan('run2', (root: Span) => {
      tracer.startSpan('llm', { attributes: { 'openinference.span.kind': 'LLM', 'llm.token_count.prompt': 120 } }).end();
      expectReason(() => guard.check(), 'max_tokens_per_run');
      root.end();
    });
  });

  it('stops a run stopped from the console, and only that run', () => {
    const { tracer, guard } = setup();
    tracer.startActiveSpan('run', (root: Span) => {
      guard.update([], [], ['0'.repeat(32)]);
      guard.check();
      guard.update([], [], [root.spanContext().traceId]);
      expectReason(() => guard.check(), 'stopped');
      expect(guard.status().stopped).toBe(true);
      root.end();
    });
    tracer.startActiveSpan('other', (s: Span) => {
      guard.check();
      s.end();
    });
    const st = parseSettings(JSON.stringify({ version: 1, sample_rate: 1, capture_content: true, stopped_runs: ['0af7651916cd43dd8448eb211c80319c', 'NOPE', 4] }))!;
    expect(st.stoppedRuns).toEqual(['0af7651916cd43dd8448eb211c80319c']);
  });

  it('guards against the same tool called with the same arguments', () => {
    const { tracer, guard } = setup();
    guard.update([lim({ agent: 'refunds', maxRepeatCalls: 2 })], []);
    const tool = (name: string, args: string) =>
      tracer.startSpan(`execute_tool ${name}`, { attributes: { 'gen_ai.operation.name': 'execute_tool', 'gen_ai.tool.name': name, 'gen_ai.tool.call.arguments': args } }).end();
    tracer.startActiveSpan('run', (root: Span) => {
      tool('search', '{"q":"refund","page":1}');
      tool('search', '{"page":1,"q":"refund"}'); // same call, keys reordered
      tool('search', '{"q":"refund","page":2}');
      tool('lookup', '{"q":"refund","page":1}');
      guard.check();
      tool('search', '{"q": "refund", "page": 1}');
      const e = expectReason(() => guard.check(), 'max_repeat_calls');
      expect(e.value).toBe(3);
      expect(e.message).toContain('search');
      root.end();
    });
    expect(parseLimits([{ agent: '*', max_repeat_calls: 4 }])[0]!.maxRepeatCalls).toBe(4);
  });

  it('bounds the number of tracked runs', () => {
    const c = new RunCounter(3);
    const p = new BasicTracerProvider({ spanProcessors: [c] });
    for (let i = 0; i < 5; i++) p.getTracer('t').startSpan('r').end();
    expect(c.size()).toBe(3);
  });

  it('public API: no-op before init, enforces after a console pause', () => {
    bindle.checkLimits();
    expect(bindle.limitsStatus()).toBeUndefined();
    const mem = new InMemorySpanExporter();
    expect(bindle.init({ apiKey: 'bd_test_' + 'a'.repeat(40), endpoint: 'http://localhost:4318', agent: 'refunds', remoteConfig: false }, { exporter: () => mem })).toBe(true);
    bindle.checkLimits();
    bindle._applyRemote({ version: 2, sampleRate: 1, captureContent: true, redactPatterns: {}, pollIntervalSeconds: 60, limits: [lim({ agent: 'refunds', paused: true })], overBudget: [], stoppedRuns: [] });
    expect(() => bindle.checkLimits()).toThrow(bindle.LimitExceededError);
    expect(bindle.limitsStatus()!.paused).toBe(true);
    void trace;
  });

  it('tags every span in a session, framework spans included', async () => {
    const mem = new InMemorySpanExporter();
    expect(bindle.init({ apiKey: 'bd_test_' + 'a'.repeat(40), endpoint: 'http://localhost:4318', agent: 'chat', remoteConfig: false }, { exporter: () => mem })).toBe(true);
    bindle.withSession('conv-1', () => trace.getTracer('framework').startSpan('framework span').end());
    trace.getTracer('framework').startSpan('after').end();
    await bindle.flush();
    const byName = Object.fromEntries(mem.getFinishedSpans().map((s) => [s.name, s]));
    expect(byName['framework span']!.attributes['bindle.session.id']).toBe('conv-1');
    expect(byName['after']!.attributes['bindle.session.id']).toBeUndefined();
  });

  it('records user feedback', async () => {
    const mem = new InMemorySpanExporter();
    expect(bindle.init({ apiKey: 'bd_test_' + 'a'.repeat(40), endpoint: 'http://localhost:4318', agent: 'refunds', remoteConfig: false }, { exporter: () => mem })).toBe(true);
    expect(bindle.currentTraceId()).toBeUndefined();
    let tid = '';
    trace.getTracer('t').startActiveSpan('run', (s: Span) => {
      tid = bindle.currentTraceId()!;
      s.end();
    });
    expect(tid).toHaveLength(32);
    expect(bindle.feedback({ traceId: tid, thumbsUp: false, comment: 'wrong order' })).toBe(true);
    expect(bindle.feedback({ traceId: tid })).toBe(false);
    await bindle.flush();
    const fb = mem.getFinishedSpans().find((s) => s.name === 'score feedback')!;
    expect(fb.attributes['bindle.score.source']).toBe('user');
    expect(fb.attributes['bindle.score.value']).toBe(0);
    expect(fb.spanContext().traceId).toBe(tid);
  });

  it('records outcomes', async () => {
    const mem = new InMemorySpanExporter();
    expect(bindle.init({ apiKey: 'bd_test_' + 'a'.repeat(40), endpoint: 'http://localhost:4318', agent: 'support', remoteConfig: false }, { exporter: () => mem })).toBe(true);
    let tid = '';
    trace.getTracer('t').startActiveSpan('run', (s: Span) => {
      expect(bindle.outcome('ticket_resolved')).toBe(true);
      tid = bindle.currentTraceId()!;
      s.end();
    });
    expect(bindle.outcome('refund_issued', { success: false, value: 42.5, traceId: tid })).toBe(true);
    expect(bindle.outcome('bad', { success: 'yes' as unknown as boolean })).toBe(false);
    expect(bindle.outcome('', { traceId: tid })).toBe(false);
    await bindle.flush();
    const byName = Object.fromEntries(mem.getFinishedSpans().map((s) => [s.name, s]));
    const ok = byName['score ticket_resolved']!;
    const bad = byName['score refund_issued']!;
    expect(ok.attributes['bindle.score.source']).toBe('outcome');
    expect(ok.attributes['bindle.score.passed']).toBe(true);
    expect(ok.attributes['bindle.score.external']).toBeUndefined();
    expect(bad.attributes['bindle.score.passed']).toBe(false);
    expect(bad.attributes['bindle.score.value']).toBe(42.5);
    expect(bad.spanContext().traceId).toBe(tid);
  });

  it('records scores inside a run and by trace ID', async () => {
    expect(bindle.score('x', { value: 1 })).toBe(false); // not initialized
    const mem = new InMemorySpanExporter();
    expect(bindle.init({ apiKey: 'bd_test_' + 'a'.repeat(40), endpoint: 'http://localhost:4318', agent: 'refunds', remoteConfig: false }, { exporter: () => mem })).toBe(true);
    expect(bindle.score('helpful', { value: 0.9 })).toBe(false); // outside a run
    let runTrace = '';
    trace.getTracer('t').startActiveSpan('run', (s: Span) => {
      runTrace = s.spanContext().traceId;
      expect(bindle.score('helpful', { value: 0.9, passed: true })).toBe(true);
      s.end();
    });
    expect(bindle.score('judge', { passed: false, traceId: runTrace.toUpperCase() })).toBe(true);
    expect(bindle.score('bad', { value: Number.NaN, traceId: runTrace })).toBe(false);
    expect(bindle.score('bad', { value: 1, traceId: 'nothex' })).toBe(false);
    expect(bindle.score('nothing', { traceId: runTrace })).toBe(false);
    await bindle.flush();
    const byName = Object.fromEntries(mem.getFinishedSpans().map((s) => [s.name, s]));
    const inRun = byName['score helpful']!;
    const ext = byName['score judge']!;
    expect(inRun.spanContext().traceId).toBe(runTrace);
    expect(inRun.attributes['bindle.score.value']).toBe(0.9);
    expect(inRun.attributes['bindle.score.external']).toBeUndefined();
    expect(ext.spanContext().traceId).toBe(runTrace);
    expect(ext.attributes['bindle.score.external']).toBe(true);
    expect(ext.attributes['bindle.score.passed']).toBe(false);
  });
});
