// Connecting runs across processes, queues, people and MCP servers.
//
// contextToken() captures where a run is (W3C trace context plus the agent
// and session) as one short string; resume(token) starts a run that links
// back to it. injectMcpMeta()/withMcpContext() carry the trace context in an
// MCP request's params._meta (traceparent/tracestate, per the OpenTelemetry
// MCP conventions).
//
// Token format (shared with the Python SDK):
//   b1.<traceparent>[.<base64url JSON {"ts": tracestate, "a": agent, "s": session}>]
import { createTraceState, isSpanContextValid, type SpanContext } from '@opentelemetry/api';

export type LinkKind = 'resumed' | 'handoff' | 'called';
export const LINK_KINDS: readonly LinkKind[] = ['resumed', 'handoff', 'called'];
export const MAX_TOKEN = 2048;
const MAX_FIELD = 256;
const TRACEPARENT = /^00-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$/;
const ZERO_TRACE = '0'.repeat(32);
const ZERO_SPAN = '0'.repeat(16);

export interface ParsedToken {
  spanContext: SpanContext;
  agent?: string;
  session?: string;
}

function traceparent(sc: SpanContext): string {
  return `00-${sc.traceId}-${sc.spanId}-${(sc.traceFlags & 0xff).toString(16).padStart(2, '0')}`;
}

function spanContextFrom(tp: unknown, ts: unknown): SpanContext | undefined {
  const m = typeof tp === 'string' ? TRACEPARENT.exec(tp) : null;
  const [, traceId = ZERO_TRACE, spanId = ZERO_SPAN, flags = '00'] = m ?? [];
  if (!m || traceId === ZERO_TRACE || spanId === ZERO_SPAN) return undefined;
  const sc: SpanContext = { traceId, spanId, traceFlags: parseInt(flags, 16), isRemote: true };
  if (typeof ts === 'string' && ts) {
    try {
      sc.traceState = createTraceState(ts.slice(0, 512));
    } catch {
      /* ignore a bad tracestate */
    }
  }
  return sc;
}

export function makeToken(sc: SpanContext | undefined, agent?: string, session?: string): string | undefined {
  if (!sc || !isSpanContextValid(sc)) return undefined;
  const extra: Record<string, string> = {};
  const ts = sc.traceState?.serialize() ?? '';
  if (ts) extra.ts = ts.slice(0, 512);
  if (agent) extra.a = agent.slice(0, MAX_FIELD);
  if (session) extra.s = session.slice(0, MAX_FIELD);
  let token = `b1.${traceparent(sc)}`;
  if (Object.keys(extra).length) token += '.' + Buffer.from(JSON.stringify(extra), 'utf8').toString('base64url');
  return token;
}

/** The token's trace context, agent and session; undefined if malformed. */
export function parseToken(token: unknown): ParsedToken | undefined {
  if (typeof token !== 'string' || token.length > MAX_TOKEN) return undefined;
  const parts = token.trim().split('.');
  if ((parts.length !== 2 && parts.length !== 3) || parts[0] !== 'b1') return undefined;
  let extra: Record<string, unknown> = {};
  const [, tp = '', payload] = parts;
  if (payload !== undefined) {
    if (!/^[A-Za-z0-9_-]*$/.test(payload)) return undefined;
    try {
      const v: unknown = JSON.parse(Buffer.from(payload, 'base64url').toString('utf8'));
      if (typeof v !== 'object' || v === null || Array.isArray(v)) return undefined;
      extra = v as Record<string, unknown>;
    } catch {
      return undefined;
    }
  }
  const spanContext = spanContextFrom(tp, extra.ts);
  if (!spanContext) return undefined;
  const field = (k: string) => (typeof extra[k] === 'string' && extra[k] ? (extra[k] as string).slice(0, MAX_FIELD) : undefined);
  return { spanContext, agent: field('a'), session: field('s') };
}

/** A copy of MCP request params with the span context in _meta; params unchanged without one. */
export function injectMeta<P extends Record<string, unknown>>(params: P | undefined, sc: SpanContext | undefined): P {
  const out = { ...(params ?? {}) } as Record<string, unknown>;
  if (!sc || !isSpanContextValid(sc)) return out as P;
  const meta = { ...((typeof out._meta === 'object' && out._meta !== null ? out._meta : {}) as Record<string, unknown>) };
  meta.traceparent = traceparent(sc);
  const ts = sc.traceState?.serialize();
  if (ts) meta.tracestate = ts;
  out._meta = meta;
  return out as P;
}

/** The caller's span context from MCP request params._meta, if any. */
export function extractMeta(params: unknown): SpanContext | undefined {
  if (typeof params !== 'object' || params === null) return undefined;
  const meta = (params as Record<string, unknown>)._meta;
  if (typeof meta !== 'object' || meta === null) return undefined;
  const m = meta as Record<string, unknown>;
  return spanContextFrom(m.traceparent, m.tracestate);
}
