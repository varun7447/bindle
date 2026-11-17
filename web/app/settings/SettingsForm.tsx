'use client';

import { useActionState } from 'react';
import { formatPatterns, type SDKConfig } from '@/lib/sdksettings';
import { saveSettingsAction, type SaveState } from './actions';

export function SettingsForm({ config, canEdit }: { config: SDKConfig; canEdit: boolean }) {
  const [state, action, pending] = useActionState<SaveState, FormData>(saveSettingsAction, { kind: 'idle' });
  // After a save, the page re-renders with the new version; keep the key in
  // sync so the form resets to the stored values.
  return (
    <form action={action} key={config.version} className="stack" style={{ maxWidth: 720 }}>
      <input type="hidden" name="version" value={config.version} />
      <fieldset disabled={!canEdit || pending} style={{ border: 0, padding: 0, margin: 0 }} className="stack">
        <section className="panel">
          <h2>Sampling</h2>
          <label style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            Keep
            <input
              name="sample_percent"
              type="number"
              min={0}
              max={100}
              step="any"
              defaultValue={Math.round(config.sample_rate * 100000) / 1000}
              style={{ width: 100 }}
              aria-describedby="sample-help"
            />
            % of traces
          </label>
          <p id="sample-help" className="sub" style={{ marginBottom: 0 }}>
            Whole traces are kept or dropped together, using the same rule in the SDK and in Bindle&apos;s ingest.
          </p>
        </section>
        <section className="panel">
          <h2>Content capture</h2>
          <label style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <input name="capture_content" type="checkbox" defaultChecked={config.capture_content} />
            Send prompts, outputs and reasoning
          </label>
          <p className="sub" style={{ marginBottom: 0 }}>
            When off, steps keep their timing, status, tokens and errors, and their content is removed in the SDK and again by
            Bindle before storage.
          </p>
        </section>
        <section className="panel">
          <h2>Extra redaction patterns</h2>
          <textarea
            name="patterns"
            rows={6}
            defaultValue={formatPatterns(config.redact_patterns)}
            placeholder={'ORDER_ID=\\bord_[0-9a-f]{8}\\b\nACCOUNT=acct-\\d+'}
            spellCheck={false}
            className="mono"
            style={{ width: '100%', background: 'var(--bg)', color: 'var(--text)', border: '1px solid var(--border)', borderRadius: 6, padding: 10 }}
            aria-describedby="patterns-help"
          />
          <p id="patterns-help" className="sub" style={{ marginBottom: 0 }}>
            One <code>NAME=regex</code> per line, up to 20. Matches become <code>[REDACTED:NAME]</code>. These add to the
            built-in rules (emails, phone numbers, cards, secrets), which can&apos;t be turned off. Nested repetition such as{' '}
            <code>(a+)+</code> isn&apos;t allowed, so a pattern can&apos;t slow your agent down.
          </p>
        </section>
      </fieldset>
      {canEdit ? (
        <div className="filters" style={{ marginBottom: 0 }}>
          <button type="submit" className="btn btn-primary" disabled={pending}>
            {pending ? 'Saving…' : 'Save settings'}
          </button>
          {state.kind === 'saved' ? (
            <span className="sub" role="status">
              <span aria-hidden="true" style={{ color: 'var(--good)' }}>✓</span> Saved (version {state.version}). Agents pick it up within two minutes.
            </span>
          ) : null}
        </div>
      ) : null}
      {state.kind === 'error' ? (
        <div className="notice notice-error" role="alert">
          <span aria-hidden="true">✕</span>
          <div>{state.message}</div>
        </div>
      ) : null}
    </form>
  );
}
