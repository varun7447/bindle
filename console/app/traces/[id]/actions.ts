'use server';

import { revalidatePath } from 'next/cache';
import { ApiError, api } from '@/lib/api';
import { assertSameOrigin } from '@/lib/auth';
import { isTraceId } from '@/lib/ids';

export type StopState = { kind: 'idle' } | { kind: 'stopped'; at?: string; pollSeconds: number } | { kind: 'error'; message: string };

// Records a stop for the run. The agent's next bindle.check_limits() call
// after its settings refresh raises; nothing else is interrupted.
export async function stopRunAction(_: StopState, form: FormData): Promise<StopState> {
  try {
    await assertSameOrigin();
    const traceId = String(form.get('trace_id') ?? '');
    if (!isTraceId(traceId)) return { kind: 'error', message: 'Invalid run.' };
    const res = await api.stopRun(traceId);
    revalidatePath(`/traces/${traceId}`);
    return { kind: 'stopped', at: res.stopped_at, pollSeconds: res.poll_seconds };
  } catch (e) {
    if (e instanceof ApiError) {
      if (e.status === 403) return { kind: 'error', message: 'Only owners and admins can stop runs.' };
      if (e.status === 401) return { kind: 'error', message: 'Your session expired. Sign in again.' };
      if (e.status === 503) return { kind: 'error', message: 'Stopping runs is not configured on this Bindle.' };
    }
    return { kind: 'error', message: 'Could not stop the run. Try again.' };
  }
}
