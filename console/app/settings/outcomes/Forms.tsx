'use client';

import { useActionState, useState } from 'react';
import { createEndpointAction, revokeEndpointAction, saveOutcomeSettingsAction, type OutcomeActionState } from './actions';

const idle: OutcomeActionState = { kind: 'idle' };

function Status({ state }: { state: OutcomeActionState }) {
  if (state.kind === 'error')
    return (
      <div className="notice notice-error" role="alert">
        <span aria-hidden="true">✕</span>
        <div>{state.message}</div>
      </div>
    );
  if (state.kind === 'saved')
    return (
      <div className="notice" role="status">
        <span aria-hidden="true">✓</span>
        <div>Saved. Grading on the Agents page uses the new reversal list right away.</div>
      </div>
    );
  return null;
}

export function SettingsForm({ reversals, pendingHours, canManage }: { reversals: string[]; pendingHours: number; canManage: boolean }) {
  const [state, action, pending] = useActionState(saveOutcomeSettingsAction, idle);
  return (
    <form action={action} className="stack">
      <label className="stack" style={{ gap: 4 }}>
        <span>
          <b>Reversal outcomes</b>, one per line. A run with one of these counts as wrong, even if every step and eval passed.
        </span>
        <textarea name="reversals" rows={4} defaultValue={reversals.join('\n')} placeholder={'ticket_reopened\nrefund_reversed\nescalated_to_human'} disabled={!canManage} />
      </label>
      <label style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
        <span>Keep retrying an event that matches no run for</span>
        <input name="pending_hours" type="number" min={1} max={168} defaultValue={pendingHours} style={{ width: 80 }} disabled={!canManage} />
        <span>hours (up to 7 days).</span>
      </label>
      {canManage ? (
        <div>
          <button type="submit" className="btn btn-primary" disabled={pending}>
            {pending ? 'Saving…' : 'Save'}
          </button>
        </div>
      ) : null}
      <Status state={state} />
    </form>
  );
}

export function CreateEndpointForm({ ingestUrl }: { ingestUrl: string }) {
  const [state, action, pending] = useActionState(createEndpointAction, idle);
  const [copied, setCopied] = useState(false);
  return (
    <div className="stack">
      <form action={action} className="filters" style={{ marginBottom: 0 }}>
        <input name="name" placeholder="Endpoint name, e.g. Helpdesk" maxLength={80} aria-label="Endpoint name" style={{ minWidth: 260 }} />
        <button type="submit" className="btn btn-primary" disabled={pending}>
          {pending ? 'Creating…' : 'Create webhook endpoint'}
        </button>
      </form>
      {state.kind === 'created' ? (
        <div className="notice notice-warn" role="status">
          <span aria-hidden="true">⚠</span>
          <div style={{ minWidth: 0, flex: 1 }}>
            <strong>Copy the signing secret for “{state.name}” now. It won&apos;t be shown again.</strong>
            <div style={{ display: 'flex', gap: 8, marginTop: 8, alignItems: 'center' }}>
              <code className="mono" style={{ overflowWrap: 'anywhere', flex: 1 }}>
                {state.secret}
              </code>
              <button
                type="button"
                onClick={async () => {
                  await navigator.clipboard.writeText(state.secret);
                  setCopied(true);
                }}
              >
                {copied ? 'Copied' : 'Copy'}
              </button>
            </div>
            <div className="sub" style={{ marginTop: 6 }}>
              POST events to <code>{ingestUrl + state.path}</code> with <code>{state.header}: t=&lt;unix seconds&gt;,v1=&lt;hex HMAC-SHA256(secret, t + &quot;.&quot; + body)&gt;</code>.
              Requests more than 5 minutes old are rejected.
            </div>
          </div>
        </div>
      ) : (
        <Status state={state} />
      )}
    </div>
  );
}

export function RevokeEndpointForm({ id }: { id: string }) {
  const [state, action, pending] = useActionState(revokeEndpointAction, idle);
  return (
    <form action={action} style={{ display: 'inline' }}>
      <input type="hidden" name="id" value={id} />
      <button type="submit" disabled={pending}>
        {pending ? 'Revoking…' : 'Revoke'}
      </button>
      <Status state={state} />
    </form>
  );
}
