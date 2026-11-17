'use server';

import { revalidatePath } from 'next/cache';
import { ApiError, api } from '@/lib/api';
import { assertSameOrigin } from '@/lib/auth';
import { validGrace } from '@/lib/keys';

export type KeyActionState =
  | { kind: 'idle' }
  | { kind: 'created'; name: string; secret: string; note?: string }
  | { kind: 'error'; message: string };

function message(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 403) return 'Only owners and admins can manage keys.';
    if (e.status === 401) return 'Your session expired. Sign in again.';
    if (e.status === 404) return 'That key no longer exists or was already rotated.';
    if (e.status === 400) return e.message;
  }
  return 'Something went wrong. Please try again.';
}

// Server Actions: Next.js rejects cross-origin calls; assertSameOrigin also
// requires an Origin header when sign-in is enabled.

export async function createKeyAction(_: KeyActionState, form: FormData): Promise<KeyActionState> {
  try {
    await assertSameOrigin();
    const name = String(form.get('name') ?? '').slice(0, 100);
    const res = await api.createKey(name);
    revalidatePath('/keys');
    return { kind: 'created', name: res.key.name, secret: res.secret };
  } catch (e) {
    return { kind: 'error', message: message(e) };
  }
}

export async function rotateKeyAction(_: KeyActionState, form: FormData): Promise<KeyActionState> {
  try {
    await assertSameOrigin();
    const grace = validGrace(form.get('grace'));
    if (grace === null) return { kind: 'error', message: 'Choose when the old key should stop working.' };
    const res = await api.rotateKey(String(form.get('id') ?? ''), grace);
    revalidatePath('/keys');
    const note = grace === 0 ? 'The old key stops working within a minute.' : `The old key keeps working for ${grace} hour${grace === 1 ? '' : 's'}.`;
    return { kind: 'created', name: res.key.name, secret: res.secret, note };
  } catch (e) {
    return { kind: 'error', message: message(e) };
  }
}

export async function revokeKeyAction(_: KeyActionState, form: FormData): Promise<KeyActionState> {
  try {
    await assertSameOrigin();
    await api.revokeKey(String(form.get('id') ?? ''));
    revalidatePath('/keys');
    return { kind: 'idle' };
  } catch (e) {
    return { kind: 'error', message: message(e) };
  }
}
