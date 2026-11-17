import { describe, expect, it } from 'vitest';
import { emptyReason } from './content';
import { parseMessages } from './messages';

// Real stored values from each framework (see examples/frameworks).
const MAF_IN = `system: [{"type": "text", "content": "Help with refunds."}]

[{"role": "user", "parts": [{"type": "text", "content": "Where is my refund? I'm [REDACTED:EMAIL]"}]}]`;
const MAF_OUT = `[{"role": "assistant", "parts": [{"type": "tool_call", "id": "c1", "name": "lookup_order", "arguments": {"email": "[REDACTED:EMAIL]"}}]}]`;
const ADK_IN = `system: [{"content":"Help with refunds.","type":"text"}]

[{"role":"user","parts":[{"content":"Where is my refund?","type":"text"}]},{"role":"assistant","parts":[{"id":"x","name":"lookup_order","arguments":{"email":"e"},"type":"tool_call"}]},{"role":"user","parts":[{"id":"x","response":{"result":"order 1234"},"type":"tool_call_response"}]}]`;
const STRANDS_OLD_IN = `[{"text": "Where is my refund?"}]

[{"toolUse": {"toolUseId": "t1", "name": "lookup_order", "input": {"email": "e"}}}]

[{"toolResult": {"toolUseId": "t1", "status": "success", "content": [{"text": "order 1234: refund issued"}]}}]`;
const STRANDS_NEW_IN = `[{"role":"user","content":[{"text":"Where is my refund?"}]},{"role":"assistant","content":[{"toolUse":{"toolUseId":"t1","name":"lookup_order","input":{"email":"e"}}}]},{"role":"tool","content":[{"toolResult":{"toolUseId":"t1","content":[{"text":"order 1234"}]}}]}]`;
const LC_IN = `{"messages": [[{"lc": 1, "type": "constructor", "id": ["langchain", "schema", "messages", "HumanMessage"], "kwargs": {"content": "Plan: look up the order", "type": "human"}}]]}`;
const LC_OUT = `{"generations": [[{"text": "Your refund is on its way", "type": "ChatGeneration", "message": {"lc": 1, "id": ["langchain", "schema", "messages", "AIMessage"], "kwargs": {"content": "Your refund is on its way", "type": "ai", "tool_calls": []}}}]], "type": "LLMResult"}`;

const shape = (ms: ReturnType<typeof parseMessages>) => ms?.map((m) => `${m.role}:${m.parts.map((p) => p.kind).join('+')}`);

describe('parseMessages', () => {
  it('reads Microsoft Agent Framework messages', () => {
    expect(shape(parseMessages(MAF_IN, 'input'))).toEqual(['system:text', 'user:text']);
    const out = parseMessages(MAF_OUT, 'output')!;
    expect(out[0]).toEqual({ role: 'assistant', parts: [{ kind: 'tool_call', name: 'lookup_order', args: '{\n  "email": "[REDACTED:EMAIL]"\n}' }] });
  });
  it('reads Google ADK history with tool calls and results', () => {
    expect(shape(parseMessages(ADK_IN, 'input'))).toEqual(['system:text', 'user:text', 'assistant:tool_call', 'user:tool_result']);
  });
  it('reads Strands blocks, old and role-tagged', () => {
    expect(shape(parseMessages(STRANDS_OLD_IN, 'input'))).toEqual(['user:text', 'assistant:tool_call', 'tool:tool_result']);
    const ms = parseMessages(STRANDS_NEW_IN, 'input')!;
    expect(shape(ms)).toEqual(['user:text', 'assistant:tool_call', 'tool:tool_result']);
    expect(ms[2]!.parts[0]).toEqual({ kind: 'tool_result', text: 'order 1234' });
    expect(shape(parseMessages('[{"text": "Your refund is on its way."}]', 'output'))).toEqual(['assistant:text']);
  });
  it('reads LangChain envelopes', () => {
    expect(parseMessages(LC_IN, 'input')).toEqual([{ role: 'user', parts: [{ kind: 'text', text: 'Plan: look up the order' }] }]);
    expect(parseMessages(LC_OUT, 'output')).toEqual([{ role: 'assistant', parts: [{ kind: 'text', text: 'Your refund is on its way' }] }]);
  });
  it('reads OpenAI chat', () => {
    const ms = parseMessages('[{"role":"system","content":"be brief"},{"role":"assistant","content":null,"tool_calls":[{"function":{"name":"f","arguments":"{}"}}]}]', 'input');
    expect(shape(ms)).toEqual(['system:text', 'assistant:tool_call']);
  });
  it('keeps a plain-text system prompt', () => {
    expect(shape(parseMessages('system: Be brief.\n\n[{"role":"user","content":"hi"}]', 'input'))).toEqual(['system:text', 'user:text']);
  });
  it('leaves anything else alone', () => {
    for (const t of ['', 'hello', '{"order_id": "o_981", "amount": 42.5}', '[1,2]', '{"a": [']) {
      expect(parseMessages(t, 'input')).toBeNull();
    }
  });
});

describe('emptyReason', () => {
  const base = { type: 'tool_call', framework: '', attributes: {}, input: '', output: '' };
  it('names who dropped content', () => {
    expect(emptyReason({ ...base, attributes: { 'bindle.content_dropped': 'workspace' } }, 'input')).toMatchObject({ href: '/settings/sdk' });
    expect(emptyReason({ ...base, attributes: { 'bindle.content_dropped': 'sdk' } }, 'input').title).toContain("agent's code");
  });
  it('gives the framework switch when the framework sent nothing', () => {
    const r = emptyReason({ ...base, framework: 'Microsoft Agent Framework' }, 'output');
    expect(r.code).toBe('ENABLE_SENSITIVE_DATA=true');
    // If the other field arrived, the framework is capturing: no switch.
    expect(emptyReason({ ...base, framework: 'Microsoft Agent Framework', input: 'x' }, 'output').code).toBeUndefined();
  });
  it('explains wrapper steps', () => {
    expect(emptyReason({ ...base, type: 'agent' }, 'input').title).toBe('This step has no input of its own.');
    expect(emptyReason(base, 'output').title).toBe("The agent didn't send an output for this step.");
  });
});
