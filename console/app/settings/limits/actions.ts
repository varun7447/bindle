'use server';

import { revalidatePath } from 'next/cache';
import { ApiError, api } from '@/lib/api';
import { assertSameOrigin } from '@/lib/auth';
import { type LimitRow, parseRows } from '@/lib/limits';

export type SaveState = { kind: 'idle' } | { kind: 'saved'; version: number } | { kind: 'error'; message: string };

export async function saveLimitsAction(_: SaveState, form: FormData): Promise<SaveState> {
  try {
    await assertSameOrigin();
    const version = Number(form.get('version'));
    if (!Number.isInteger(version) || version < 0) return { kind: 'error', message: 'Reload the page and try again.' };
    let rows: LimitRow[];
    try {
      rows = JSON.parse(String(form.get('rows') ?? '[]')) as LimitRow[];
      if (!Array.isArray(rows)) throw new Error();
    } catch {
      return { kind: 'error', message: 'Reload the page and try again.' };
    }
    const parsed = parseRows(rows.map((r) => ({
      agent: String(r.agent ?? ''), paused: r.paused === true, maxSteps: String(r.maxSteps ?? ''),
      maxTokens: String(r.maxTokens ?? ''), budget: String(r.budget ?? ''), pauseOverBudget: r.pauseOverBudget === true,
    })));
    if (parsed.error) return { kind: 'error', message: parsed.error };
    const saved = await api.saveLimits(version, parsed.limits);
    revalidatePath('/settings/limits');
    return { kind: 'saved', version: saved.version };
  } catch (e) {
    if (e instanceof ApiError) {
      if (e.status === 409) return { kind: 'error', message: 'Someone else changed these settings. Reload to see their changes.' };
      if (e.status === 403) return { kind: 'error', message: 'Only owners and admins can change agent limits.' };
      if (e.status === 400) return { kind: 'error', message: e.message };
      if (e.status === 401) return { kind: 'error', message: 'Your session expired. Sign in again.' };
    }
    return { kind: 'error', message: 'Could not save. Please try again.' };
  }
}
