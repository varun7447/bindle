// Outcome-graded quality (feature "outcome_grading"): was the agent actually
// right, judged by what happened afterwards in the business? Pure helpers;
// unit-tested.

export const OUTCOME_GRADING = 'outcome_grading';

export interface ReversedRun {
  trace_id: string;
  outcome: string;
}

export interface Grade {
  agent: string;
  version: string;
  runs: number;
  successes: number;
  success_rate: number;
  evaluated_runs: number;
  passed_evals: number;
  reversed: number;
  reversed_after_passing: number;
  top_reversal?: string;
  headline: string;
  reversed_runs: ReversedRun[];
}

export interface GradingResponse {
  window: string;
  grades: Grade[];
  truncated: boolean;
}

export interface OutcomeSettings {
  reversal_outcomes: string[];
  pending_hours: number;
  updated_at?: string;
}

export interface OutcomeEndpoint {
  id: string;
  name: string;
  created_by?: string;
  created_at: string;
  revoked_at?: string;
}

export type OutcomeEventStatus = 'pending' | 'matched' | 'unmatched';

export interface OutcomeEvent {
  id: string;
  correlation_keys: string[];
  name: string;
  success: boolean;
  value?: number;
  comment?: string;
  source: 'api' | 'webhook';
  endpoint_id?: string;
  occurred_at: string;
  received_at: string;
  status: OutcomeEventStatus;
  attempts: number;
  next_attempt_at: string;
  expires_at: string;
  matched_traces: string[];
  matched_at?: string;
}

/** Whether the workspace has outcome grading on (from GET /api/features). */
export function gradingOn(features: string[] | undefined): boolean {
  return !!features?.includes(OUTCOME_GRADING);
}

/** "ticket_reopened" → "ticket reopened". */
export function outcomeLabel(name: string): string {
  return name.replace(/_/g, ' ');
}

/** Parses the reversal list typed in Settings: one name per line or comma-separated. */
export function parseReversals(text: string): { names: string[]; error?: string } {
  const names: string[] = [];
  for (const raw of text.split(/[\n,]/)) {
    const n = raw.trim();
    if (!n) continue;
    if (n.length > 80 || /[\u0000-\u001f\u007f]/.test(n)) return { names: [], error: `“${n.slice(0, 40)}” must be 1-80 characters on one line.` };
    if (!names.includes(n)) names.push(n);
  }
  if (names.length > 50) return { names: [], error: 'At most 50 reversal outcomes.' };
  return { names: names.sort() };
}

/** One sentence for an event's state, for Settings → Outcomes. */
export function eventState(e: OutcomeEvent, now = Date.now()): string {
  switch (e.status) {
    case 'matched':
      return `Matched to ${e.matched_traces.length} ${e.matched_traces.length === 1 ? 'run' : 'runs'}.`;
    case 'unmatched':
      return 'No run carried this correlation before the retry window ended.';
    default: {
      const left = Date.parse(e.expires_at) - now;
      const hours = Math.max(0, Math.round(left / 3_600_000));
      return `Waiting for a matching run; retried for ${hours < 1 ? 'less than an hour' : `${hours} more ${hours === 1 ? 'hour' : 'hours'}`}.`;
    }
  }
}

/** The grade worth leading with: the newest version with a reversal, else the newest version. */
export function leadGrade(grades: Grade[]): Grade | undefined {
  return grades.find((g) => g.reversed > 0) ?? grades[0];
}
