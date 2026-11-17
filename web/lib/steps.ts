// Step-type identity. Colors are the product spec's step colors; types the
// spec does not name (agent, guardrail, other) use a neutral gray.
//
// Color is a secondary cue only: the dataviz validator shows these six hues
// cannot all be told apart (llm vs decision under protanopia, tool vs eval
// even with full color vision), so every place a step appears also prints its
// type label.

export interface StepKind {
  label: string;
  color: string;
}

const KINDS: Record<string, StepKind> = {
  llm_call: { label: 'LLM', color: '#6CA8FF' },
  tool_call: { label: 'Tool', color: '#F2A65A' },
  decision: { label: 'Decision', color: '#B79CFF' },
  retrieval: { label: 'Retrieval', color: '#7FD4E8' },
  eval: { label: 'Eval', color: '#F5C26B' },
  handoff: { label: 'Handoff', color: '#E58AD0' },
  agent: { label: 'Agent', color: '#8B949E' },
  guardrail: { label: 'Guardrail', color: '#8B949E' },
};

export function stepKind(type: string): StepKind {
  return KINDS[type] ?? { label: type ? type.replace(/_/g, ' ') : 'Other', color: '#8B949E' };
}

/** Fixed legend order, independent of which types a trace contains. */
export const KIND_ORDER = ['agent', 'llm_call', 'tool_call', 'retrieval', 'decision', 'eval', 'handoff', 'guardrail'];
