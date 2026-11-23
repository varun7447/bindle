'use client';

import { useActionState, useState } from 'react';
import { GRACE_CHOICES } from '@/lib/keys';
import {
  createKeyAction,
  createMgmtKeyAction,
  revokeKeyAction,
  revokeMgmtKeyAction,
  rotateKeyAction,
  type KeyActionState,
} from './actions';
import { SCOPE_LABEL } from '@/lib/passports';

const idle: KeyActionState = { kind: 'idle' };

/** Shows a new secret exactly once, with a copy button. */
function SecretOnce({ state }: { state: KeyActionState }) {
  const [copied, setCopied] = useState(false);
  if (state.kind === 'error')
    return (
      <div className="notice notice-error" role="alert">
        <span aria-hidden="true">✕</span>
        <div>{state.message}</div>
      </div>
    );
  if (state.kind !== 'created') return null;
  return (
    <div className="notice notice-warn" role="status">
      <span aria-hidden="true">⚠</span>
      <div style={{ minWidth: 0, flex: 1 }}>
        <strong>Copy your new key for “{state.name}” now. It won&apos;t be shown again.</strong>
        {state.note ? <div>{state.note}</div> : null}
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
          {state.mgmt ? (
            <>
              Set it as <code>BINDLE_MGMT_KEY</code> in CI or your shell for the <code>bindle</code> CLI. Never give it to an agent.
            </>
          ) : (
            <>
              Set it as <code>BINDLE_API_KEY</code> in your agent&apos;s environment.
            </>
          )}
        </div>
      </div>
    </div>
  );
}

export function CreateKeyForm() {
  const [state, action, pending] = useActionState(createKeyAction, idle);
  return (
    <div className="stack">
      <form action={action} className="filters" style={{ marginBottom: 0 }}>
        <input name="name" placeholder="Key name, e.g. production agents" maxLength={100} aria-label="Key name" style={{ minWidth: 280 }} />
        <button type="submit" className="btn-primary btn" disabled={pending}>
          {pending ? 'Creating…' : 'Create key'}
        </button>
      </form>
      <SecretOnce state={state} />
    </div>
  );
}

export function RotateKeyForm({ id }: { id: string }) {
  const [state, action, pending] = useActionState(rotateKeyAction, idle);
  return (
    <div>
      <form action={action} style={{ display: 'inline-flex', gap: 6 }}>
        <input type="hidden" name="id" value={id} />
        <select name="grace" defaultValue="24" aria-label="When the old key stops working">
          {GRACE_CHOICES.map((g) => (
            <option key={g.hours} value={g.hours}>
              Old key stops: {g.label.toLowerCase()}
            </option>
          ))}
        </select>
        <button type="submit" disabled={pending}>
          {pending ? 'Rotating…' : 'Rotate'}
        </button>
      </form>
      <SecretOnce state={state} />
    </div>
  );
}

/** Creates a management key for the CLI or CI, with the scopes chosen. */
export function CreateMgmtKeyForm({ scopes }: { scopes: string[] }) {
  const [state, action, pending] = useActionState(createMgmtKeyAction, idle);
  return (
    <div className="stack">
      <form action={action} className="filters" style={{ marginBottom: 0, flexWrap: 'wrap' }}>
        <input name="name" placeholder="Key name, e.g. passports CI" maxLength={100} aria-label="Management key name" style={{ minWidth: 240 }} />
        <fieldset aria-label="What the key may do" style={{ display: 'inline-flex', gap: 12, border: 0, padding: 0, margin: 0 }}>
          {scopes.map((s) => (
            <label key={s} style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
              <input type="checkbox" name="scope" value={s} defaultChecked={s === 'passport:write'} />
              {SCOPE_LABEL[s] ?? s}
            </label>
          ))}
        </fieldset>
        <button type="submit" className="btn-primary btn" disabled={pending}>
          {pending ? 'Creating…' : 'Create management key'}
        </button>
      </form>
      <SecretOnce state={state} />
    </div>
  );
}

export function RevokeMgmtKeyForm({ id, name }: { id: string; name: string }) {
  const [state, action, pending] = useActionState(revokeMgmtKeyAction, idle);
  return (
    <div>
      <form
        action={action}
        onSubmit={(e) => {
          if (!confirm(`Revoke “${name}”? The CLI and CI jobs using it stop working within a minute.`)) e.preventDefault();
        }}
      >
        <input type="hidden" name="id" value={id} />
        <button type="submit" disabled={pending} style={{ color: 'var(--critical)' }}>
          {pending ? 'Revoking…' : 'Revoke'}
        </button>
      </form>
      {state.kind === 'error' ? <SecretOnce state={state} /> : null}
    </div>
  );
}

export function RevokeKeyForm({ id, name }: { id: string; name: string }) {
  const [state, action, pending] = useActionState(revokeKeyAction, idle);
  return (
    <div>
      <form
        action={action}
        onSubmit={(e) => {
          if (!confirm(`Revoke “${name}”? Agents using it stop sending traces within a minute.`)) e.preventDefault();
        }}
      >
        <input type="hidden" name="id" value={id} />
        <button type="submit" disabled={pending} style={{ color: 'var(--critical)' }}>
          {pending ? 'Revoking…' : 'Revoke'}
        </button>
      </form>
      {state.kind === 'error' ? <SecretOnce state={state} /> : null}
    </div>
  );
}
