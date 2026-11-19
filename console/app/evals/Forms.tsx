'use client';

import { useActionState, useState } from 'react';
import { KINDS } from '@/lib/evals';
import { type EvalActionState, addRunAction, addRunsAction, addTracesAction, createDatasetAction, createScorerAction, datasetAction, scorerAction } from './actions';

const idle: EvalActionState = { kind: 'idle' };

function Status({ state }: { state: EvalActionState }) {
  if (state.kind === 'idle') return null;
  return state.kind === 'error' ? (
    <span className="sub" role="alert" style={{ color: 'var(--critical-text)' }}>
      {state.message}
    </span>
  ) : (
    <span className="sub" role="status">
      <span aria-hidden="true" style={{ color: 'var(--good)' }}>✓</span> {state.message}
    </span>
  );
}

export function NewCheckForm({ agents }: { agents: string[] }) {
  const [state, action, pending] = useActionState(createScorerAction, idle);
  const [kind, setKind] = useState('contains');
  const k = KINDS[kind]!;
  return (
    <form action={action} className="eval-form">
      <datalist id="eval-agents">
        {agents.map((a) => (
          <option key={a} value={a} />
        ))}
      </datalist>
      <label>
        Name <input name="name" required maxLength={80} placeholder="e.g. mentions the refund" />
      </label>
      <label>
        Check{' '}
        <select name="kind" value={kind} onChange={(e) => setKind(e.target.value)}>
          {Object.entries(KINDS).map(([v, info]) => (
            <option key={v} value={v}>
              {info.label}
            </option>
          ))}
        </select>
      </label>
      {k.input === 'text' || k.input === 'pattern' || k.input === 'tool' ? (
        <label>
          {k.input === 'tool' ? 'Tool' : k.input === 'pattern' ? 'Pattern (RE2)' : 'Text'}{' '}
          <input name="value" required maxLength={500} spellCheck={false} className={k.input === 'pattern' ? 'mono' : undefined} />
        </label>
      ) : null}
      {k.input === 'number' ? (
        <label>
          Limit ({k.unit}) <input name="number" required inputMode="decimal" style={{ width: 100 }} />
        </label>
      ) : null}
      {k.input === 'text' || k.input === 'pattern' ? (
        <label className="inline">
          <input type="checkbox" name="case_sensitive" /> Case-sensitive
        </label>
      ) : null}
      <label>
        Agent <input name="agent" list="eval-agents" placeholder="all agents" style={{ width: 150 }} />
      </label>
      <button type="submit" className="btn btn-primary" disabled={pending}>
        {pending ? 'Adding…' : 'Add check'}
      </button>
      <Status state={state} />
    </form>
  );
}

export function CheckButtons({ id, enabled, canEdit }: { id: string; enabled: boolean; canEdit: boolean }) {
  const [state, action, pending] = useActionState(scorerAction, idle);
  if (!canEdit) return null;
  return (
    <form action={action} style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
      <input type="hidden" name="id" value={id} />
      <button type="submit" name="op" value="toggle" className="btn btn-sm" disabled={pending}>
        {enabled ? 'Turn off' : 'Turn on'}
      </button>
      <button type="submit" name="op" value="delete" className="btn btn-sm" disabled={pending} aria-label="Delete check">
        Delete
      </button>
      {state.kind === 'error' ? <Status state={state} /> : null}
    </form>
  );
}

export function NewDatasetForm() {
  const [state, action, pending] = useActionState(createDatasetAction, idle);
  return (
    <form action={action} className="eval-form">
      <label>
        Name <input name="name" required maxLength={80} placeholder="e.g. refund regressions" />
      </label>
      <label>
        Description <input name="description" maxLength={500} placeholder="optional" style={{ width: 280 }} />
      </label>
      <button type="submit" className="btn btn-primary" disabled={pending}>
        {pending ? 'Creating…' : 'Create dataset'}
      </button>
      <Status state={state} />
    </form>
  );
}

export function AddRunsForm({ datasetId, agents }: { datasetId: string; agents: string[] }) {
  const [state, action, pending] = useActionState(addRunsAction, idle);
  return (
    <form action={action} className="eval-form">
      <input type="hidden" name="dataset_id" value={datasetId} />
      <label>
        Agent{' '}
        <select name="agent" defaultValue="">
          <option value="">Any agent</option>
          {agents.map((a) => (
            <option key={a} value={a}>
              {a}
            </option>
          ))}
        </select>
      </label>
      <label>
        Runs{' '}
        <select name="status" defaultValue="">
          <option value="">All</option>
          <option value="failed">Failed</option>
          <option value="ok">Succeeded</option>
        </select>
      </label>
      <label>
        From the last{' '}
        <select name="range" defaultValue="7d">
          <option value="24h">24 hours</option>
          <option value="7d">7 days</option>
          <option value="30d">30 days</option>
        </select>
      </label>
      <label>
        How many (max 100) <input name="limit" inputMode="numeric" defaultValue="20" style={{ width: 80 }} />
      </label>
      <button type="submit" className="btn" disabled={pending}>
        {pending ? 'Adding…' : 'Add runs'}
      </button>
      <Status state={state} />
    </form>
  );
}

export function DatasetButtons({ datasetId, canDelete }: { datasetId: string; canDelete: boolean }) {
  const [state, action, pending] = useActionState(datasetAction, idle);
  return (
    <form action={action} style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
      <input type="hidden" name="dataset_id" value={datasetId} />
      <button type="submit" name="op" value="evaluate" className="btn btn-primary" disabled={pending}>
        {pending ? 'Running…' : 'Run checks'}
      </button>
      <a className="btn" href={`/console-api/datasets/${datasetId}/export`}>
        Export JSONL
      </a>
      {canDelete ? (
        <button type="submit" name="op" value="delete" className="btn" disabled={pending}>
          Delete dataset
        </button>
      ) : null}
      <Status state={state} />
    </form>
  );
}

export function RemoveItemButton({ datasetId, traceId }: { datasetId: string; traceId: string }) {
  const [state, action, pending] = useActionState(datasetAction, idle);
  return (
    <form action={action}>
      <input type="hidden" name="dataset_id" value={datasetId} />
      <input type="hidden" name="trace_id" value={traceId} />
      <button type="submit" name="op" value="remove" className="btn btn-sm" disabled={pending} aria-label="Remove from dataset">
        ✕
      </button>
      {state.kind === 'error' ? <Status state={state} /> : null}
    </form>
  );
}

export function AddToDataset({ traceId, datasets }: { traceId: string; datasets: { id: string; name: string }[] }) {
  const [state, action, pending] = useActionState(addRunAction, idle);
  if (datasets.length === 0) return null;
  return (
    <form action={action} style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
      <input type="hidden" name="trace_id" value={traceId} />
      <select name="dataset_id" aria-label="Dataset" defaultValue={datasets[0]!.id}>
        {datasets.map((d) => (
          <option key={d.id} value={d.id}>
            {d.name}
          </option>
        ))}
      </select>
      <button type="submit" className="btn btn-sm" disabled={pending}>
        Add to dataset
      </button>
      <Status state={state} />
    </form>
  );
}

export function AddTracesToDataset({ traceIds, datasets }: { traceIds: string[]; datasets: { id: string; name: string }[] }) {
  const [state, action, pending] = useActionState(addTracesAction, idle);
  if (datasets.length === 0) return null;
  return (
    <form action={action} style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
      <input type="hidden" name="trace_ids" value={JSON.stringify(traceIds)} />
      <select name="dataset_id" aria-label="Dataset" defaultValue={datasets[0]!.id}>
        {datasets.map((d) => (
          <option key={d.id} value={d.id}>
            {d.name}
          </option>
        ))}
      </select>
      <button type="submit" className="btn btn-sm" disabled={pending}>
        Add all to dataset
      </button>
      <Status state={state} />
    </form>
  );
}
