'use client';

import { useActionState, useState } from 'react';
import { CHANNEL_KINDS, FEATURE_KINDS, HAS_MIN, RULE_KINDS, WINDOWS, thresholdInput, type AlertRule, type ChannelKind, type RuleKind } from '@/lib/alerting';
import { channelAction, createChannelAction, createRuleAction, updateRuleAction, type AlertActionState } from './actions';

const idle: AlertActionState = { kind: 'idle' };

function Status({ state }: { state: AlertActionState }) {
  const [copied, setCopied] = useState(false);
  if (state.kind === 'error')
    return (
      <div className="notice notice-error" role="alert">
        <span aria-hidden="true">✕</span>
        <div>{state.message}</div>
      </div>
    );
  if (state.kind !== 'done') return null;
  if (!state.secret)
    return (
      <span className="sub" role="status">
        ✓ {state.message}
      </span>
    );
  const secret = state.secret;
  return (
    <div className="notice notice-warn" role="status">
      <span aria-hidden="true">⚠</span>
      <div style={{ minWidth: 0, flex: 1 }}>
        <strong>{state.message} Copy the signing secret now. It won&apos;t be shown again.</strong>
        <div style={{ display: 'flex', gap: 8, marginTop: 8, alignItems: 'center' }}>
          <code className="mono" style={{ overflowWrap: 'anywhere', flex: 1 }}>
            {secret}
          </code>
          <button
            type="button"
            onClick={async () => {
              await navigator.clipboard.writeText(secret);
              setCopied(true);
            }}
          >
            {copied ? 'Copied' : 'Copy'}
          </button>
        </div>
        <div className="sub" style={{ marginTop: 6 }}>
          Verify each request: HMAC-SHA256 of <code>X-Bindle-Timestamp + &quot;.&quot; + body</code> with this secret must equal{' '}
          <code>X-Bindle-Signature</code> (after <code>sha256=</code>). Reject timestamps older than 5 minutes.
        </div>
      </div>
    </div>
  );
}

function WindowSelect({ value }: { value: number }) {
  return (
    <select name="window" defaultValue={String(value)} aria-label="Window">
      {WINDOWS.map((w) => (
        <option key={w.minutes} value={w.minutes}>
          {w.label}
        </option>
      ))}
    </select>
  );
}

export function NewRuleForm({ features = [] }: { features?: string[] }) {
  const kinds = RULE_KINDS.filter((k) => !FEATURE_KINDS[k.kind] || features.includes(FEATURE_KINDS[k.kind]!));
  const [state, action, pending] = useActionState(createRuleAction, idle);
  const [kind, setKind] = useState<RuleKind>('run_cost');
  const meta = RULE_KINDS.find((k) => k.kind === kind)!;
  return (
    <div className="stack">
      <form action={action} className="filters" style={{ marginBottom: 0 }}>
        <select name="kind" value={kind} onChange={(e) => setKind(e.target.value as RuleKind)} aria-label="Alert on">
          {kinds.map((k) => (
            <option key={k.kind} value={k.kind}>
              {k.label}
            </option>
          ))}
        </select>
        {kind === 'weekly_digest' ? (
          <>
            <input type="hidden" name="threshold" value="1" />
            <input type="hidden" name="window" value="1440" />
          </>
        ) : (
          <>
            <input name="threshold" inputMode="decimal" required placeholder={meta.unit} aria-label={`Threshold (${meta.unit})`} style={{ width: 120 }} />
            <WindowSelect value={kind === 'cost_regression' ? 1440 : 60} />
          </>
        )}
        {HAS_MIN.includes(kind) ? <input name="min_runs" inputMode="numeric" placeholder="minimum" aria-label="Minimum count" style={{ width: 100 }} /> : null}
        <button type="submit" className="btn btn-primary" disabled={pending}>
          {pending ? 'Adding…' : 'Add rule'}
        </button>
      </form>
      <div className="sub">{meta.help}</div>
      <Status state={state} />
    </div>
  );
}

export function RuleRow({ rule, canEdit }: { rule: AlertRule; canEdit: boolean }) {
  const [state, action, pending] = useActionState(updateRuleAction, idle);
  const meta = RULE_KINDS.find((k) => k.kind === rule.kind);
  return (
    <form action={action} className="filters" style={{ marginBottom: 0, alignItems: 'center' }}>
      <input type="hidden" name="id" value={rule.id} />
      <input type="hidden" name="kind" value={rule.kind} />
      <strong style={{ minWidth: 120 }}>{meta?.label ?? rule.kind}</strong>
      {rule.kind === 'weekly_digest' ? (
        <>
          <input type="hidden" name="threshold" value="1" />
          <input type="hidden" name="window" value="1440" />
          <span className="sub">Mondays at 09:00 UTC</span>
        </>
      ) : (
        <>
          <label className="sub" style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
            at
            <input
              name="threshold"
              defaultValue={thresholdInput(rule.kind, rule.threshold)}
              inputMode="decimal"
              disabled={!canEdit}
              aria-label={`Threshold (${meta?.unit ?? ''})`}
              style={{ width: 90 }}
            />
            {meta?.unit}
          </label>
          <WindowSelect value={rule.window_minutes} />
        </>
      )}
      {HAS_MIN.includes(rule.kind) ? (
        <input name="min_runs" defaultValue={String(rule.min_runs)} inputMode="numeric" disabled={!canEdit} aria-label="Minimum runs" style={{ width: 80 }} />
      ) : null}
      <label style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
        <input type="checkbox" name="enabled" defaultChecked={rule.enabled} disabled={!canEdit} /> On
      </label>
      {canEdit ? (
        <button type="submit" disabled={pending}>
          {pending ? 'Saving…' : 'Save'}
        </button>
      ) : null}
      <Status state={state} />
    </form>
  );
}

export function NewChannelForm() {
  const [state, action, pending] = useActionState(createChannelAction, idle);
  const [kind, setKind] = useState<ChannelKind>('slack');
  const meta = CHANNEL_KINDS.find((k) => k.kind === kind)!;
  return (
    <div className="stack">
      <form action={action} className="filters" style={{ marginBottom: 0 }}>
        <select name="kind" value={kind} onChange={(e) => setKind(e.target.value as ChannelKind)} aria-label="Channel type">
          {CHANNEL_KINDS.map((k) => (
            <option key={k.kind} value={k.kind}>
              {k.label}
            </option>
          ))}
        </select>
        <input name="name" placeholder="Name, e.g. #agent-oncall" maxLength={100} aria-label="Channel name" />
        <input
          name="target"
          required
          type={kind === 'email' ? 'email' : 'url'}
          placeholder={meta.placeholder}
          aria-label={kind === 'email' ? 'Email address' : 'URL'}
          style={{ minWidth: 320 }}
        />
        <button type="submit" className="btn btn-primary" disabled={pending}>
          {pending ? 'Adding…' : 'Add channel'}
        </button>
      </form>
      <Status state={state} />
    </div>
  );
}

export function ChannelActions({ id, name, enabled }: { id: string; name: string; enabled: boolean }) {
  const [state, action, pending] = useActionState(channelAction, idle);
  return (
    <div>
      <form action={action} style={{ display: 'inline-flex', gap: 6, flexWrap: 'wrap' }}>
        <input type="hidden" name="id" value={id} />
        <button type="submit" name="op" value="test" disabled={pending}>
          Send test
        </button>
        <button type="submit" name="op" value={enabled ? 'pause' : 'resume'} disabled={pending}>
          {enabled ? 'Pause' : 'Resume'}
        </button>
        <button
          type="submit"
          name="op"
          value="delete"
          disabled={pending}
          onClick={(e) => {
            if (!confirm(`Delete “${name}”? Alerts stop going there.`)) e.preventDefault();
          }}
        >
          Delete
        </button>
      </form>
      <Status state={state} />
    </div>
  );
}
