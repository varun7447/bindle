'use server';

import { ApiError, api } from '@/lib/api';
import { assertSameOrigin } from '@/lib/auth';
import type { ReplayMessage, ReplayResponse } from '@/lib/replay';

export type ReplayState = { kind: 'idle' } | { kind: 'done'; res: ReplayResponse } | { kind: 'error'; message: string };

const ROLES = new Set(['system', 'user', 'assistant', 'tool']);

// The provider key passes through this action to the Bindle API for one
// request. It is never stored, logged or returned.
export async function replayAction(_: ReplayState, form: FormData): Promise<ReplayState> {
  try {
    await assertSameOrigin();
    const traceId = String(form.get('trace_id') ?? '');
    const stepId = String(form.get('step_id') ?? '');
    let messages: ReplayMessage[];
    try {
      const raw = JSON.parse(String(form.get('messages') ?? '[]')) as { role?: unknown; content?: unknown }[];
      messages = raw.map((m) => ({ role: (ROLES.has(String(m.role)) ? m.role : 'user') as ReplayMessage['role'], content: String(m.content ?? '') }));
    } catch {
      return { kind: 'error', message: 'Reload the page and try again.' };
    }
    const maxTokens = Number(form.get('max_tokens') || 0);
    const res = await api.replay(traceId, stepId, {
      provider: String(form.get('provider') ?? ''),
      model: String(form.get('model') ?? '').trim(),
      api_key: String(form.get('api_key') ?? '').trim(),
      messages,
      max_tokens: Number.isInteger(maxTokens) && maxTokens > 0 ? maxTokens : 0,
    });
    return { kind: 'done', res };
  } catch (e) {
    if (e instanceof ApiError) {
      if (e.status === 400 || e.status === 502) return { kind: 'error', message: e.message };
      if (e.status === 429) return { kind: 'error', message: 'Too many replays. Try again in a minute.' };
      if (e.status === 401) return { kind: 'error', message: 'Your session expired. Sign in again.' };
      if (e.status === 503) return { kind: 'error', message: 'Replay is not configured on this Bindle.' };
    }
    return { kind: 'error', message: 'Could not run the replay. Please try again.' };
  }
}
