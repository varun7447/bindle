'use server';

import { revalidatePath } from 'next/cache';
import { ApiError, api } from '@/lib/api';
import { assertSameOrigin } from '@/lib/auth';
import { parseReversals } from '@/lib/grading';

export type OutcomeActionState =
  | { kind: 'idle' }
  | { kind: 'saved' }
  | { kind: 'created'; name: string; secret: string; path: string; header: string }
  | { kind: 'error'; message: string };

function message(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 403) return 'Only owners and admins can change outcome settings.';
    if (e.status === 401) return 'Your session expired. Sign in again.';
    if (e.status === 404) return 'That endpoint no longer exists, or outcome grading is off for this workspace.';
    if (e.status === 400) return e.message;
  }
  return 'Something went wrong. Please try again.';
}

export async function saveOutcomeSettingsAction(_: OutcomeActionState, form: FormData): Promise<OutcomeActionState> {
  try {
    await assertSameOrigin();
    const { names, error } = parseReversals(String(form.get('reversals') ?? '').slice(0, 10_000));
    if (error) return { kind: 'error', message: error };
    const hours = Number(form.get('pending_hours'));
    if (!Number.isInteger(hours) || hours < 1 || hours > 168) return { kind: 'error', message: 'Retry window must be 1 to 168 hours.' };
    await api.putOutcomeSettings({ reversal_outcomes: names, pending_hours: hours });
    revalidatePath('/settings/outcomes');
    return { kind: 'saved' };
  } catch (e) {
    return { kind: 'error', message: message(e) };
  }
}

export async function createEndpointAction(_: OutcomeActionState, form: FormData): Promise<OutcomeActionState> {
  try {
    await assertSameOrigin();
    const name = String(form.get('name') ?? '').trim().slice(0, 80);
    if (!name) return { kind: 'error', message: 'Name the endpoint, for example “Helpdesk”.' };
    const res = await api.createOutcomeEndpoint(name);
    revalidatePath('/settings/outcomes');
    return { kind: 'created', name: res.endpoint.name, secret: res.secret, path: res.path, header: res.signature_header };
  } catch (e) {
    return { kind: 'error', message: message(e) };
  }
}

export async function revokeEndpointAction(_: OutcomeActionState, form: FormData): Promise<OutcomeActionState> {
  try {
    await assertSameOrigin();
    await api.revokeOutcomeEndpoint(String(form.get('id') ?? ''));
    revalidatePath('/settings/outcomes');
    return { kind: 'idle' };
  } catch (e) {
    return { kind: 'error', message: message(e) };
  }
}
