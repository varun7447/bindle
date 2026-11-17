// Why a step's input or output is empty, in words a user can act on.
// Pure; unit-tested.

import type { StepDetail } from './types';

export interface EmptyReason {
  title: string;
  detail?: string;
  /** Console page that fixes it, if any. */
  href?: string;
  /** Setting to copy, if any. */
  code?: string;
}

export const DROPPED_ATTR = 'bindle.content_dropped';

/** Frameworks that leave content out of traces unless switched on. */
const FRAMEWORK_SWITCH: Record<string, { what: string; code: string }> = {
  'Microsoft Agent Framework': {
    what: 'Microsoft Agent Framework leaves prompts, outputs and tool data out of traces unless sensitive data is enabled.',
    code: 'ENABLE_SENSITIVE_DATA=true',
  },
  'Google ADK': {
    what: 'Google ADK sends prompts and responses as log events, not on the trace, unless told otherwise.',
    code: 'OTEL_SEMCONV_STABILITY_OPT_IN=gen_ai_latest_experimental\nOTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=SPAN_ONLY',
  },
  'Microsoft Foundry Agent Service': {
    what: 'The Foundry Agent Service SDK leaves message content out of traces unless content recording is enabled.',
    code: 'AIAgentsInstrumentor().instrument(enable_content_recording=True)',
  },
};

const NO_CONTENT_OF_ITS_OWN = new Set(['agent', 'decision', 'handoff', 'guardrail']);

export function emptyReason(step: Pick<StepDetail, 'type' | 'framework' | 'attributes' | 'input' | 'output'>, field: 'input' | 'output'): EmptyReason {
  const dropped = step.attributes?.[DROPPED_ATTR];
  if (dropped === 'workspace') {
    return {
      title: 'Not stored: content capture is off for this workspace.',
      detail: 'Owners and admins can turn it on in SDK settings. It applies to new runs.',
      href: '/settings/sdk',
    };
  }
  if (dropped === 'sdk') {
    return {
      title: "Not sent: content capture is off in this agent's code.",
      detail: 'Remove BINDLE_CAPTURE_CONTENT=false from its environment (or capture_content=False from bindle.init) to send prompts and outputs.',
    };
  }
  const other = field === 'input' ? step.output : step.input;
  const sw = FRAMEWORK_SWITCH[step.framework];
  if (sw && !other) {
    return { title: `Not sent by ${step.framework}.`, detail: `${sw.what} Set this in the agent's environment:`, code: sw.code };
  }
  if (NO_CONTENT_OF_ITS_OWN.has(step.type)) {
    return {
      title: `This step has no ${field} of its own.`,
      detail: step.type === 'agent' ? 'Its child steps (LLM and tool calls) hold the details.' : undefined,
    };
  }
  return { title: `The agent didn't send ${field === 'input' ? 'an input' : 'an output'} for this step.` };
}

/** The same reason, worded for input and output together. */
export function bothEmpty(r: EmptyReason): EmptyReason {
  return {
    ...r,
    title: r.title
      .replace('This step has no input of its own.', 'This step has no input or output of its own.')
      .replace("The agent didn't send an input for this step.", "The agent didn't send an input or output for this step."),
  };
}
