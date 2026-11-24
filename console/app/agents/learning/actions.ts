'use server';

import { redirect } from 'next/navigation';
import { ApiError, api } from '@/lib/api';
import { assertSameOrigin } from '@/lib/auth';
import { noteHref } from '@/lib/learning';

function message(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 401) return 'Your session expired. Sign in again.';
    if (e.status === 403 || e.status === 400 || e.status === 409 || e.status === 422) return e.message;
    if (e.status === 404) return 'That note no longer exists, or the learning loop is off for this workspace.';
  }
  return 'Something went wrong. Please try again.';
}

function back(agent: string, path: string, msg?: string, ok?: string): never {
  const href = noteHref(agent, path);
  const q = new URLSearchParams();
  if (msg) q.set('error', msg.slice(0, 300));
  if (ok) q.set('done', ok);
  redirect(q.size ? `${href}&${q.toString()}` : href);
}

const field = (form: FormData, k: string, max: number) => String(form.get(k) ?? '').slice(0, max);

/** Approve, retire or restore a skill, at the exact text the person saw. */
export async function skillAction(form: FormData): Promise<void> {
  await assertSameOrigin();
  const agent = field(form, 'agent', 128);
  const path = field(form, 'path', 200);
  const action = field(form, 'action', 16);
  const sha = field(form, 'sha256', 64);
  if (action !== 'approve' && action !== 'retire' && action !== 'restore') back(agent, path, 'Unknown action.');
  let err: string | undefined;
  try {
    await api.vaultAction(agent, path, action, sha);
  } catch (e) {
    err = message(e);
  }
  back(agent, path, err, err ? undefined : action);
}

/** A security reviewer clears a flagged skill's scan for its current text. */
export async function reviewAction(form: FormData): Promise<void> {
  await assertSameOrigin();
  const agent = field(form, 'agent', 128);
  const path = field(form, 'path', 200);
  let err: string | undefined;
  try {
    await api.vaultReview(agent, path, field(form, 'sha256', 64));
  } catch (e) {
    err = message(e);
  }
  back(agent, path, err, err ? undefined : 'review');
}

/** Save an edit (or a new note). Saving a skill makes it a draft again. */
export async function saveNoteAction(form: FormData): Promise<void> {
  await assertSameOrigin();
  const agent = field(form, 'agent', 128);
  const path = field(form, 'path', 200);
  let err: string | undefined;
  try {
    await api.saveVaultNote(agent, path, field(form, 'content', 64 << 10), field(form, 'base', 64));
  } catch (e) {
    err = message(e);
  }
  back(agent, path, err, err ? undefined : 'saved');
}
