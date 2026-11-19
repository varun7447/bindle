'use server';

import { revalidatePath } from 'next/cache';
import { redirect } from 'next/navigation';
import { ApiError, api } from '@/lib/api';
import { assertSameOrigin } from '@/lib/auth';
import { parseScorerForm } from '@/lib/evals';
import { isTraceId } from '@/lib/ids';
import { parseChecks, parseProblemSource, problemTitle } from '@/lib/outcomes';

export type EvalActionState = { kind: 'idle' } | { kind: 'done'; message: string } | { kind: 'error'; message: string };

function message(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 403) return 'Only owners and admins can do that.';
    if (e.status === 401) return 'Your session expired. Sign in again.';
    if (e.status === 404) return 'That item no longer exists. Reload the page.';
    if (e.status === 409) return 'That name is already used.';
    if (e.status === 400) return e.message; // the API's own validation text
    if (e.status === 503) return 'Evals are not configured on this deployment.';
  }
  return 'Something went wrong. Please try again.';
}

async function run(paths: string[], fn: () => Promise<EvalActionState>): Promise<EvalActionState> {
  try {
    await assertSameOrigin();
    const out = await fn();
    for (const p of paths) revalidatePath(p);
    return out;
  } catch (e) {
    return { kind: 'error', message: message(e) };
  }
}

const s = (form: FormData, k: string) => String(form.get(k) ?? '');

export async function createScorerAction(_: EvalActionState, form: FormData): Promise<EvalActionState> {
  return run(['/evals'], async () => {
    const p = parseScorerForm({
      name: s(form, 'name'), kind: s(form, 'kind'), agent: s(form, 'agent'), value: s(form, 'value'), number: s(form, 'number'),
      case_sensitive: form.get('case_sensitive') === 'on',
    });
    if (p.error || !p.scorer) return { kind: 'error', message: p.error ?? 'Check the form.' };
    await api.createScorer({ ...p.scorer, enabled: true });
    return { kind: 'done', message: 'Check added.' };
  });
}

export async function scorerAction(_: EvalActionState, form: FormData): Promise<EvalActionState> {
  return run(['/evals'], async () => {
    const id = s(form, 'id');
    if (s(form, 'op') === 'delete') {
      await api.deleteScorer(id);
      return { kind: 'done', message: 'Check deleted.' };
    }
    const cur = (await api.scorers()).scorers.find((x) => x.id === id);
    if (!cur) return { kind: 'error', message: 'That check no longer exists. Reload the page.' };
    await api.updateScorer(id, { name: cur.name, agent: cur.agent, value: cur.value, number: cur.number, case_sensitive: cur.case_sensitive, enabled: !cur.enabled });
    return { kind: 'done', message: cur.enabled ? 'Check turned off.' : 'Check turned on.' };
  });
}

export async function createDatasetAction(_: EvalActionState, form: FormData): Promise<EvalActionState> {
  let id = '';
  const out = await run(['/evals'], async () => {
    const d = await api.createDataset(s(form, 'name'), s(form, 'description'));
    id = d.id;
    return { kind: 'done', message: 'Dataset created.' };
  });
  if (id) redirect(`/evals/datasets/${id}`);
  return out;
}

/** Adds runs matching a filter (up to 100, newest first). */
export async function addRunsAction(_: EvalActionState, form: FormData): Promise<EvalActionState> {
  const id = s(form, 'dataset_id');
  return run([`/evals/datasets/${id}`, '/evals'], async () => {
    const days = { '24h': 1, '7d': 7, '30d': 30 }[s(form, 'range')] ?? 7;
    const status = s(form, 'status');
    const limit = Math.min(Math.max(Number(s(form, 'limit')) || 20, 1), 100);
    const runs = await api.runs({
      from: new Date(Date.now() - days * 86_400_000).toISOString(),
      to: new Date().toISOString(),
      limit: String(limit),
      ...(s(form, 'agent') ? { agent: s(form, 'agent') } : {}),
      ...(status === 'failed' || status === 'ok' ? { status } : {}),
    });
    const ids = runs.runs.map((r) => r.trace_id);
    if (ids.length === 0) return { kind: 'error', message: 'No runs match that filter.' };
    const res = await api.addDatasetItems(id, ids);
    return { kind: 'done', message: `Added ${res.added} run${res.added === 1 ? '' : 's'}${res.added < res.found ? ` (${res.found - res.added} already in the dataset)` : ''}.` };
  });
}

/** Adds one run (from the run page). */
export async function addRunAction(_: EvalActionState, form: FormData): Promise<EvalActionState> {
  const id = s(form, 'dataset_id');
  const trace = s(form, 'trace_id');
  return run([`/evals/datasets/${id}`], async () => {
    if (!isTraceId(trace)) return { kind: 'error', message: 'Invalid run.' };
    if (!id) return { kind: 'error', message: 'Choose a dataset.' };
    const res = await api.addDatasetItems(id, [trace]);
    return { kind: 'done', message: res.added ? 'Added to the dataset.' : 'Already in the dataset.' };
  });
}

/** Adds several runs (search results) to a dataset. */
export async function addTracesAction(_: EvalActionState, form: FormData): Promise<EvalActionState> {
  const id = s(form, 'dataset_id');
  return run([`/evals/datasets/${id}`], async () => {
    let ids: string[];
    try {
      ids = (JSON.parse(s(form, 'trace_ids')) as unknown[]).map(String).filter(isTraceId).slice(0, 100);
    } catch {
      return { kind: 'error', message: 'Reload the page and try again.' };
    }
    if (!id || ids.length === 0) return { kind: 'error', message: 'Choose a dataset.' };
    const res = await api.addDatasetItems(id, ids);
    return { kind: 'done', message: `Added ${res.added} run${res.added === 1 ? '' : 's'}.` };
  });
}

/**
 * Copies problem runs (a failure group, disliked runs, failed scores) into
 * an existing or new dataset, and saves the suggested checks the user kept.
 */
export async function addProblemsAction(_: EvalActionState, form: FormData): Promise<EvalActionState> {
  const src = parseProblemSource(Object.fromEntries(['kind', 'step_name', 'pattern', 'agent', 'score', 'window'].map((k) => [k, s(form, k)])));
  let id = s(form, 'dataset_id');
  const out = await run(['/evals'], async () => {
    if (!src) return { kind: 'error', message: 'Reload the page and try again.' };
    const checks = parseChecks(form.getAll('check').map(String));
    if (id === 'new') {
      const name = s(form, 'new_name').trim();
      if (!name) return { kind: 'error', message: 'Name the new dataset.' };
      id = (await api.createDataset(name, `From production: ${problemTitle(src)}`.slice(0, 500))).id;
    }
    if (!id) return { kind: 'error', message: 'Choose a dataset.' };
    const res = await api.addProblemItems(id, src);
    let saved = 0;
    for (const c of checks) {
      try {
        await api.createScorer({ ...c, enabled: true });
        saved++;
      } catch (e) {
        if (!(e instanceof ApiError && e.status === 409)) throw e; // already saved under that name
      }
    }
    return {
      kind: 'done',
      message: `Added ${res.added} run${res.added === 1 ? '' : 's'}${res.added < res.found ? ` (${res.found - res.added} already there)` : ''}${saved ? `, saved ${saved} check${saved === 1 ? '' : 's'}` : ''}.`,
    };
  });
  if (out.kind === 'done' && id && id !== 'new') redirect(`/evals/datasets/${id}`);
  return out;
}

export async function datasetAction(_: EvalActionState, form: FormData): Promise<EvalActionState> {
  const id = s(form, 'dataset_id');
  const op = s(form, 'op');
  if (op === 'delete') {
    const out = await run(['/evals'], async () => {
      await api.deleteDataset(id);
      return { kind: 'done', message: 'Dataset deleted.' };
    });
    if (out.kind === 'done') redirect('/evals');
    return out;
  }
  return run([`/evals/datasets/${id}`, '/evals'], async () => {
    if (op === 'remove') {
      await api.removeDatasetItem(id, s(form, 'trace_id'));
      return { kind: 'done', message: 'Removed from the dataset.' };
    }
    const ev = await api.evaluateDataset(id);
    const graded = ev.tallies.reduce((n, t) => n + t.passed + t.failed, 0);
    if (ev.tallies.length === 0) return { kind: 'error', message: 'Add a check first: there is nothing to run.' };
    return { kind: 'done', message: `Ran ${ev.tallies.length} check${ev.tallies.length === 1 ? '' : 's'} on ${ev.items} run${ev.items === 1 ? '' : 's'} (${graded} results).` };
  });
}
