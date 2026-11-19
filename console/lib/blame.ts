// Which step most likely caused a run to fail (or to end badly), and why.
// Walks back from the failure using a few deterministic heuristics; no model
// call. Pure; unit-tested.

import type { Run, StepSummary } from './types';

export interface LikelyCause {
  stepId: string;
  /** 1-based position in start order, as shown in the sentence. */
  stepNumber: number;
  /** Plain sentence, e.g. "Likely cause: step 7 — lookup_order returned nothing, and step 9 answered without it." */
  text: string;
  kind: 'truncated' | 'bad_arguments' | 'repeated_failure' | 'empty_result' | 'tool_error' | 'context_overflow';
  /** The later step that used the bad result, when there is one. */
  consumerId?: string;
}

/** Finish reasons meaning the model hit its output limit. */
const CUT_OFF = new Set(['length', 'max_tokens', 'max_output_tokens', 'model_length']);

/** Errors meaning the model passed arguments the tool rejected (same words as the backend's argument-error query). */
const ARG_ERROR = /\b(invalid|missing|required|unexpected)\b.*\b(arg(ument)?s?|param(eter)?s?|field|keyword)\b|\b(arg(ument)?s?|param(eter)?s?)\b.*\b(invalid|missing|required)\b|validation ?error|schema/i;

/** Errors meaning the prompt no longer fit. */
const CONTEXT_ERROR = /context (length|window)|too many tokens|prompt is too long|maximum context|input is too long|token limit/i;

/** A prompt this many times larger than the previous call's, and at least this many tokens, is a jump. */
const JUMP_RATIO = 3;
const JUMP_MIN_TOKENS = 20_000;
/** Tool results at least this large are carried into every later prompt. */
const BIG_RESULT_BYTES = 50_000;

function short(s: string, max = 100): string {
  const t = s.replace(/\s+/g, ' ').trim();
  return t.length > max ? t.slice(0, max - 1) + '…' : t;
}

const label = (s: StepSummary) => s.tool_name || s.name;
const ms = (s: StepSummary) => Date.parse(s.start_time);
const endMs = (s: StepSummary) => ms(s) + Math.max(0, s.duration_ms);

/**
 * The most likely causing step, or null when nothing stands out beyond the
 * first failed step (which the run summary already names).
 */
export function likelyCause(run: Run, steps: StepSummary[]): LikelyCause | null {
  const work = steps.filter((s) => s.type !== 'agent' && s.type !== 'eval');
  if (!work.length) return null;
  const num = new Map(steps.map((s, i) => [s.step_id, i + 1]));
  const n = (s: StepSummary) => num.get(s.step_id)!;
  const cause = (s: StepSummary, kind: LikelyCause['kind'], what: string, consumer?: StepSummary): LikelyCause => ({
    stepId: s.step_id,
    stepNumber: n(s),
    kind,
    text: `Likely cause: step ${n(s)} — ${what}.`,
    ...(consumer ? { consumerId: consumer.step_id } : {}),
  });

  const failures = work.filter((s) => s.status === 'failure');
  const failed = run.status === 'failed' || failures.length > 0;
  const firstFailure = failures[0];
  // The step the run went wrong at: the first failure, else the last step.
  const target = firstFailure ?? work[work.length - 1]!;
  const before = (s: StepSummary) => n(s) <= n(target);
  const llmCalls = work.filter((s) => s.type === 'llm_call');

  // 1. The model's answer was cut off at its output limit.
  const cut = llmCalls.filter((s) => CUT_OFF.has(s.finish_reason ?? '') && (before(s) || !failed));
  if (cut.length) {
    const s = cut[cut.length - 1]!;
    const lastLLM = llmCalls[llmCalls.length - 1];
    if (failed || s === lastLLM) {
      return cause(s, 'truncated', `${label(s)} hit its output token limit (finish reason “${s.finish_reason}”), so its answer was cut off`);
    }
  }
  if (!failed) return null;

  // 2. The prompt no longer fit.
  const overflow = failures.find((s) => s.type === 'llm_call' && CONTEXT_ERROR.test(s.error_message));
  if (overflow) {
    const big = work.filter((s) => s.type !== 'llm_call' && n(s) < n(overflow) && s.output_bytes >= BIG_RESULT_BYTES).sort((a, b) => b.output_bytes - a.output_bytes)[0];
    if (big) {
      return cause(big, 'context_overflow', `${label(big)} returned ${Math.round(big.output_bytes / 1000)} KB, and step ${n(overflow)} then failed because the prompt was too long`, overflow);
    }
    return cause(overflow, 'context_overflow', `the prompt for ${label(overflow)} was too long: “${short(overflow.error_message)}”`);
  }

  // 3. The model called a tool with arguments it rejected.
  const badArgs = failures.find((s) => (s.type === 'tool_call' || s.type === 'retrieval') && ARG_ERROR.test(s.error_message));
  if (badArgs) {
    return cause(badArgs, 'bad_arguments', `the model called ${label(badArgs)} with arguments it rejected: “${short(badArgs.error_message)}”`);
  }

  // 4. The same call failed the same way again and again.
  const groups = new Map<string, StepSummary[]>();
  for (const s of failures) {
    const key = `${label(s)}\u0000${s.error_message}`;
    groups.set(key, [...(groups.get(key) ?? []), s]);
  }
  const repeated = [...groups.values()].filter((g) => g.length >= 2).sort((a, b) => b.length - a.length || n(a[0]!) - n(b[0]!))[0];
  if (repeated) {
    const s = repeated[0]!;
    const err = s.error_message ? `: “${short(s.error_message)}”` : '';
    return cause(s, 'repeated_failure', `${label(s)} failed the same way ${repeated.length} times${err}; the retries repeated the same call`);
  }

  // 5. A tool came back empty (or failed) and a later LLM call carried on without it.
  // Only when content is captured: with capture off every size is 0.
  const captured = work.some((s) => s.output_bytes > 0 || s.input_bytes > 0);
  const tools = work.filter((s) => (s.type === 'tool_call' || s.type === 'retrieval') && before(s));
  for (let i = tools.length - 1; i >= 0; i--) {
    const t = tools[i]!;
    const empty = captured && t.status !== 'failure' && t.output_bytes === 0;
    const errored = t.status === 'failure';
    if (!empty && !errored) continue;
    // a later LLM call that started after the tool finished, with no successful retry of the tool in between
    const consumer = llmCalls.find((l) => ms(l) >= endMs(t) && n(l) > n(t));
    if (!consumer) continue;
    const retried = work.some((s) => label(s) === label(t) && s.status !== 'failure' && n(s) > n(t) && n(s) < n(consumer) && (s.output_bytes > 0 || !captured));
    if (retried) continue;
    if (empty) {
      return cause(t, 'empty_result', `${label(t)} returned nothing, and step ${n(consumer)} answered without it`, consumer);
    }
    const err = t.error_message ? ` (“${short(t.error_message, 80)}”)` : '';
    return cause(t, 'tool_error', `${label(t)} failed${err}, and step ${n(consumer)} carried on without its result`, consumer);
  }

  // 6. A failing LLM call right after its prompt jumped in size.
  const failedLLM = failures.find((s) => s.type === 'llm_call');
  if (failedLLM) {
    const prev = [...llmCalls].reverse().find((s) => n(s) < n(failedLLM) && s.input_tokens > 0);
    if (prev && failedLLM.input_tokens >= JUMP_MIN_TOKENS && failedLLM.input_tokens >= JUMP_RATIO * prev.input_tokens) {
      return cause(
        failedLLM,
        'context_overflow',
        `its prompt jumped from ${prev.input_tokens.toLocaleString('en-US')} to ${failedLLM.input_tokens.toLocaleString('en-US')} tokens before it failed`,
      );
    }
  }
  return null;
}
