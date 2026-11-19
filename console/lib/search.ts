// Search results. Pure; unit-tested.

export interface SearchHit {
  trace_id: string;
  step_id: string;
  step_name: string;
  step_type: string;
  status: string;
  agent: string;
  agent_version: string;
  session_id: string;
  start: string;
  field: string;
  snippet: string;
  score: number;
}

const PRE = '\uE000';
const POST = '\uE001';

/** A snippet as plain and highlighted segments (the API marks matches with private-use characters). */
export function segments(snippet: string): { text: string; match: boolean }[] {
  const out: { text: string; match: boolean }[] = [];
  let rest = snippet;
  while (rest) {
    const i = rest.indexOf(PRE);
    if (i < 0) {
      out.push({ text: rest, match: false });
      break;
    }
    if (i > 0) out.push({ text: rest.slice(0, i), match: false });
    const j = rest.indexOf(POST, i + 1);
    if (j < 0) {
      out.push({ text: rest.slice(i + 1), match: true });
      break;
    }
    out.push({ text: rest.slice(i + 1, j), match: true });
    rest = rest.slice(j + 1);
  }
  return out.filter((s) => s.text !== '');
}
