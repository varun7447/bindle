// Turns a step's stored input/output into a conversation when it is one.
// Handles the shapes frameworks actually send (checked against real traces):
//  - OpenTelemetry GenAI messages [{role, parts:[{type, content|name|arguments|response}]}]
//    (Microsoft Agent Framework, Google ADK)
//  - role-tagged events [{role, content}] (Strands, Foundry, via the normalizer)
//  - Bedrock/Strands content blocks [{text}|{toolUse}|{toolResult}]
//  - OpenAI chat [{role, content, tool_calls}]
//  - LangChain {"messages":[[{kwargs:{type, content}}]]} / {"generations":[[{message|text}]]}
// plus "system: …" prefixes and "\n\n"-joined segments from the normalizer.
// Anything else returns null and is shown as raw text. Pure; unit-tested.

export type Part =
  | { kind: 'text'; text: string }
  | { kind: 'tool_call'; name: string; args: string }
  | { kind: 'tool_result'; name?: string; text: string };

export interface Message {
  role: string; // system | user | assistant | tool
  parts: Part[];
}

type J = unknown;
const isObj = (v: J): v is Record<string, J> => typeof v === 'object' && v !== null && !Array.isArray(v);

function str(v: J): string {
  if (typeof v === 'string') return v;
  if (v === undefined || v === null) return '';
  return JSON.stringify(v, null, 2);
}

function normRole(r: J, fallback: string): string {
  const s = typeof r === 'string' ? r.toLowerCase() : '';
  if (s === 'human') return 'user';
  if (s === 'ai' || s === 'model' || s === 'choice') return 'assistant';
  if (s === 'function') return 'tool';
  return s || fallback;
}

/** Parts from one content value (string, block list, or GenAI parts). */
function partsOf(content: J): Part[] {
  if (typeof content === 'string') return content.trim() ? [{ kind: 'text', text: content }] : [];
  if (isObj(content)) {
    // {content: "..."} (Foundry event bodies) or a single block
    if (typeof content.content === 'string' && Object.keys(content).length <= 2) return partsOf(content.content);
    return blockParts(content);
  }
  if (!Array.isArray(content)) return [];
  return content.flatMap((b) => (typeof b === 'string' ? partsOf(b) : isObj(b) ? blockParts(b) : []));
}

function blockParts(b: Record<string, J>): Part[] {
  // GenAI semconv parts
  if (b.type === 'text' || (b.type === undefined && typeof b.text === 'string')) {
    const t = typeof b.content === 'string' ? b.content : typeof b.text === 'string' ? b.text : '';
    return t ? [{ kind: 'text', text: t }] : [];
  }
  if (b.type === 'tool_call' || b.type === 'function_call') return [{ kind: 'tool_call', name: str(b.name), args: str(b.arguments) }];
  if (b.type === 'tool_call_response' || b.type === 'function_result' || b.type === 'tool_result') {
    return [{ kind: 'tool_result', name: typeof b.name === 'string' ? b.name : undefined, text: str(b.response ?? b.result ?? b.content) }];
  }
  // Bedrock / Strands blocks
  if (isObj(b.toolUse)) return [{ kind: 'tool_call', name: str(b.toolUse.name), args: str(b.toolUse.input) }];
  if (isObj(b.toolResult)) {
    const inner = partsOf(b.toolResult.content);
    return [{ kind: 'tool_result', text: inner.map((p) => (p.kind === 'text' ? p.text : '')).join('\n') || str(b.toolResult.content) }];
  }
  if (typeof b.content === 'string') return [{ kind: 'text', text: b.content }];
  return [];
}

function fromMessageObject(m: Record<string, J>, fallbackRole: string): Message | null {
  // LangChain serialized message
  if (isObj(m.kwargs)) {
    const k = m.kwargs;
    const parts = partsOf(k.content);
    if (Array.isArray(k.tool_calls)) {
      for (const tc of k.tool_calls) if (isObj(tc)) parts.push({ kind: 'tool_call', name: str(tc.name), args: str(tc.args) });
    }
    const role = normRole(k.type, Array.isArray(m.id) ? normRole(String(m.id.at(-1)).replace(/Message$/, ''), fallbackRole) : fallbackRole);
    return parts.length ? { role, parts } : null;
  }
  const parts = m.parts !== undefined ? partsOf(m.parts) : partsOf(m.content);
  if (Array.isArray(m.tool_calls)) {
    for (const tc of m.tool_calls) {
      if (!isObj(tc)) continue;
      const fn = isObj(tc.function) ? tc.function : tc;
      parts.push({ kind: 'tool_call', name: str(fn.name), args: str(fn.arguments ?? fn.args) });
    }
  }
  if (!parts.length) return null;
  let role = normRole(m.role, fallbackRole);
  // Role-less block lists: infer from what they contain.
  if (m.role === undefined && parts.every((p) => p.kind === 'tool_call')) role = 'assistant';
  if (m.role === undefined && parts.every((p) => p.kind === 'tool_result')) role = 'tool';
  return { role, parts };
}

function fromValue(v: J, fallbackRole: string): Message[] | null {
  if (isObj(v)) {
    // LangChain input / output envelopes
    if (Array.isArray(v.messages)) return fromValue(v.messages.flat(), fallbackRole);
    if (Array.isArray(v.generations)) {
      const out: Message[] = [];
      for (const g of v.generations.flat()) {
        if (!isObj(g)) continue;
        const m = isObj(g.message) ? fromMessageObject(g.message, 'assistant') : null;
        if (m) out.push(m);
        else if (typeof g.text === 'string' && g.text) out.push({ role: 'assistant', parts: [{ kind: 'text', text: g.text }] });
      }
      return out.length ? out : null;
    }
    if (v.role !== undefined || v.kwargs !== undefined) {
      const m = fromMessageObject(v, fallbackRole);
      return m ? [m] : null;
    }
    return null;
  }
  if (!Array.isArray(v) || v.length === 0) return null;
  const looksLikeMessages = v.every((m) => isObj(m) && (m.role !== undefined || m.kwargs !== undefined || m.parts !== undefined));
  if (looksLikeMessages) {
    const out = v.map((m) => fromMessageObject(m as Record<string, J>, fallbackRole)).filter((m): m is Message => m !== null);
    return out.length ? out : null;
  }
  // A plain list of content blocks: one message.
  const m = fromMessageObject({ content: v }, fallbackRole);
  return m ? [m] : null;
}

function tryJson(s: string): { ok: true; v: J } | { ok: false } {
  const t = s.trim();
  if (!t || (t[0] !== '[' && t[0] !== '{')) return { ok: false };
  try {
    return { ok: true, v: JSON.parse(t) };
  } catch {
    return { ok: false };
  }
}

/** The conversation in a stored input/output, or null if it isn't one. */
export function parseMessages(text: string, side: 'input' | 'output'): Message[] | null {
  if (!text || text.length > 1_000_000) return null;
  const fallback = side === 'input' ? 'user' : 'assistant';
  const whole = tryJson(text);
  if (whole.ok) return fromValue(whole.v, fallback);

  // Normalizer joins: "system: <json>\n\n<json>\n\n…"
  const out: Message[] = [];
  for (const raw of text.split(/\n\n(?=[[{]|system: )/)) {
    let seg = raw;
    let role = fallback;
    if (seg.startsWith('system: ')) {
      seg = seg.slice(8);
      role = 'system';
    }
    const j = tryJson(seg);
    if (!j.ok) {
      if (role === 'system' && seg.trim()) {
        out.push({ role, parts: [{ kind: 'text', text: seg.trim() }] });
        continue;
      }
      return null;
    }
    const ms = role === 'system' ? fromValue(j.v, 'system')?.map((m) => ({ ...m, role: 'system' })) : fromValue(j.v, role);
    if (!ms) return null;
    out.push(...ms);
  }
  return out.length ? out : null;
}
