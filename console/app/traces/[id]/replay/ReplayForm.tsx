'use client';

import { useActionState, useState } from 'react';
import { formatCost, formatCount, formatDuration } from '@/lib/format';
import { type ReplayMessage, hasRedactions } from '@/lib/replay';
import { replayAction, type ReplayState } from './actions';

const ROLES: ReplayMessage['role'][] = ['system', 'user', 'assistant', 'tool'];

export function ReplayForm(props: { traceId: string; stepId: string; messages: ReplayMessage[]; provider: 'anthropic' | 'openai'; model: string; originalOutput: string }) {
  const [state, action, pending] = useActionState<ReplayState, FormData>(replayAction, { kind: 'idle' });
  const [msgs, setMsgs] = useState<ReplayMessage[]>(props.messages);
  const set = (i: number, patch: Partial<ReplayMessage>) => setMsgs((ms) => ms.map((m, j) => (j === i ? { ...m, ...patch } : m)));
  const redacted = hasRedactions(msgs);

  return (
    <form action={action} className="stack">
      <input type="hidden" name="trace_id" value={props.traceId} />
      <input type="hidden" name="step_id" value={props.stepId} />
      <input type="hidden" name="messages" value={JSON.stringify(msgs)} />
      <section className="panel stack">
        <h2>Model</h2>
        <div className="filters" style={{ marginBottom: 0 }}>
          <label>
            Provider{' '}
            <select name="provider" defaultValue={props.provider}>
              <option value="anthropic">Anthropic</option>
              <option value="openai">OpenAI</option>
            </select>
          </label>
          <label>
            Model <input name="model" defaultValue={props.model} required style={{ width: 260 }} spellCheck={false} />
          </label>
          <label>
            Max output tokens <input name="max_tokens" inputMode="numeric" placeholder="2048" style={{ width: 90 }} />
          </label>
        </div>
        <label style={{ display: 'grid', gap: 4, maxWidth: 520 }}>
          Your API key for this provider
          <input name="api_key" type="password" autoComplete="off" required spellCheck={false} placeholder="used for this replay only" />
        </label>
        <p className="sub" style={{ margin: 0 }}>
          The key is sent to the provider for this one request and never stored or logged. You pay the provider for the call. The replay
          isn&apos;t saved.
        </p>
      </section>

      <section className="panel stack">
        <h2>Conversation</h2>
        {redacted ? (
          <div className="notice" role="note">
            <span aria-hidden="true">🛡</span>
            <div>
              Redacted values stay redacted: Bindle never stored the originals. Replace <code>[REDACTED:…]</code> markers with test values if the
              answer depends on them.
            </div>
          </div>
        ) : null}
        {msgs.map((m, i) => (
          <div key={i} className="replay-turn">
            <div className="filters" style={{ marginBottom: 4 }}>
              <select aria-label={`Turn ${i + 1} role`} value={m.role} onChange={(e) => set(i, { role: e.target.value as ReplayMessage['role'] })}>
                {ROLES.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
              <button type="button" className="btn btn-sm" onClick={() => setMsgs((ms) => ms.filter((_, j) => j !== i))} aria-label={`Remove turn ${i + 1}`}>
                ✕
              </button>
            </div>
            <textarea
              aria-label={`Turn ${i + 1} text`}
              value={m.content}
              onChange={(e) => set(i, { content: e.target.value })}
              rows={Math.min(14, Math.max(3, m.content.split('\n').length + 1))}
              className="mono"
              spellCheck={false}
            />
          </div>
        ))}
        <div>
          <button type="button" className="btn" onClick={() => setMsgs((ms) => [...ms, { role: 'user', content: '' }])}>
            + Add a turn
          </button>
        </div>
        <p className="sub" style={{ margin: 0 }}>
          Tool calls and results are sent as text; tool definitions aren&apos;t replayed.
        </p>
      </section>

      <div className="filters" style={{ marginBottom: 0 }}>
        <button type="submit" className="btn btn-primary" disabled={pending}>
          {pending ? 'Running…' : 'Run replay'}
        </button>
      </div>
      {state.kind === 'error' ? (
        <div className="notice notice-error" role="alert">
          <span aria-hidden="true">✕</span>
          <div>{state.message}</div>
        </div>
      ) : null}
      {state.kind === 'done' ? <Comparison res={state.res} /> : null}
    </form>
  );
}

function Comparison({ res }: { res: import('@/lib/replay').ReplayResponse }) {
  const o = res.original;
  const r = res.result;
  return (
    <div className="grid-2" aria-live="polite">
      <section className="panel stack">
        <h2>Original</h2>
        <div className="ctx-stats">
          <span>
            Model <b>{o.model || 'unknown'}</b>
          </span>
          <span>
            Tokens <b>{formatCount(o.input_tokens)}</b> in · <b>{formatCount(o.output_tokens)}</b> out
          </span>
          <span>
            Cost <b>{o.cost_known ? formatCost(o.cost_usd, false) : 'unknown'}</b>
          </span>
          <span>
            Took <b>{formatDuration(o.duration_ms)}</b>
          </span>
        </div>
        <pre className="payload">{o.output || 'No output was captured.'}</pre>
      </section>
      <section className="panel stack">
        <h2>Replay</h2>
        <div className="ctx-stats">
          <span>
            Model <b>{r.model}</b>
          </span>
          <span>
            Tokens <b>{formatCount(r.input_tokens)}</b> in · <b>{formatCount(r.output_tokens)}</b> out
          </span>
          <span>
            Cost <b>{res.cost_known ? formatCost(res.cost_usd, false) : 'unknown'}</b>
          </span>
          <span>
            Took <b>{formatDuration(r.latency_ms)}</b>
          </span>
          {r.stop_reason ? (
            <span>
              Stopped <b>{r.stop_reason}</b>
            </span>
          ) : null}
        </div>
        <pre className="payload">{r.output || '(empty answer)'}</pre>
      </section>
    </div>
  );
}
