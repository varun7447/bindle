'use server';

import { revalidatePath } from 'next/cache';
import { ApiError, api } from '@/lib/api';
import { assertSameOrigin } from '@/lib/auth';
import { isChannelKind, isRuleKind, parseRule, type RuleKind } from '@/lib/alerting';

export type AlertActionState =
  | { kind: 'idle' }
  | { kind: 'done'; message: string; secret?: string }
  | { kind: 'error'; message: string };

function message(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 403) return 'Only owners and admins can change alerts.';
    if (e.status === 401) return 'Your session expired. Sign in again.';
    if (e.status === 404) return 'That item no longer exists. Reload the page.';
    if (e.status === 400) return e.message; // the API's own validation text
    if (e.status === 503) return 'Alerting is not configured on this deployment.';
  }
  return 'Something went wrong. Please try again.';
}

async function run(fn: () => Promise<AlertActionState>): Promise<AlertActionState> {
  try {
    await assertSameOrigin();
    const out = await fn();
    revalidatePath('/alerts');
    return out;
  } catch (e) {
    return { kind: 'error', message: message(e) };
  }
}

export async function createRuleAction(_: AlertActionState, form: FormData): Promise<AlertActionState> {
  return run(async () => {
    const kind = form.get('kind');
    if (!isRuleKind(kind)) return { kind: 'error', message: 'Choose what to alert on.' };
    const parsed = parseRule(kind, form.get('threshold'), form.get('window'), form.get('min_runs'));
    if ('error' in parsed) return { kind: 'error', message: parsed.error };
    await api.createAlertRule({ kind, ...parsed, enabled: true });
    return { kind: 'done', message: 'Rule added.' };
  });
}

export async function updateRuleAction(_: AlertActionState, form: FormData): Promise<AlertActionState> {
  return run(async () => {
    const kind = form.get('kind') as RuleKind;
    if (!isRuleKind(kind)) return { kind: 'error', message: 'Reload the page and try again.' };
    const parsed = parseRule(kind, form.get('threshold'), form.get('window'), form.get('min_runs'));
    if ('error' in parsed) return { kind: 'error', message: parsed.error };
    await api.updateAlertRule(String(form.get('id') ?? ''), { ...parsed, enabled: form.get('enabled') === 'on' });
    return { kind: 'done', message: 'Saved.' };
  });
}

export async function createChannelAction(_: AlertActionState, form: FormData): Promise<AlertActionState> {
  return run(async () => {
    const kind = form.get('kind');
    if (!isChannelKind(kind)) return { kind: 'error', message: 'Choose a channel type.' };
    const name = String(form.get('name') ?? '').trim().slice(0, 100);
    const target = String(form.get('target') ?? '').trim().slice(0, 2000);
    const res = await api.createAlertChannel({ kind, name: name || kind, target });
    return res.signing_secret
      ? { kind: 'done', message: `Added “${res.channel.name}”.`, secret: res.signing_secret }
      : { kind: 'done', message: `Added “${res.channel.name}”. Send a test to check it.` };
  });
}

export async function channelAction(_: AlertActionState, form: FormData): Promise<AlertActionState> {
  return run(async () => {
    const id = String(form.get('id') ?? '');
    switch (form.get('op')) {
      case 'test': {
        const res = await api.testAlertChannel(id);
        return res.ok ? { kind: 'done', message: 'Test alert sent.' } : { kind: 'error', message: `Test failed: ${res.error ?? 'unknown error'}.` };
      }
      case 'pause':
        await api.setAlertChannelEnabled(id, false);
        return { kind: 'done', message: 'Paused.' };
      case 'resume':
        await api.setAlertChannelEnabled(id, true);
        return { kind: 'done', message: 'Resumed.' };
      case 'delete':
        await api.deleteAlertChannel(id);
        return { kind: 'done', message: 'Deleted.' };
    }
    return { kind: 'error', message: 'Unknown action.' };
  });
}
