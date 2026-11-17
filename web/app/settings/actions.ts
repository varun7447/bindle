'use server';

import { revalidatePath } from 'next/cache';
import { ApiError, api } from '@/lib/api';
import { assertSameOrigin } from '@/lib/auth';
import { parsePatterns, parsePercent } from '@/lib/sdksettings';

export type SaveState = { kind: 'idle' } | { kind: 'saved'; version: number } | { kind: 'error'; message: string };

export async function saveSettingsAction(_: SaveState, form: FormData): Promise<SaveState> {
  try {
    await assertSameOrigin();
    const rate = parsePercent(form.get('sample_percent'));
    if (rate === null) return { kind: 'error', message: 'Sample rate must be a number from 0 to 100.' };
    const parsed = parsePatterns(String(form.get('patterns') ?? ''));
    if (parsed.error) return { kind: 'error', message: parsed.error };
    const version = Number(form.get('version'));
    if (!Number.isInteger(version) || version < 0) return { kind: 'error', message: 'Reload the page and try again.' };
    const saved = await api.saveConfig({
      version,
      sample_rate: rate,
      capture_content: form.get('capture_content') === 'on',
      redact_patterns: parsed.patterns,
    });
    revalidatePath('/settings');
    return { kind: 'saved', version: saved.version };
  } catch (e) {
    if (e instanceof ApiError) {
      if (e.status === 409) return { kind: 'error', message: 'Someone else changed these settings. Reload to see their changes.' };
      if (e.status === 403) return { kind: 'error', message: 'Only owners and admins can change SDK settings.' };
      if (e.status === 400) return { kind: 'error', message: e.message };
      if (e.status === 401) return { kind: 'error', message: 'Your session expired. Sign in again.' };
    }
    return { kind: 'error', message: 'Could not save. Please try again.' };
  }
}
