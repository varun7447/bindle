// Step-type identity. Colors are CSS custom properties (see globals.css) so
// light and dark themes each use their own validated step of the same hue:
// categorical slots 1-5 and 7 of the reference palette, in the order the
// "where the time went" bar stacks them. Passes the dataviz validator in both
// themes; light mode needs visible labels, so every place a step appears also
// prints its type.

export interface StepKind {
  label: string;
  color: string;
}

const KINDS: Record<string, StepKind> = {
  llm_call: { label: 'LLM', color: 'var(--k-llm)' },
  tool_call: { label: 'Tool', color: 'var(--k-tool)' },
  retrieval: { label: 'Retrieval', color: 'var(--k-retrieval)' },
  decision: { label: 'Decision', color: 'var(--k-decision)' },
  eval: { label: 'Eval', color: 'var(--k-eval)' },
  handoff: { label: 'Handoff', color: 'var(--k-handoff)' },
  agent: { label: 'Agent', color: 'var(--k-neutral)' },
  guardrail: { label: 'Guardrail', color: 'var(--k-neutral)' },
};

export function stepKind(type: string): StepKind {
  if (KINDS[type]) return KINDS[type];
  const words = type ? type.replace(/_/g, ' ') : 'other';
  return { label: words.charAt(0).toUpperCase() + words.slice(1), color: 'var(--k-neutral)' };
}

/** Fixed legend order, independent of which types a trace contains. */
export const KIND_ORDER = ['agent', 'llm_call', 'tool_call', 'retrieval', 'decision', 'eval', 'handoff', 'guardrail'];
