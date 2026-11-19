'use client';

import { useActionState, useState } from 'react';
import { formatCost } from '@/lib/format';
import { type LimitRow, type LimitsResponse, MAX_LIMITS, emptyRow, parseRows, rowState, toRow } from '@/lib/limits';
import { saveLimitsAction, type SaveState } from './actions';

const TONE = { ok: 'badge-ok', warning: 'badge-warn', critical: 'badge-failed' } as const;

export function LimitsForm({ data, agents, canEdit }: { data: LimitsResponse; agents: string[]; canEdit: boolean }) {
  const [state, action, pending] = useActionState<SaveState, FormData>(saveLimitsAction, { kind: 'idle' });
  const initial = data.limits.length ? data.limits.map(toRow) : [emptyRow('*')];
  const [rows, setRows] = useState<LimitRow[]>(initial);
  const set = (i: number, patch: Partial<LimitRow>) => setRows((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const spend = data.spend_today ?? {};
  const preview = parseRows(rows);

  return (
    <form action={action} key={data.version} className="stack">
      <input type="hidden" name="version" value={data.version} />
      <input type="hidden" name="rows" value={JSON.stringify(rows)} />
      <datalist id="known-agents">
        <option value="*">All agents</option>
        {agents.map((a) => (
          <option key={a} value={a} />
        ))}
      </datalist>
      <section className="panel panel-flush">
        <fieldset disabled={!canEdit || pending} style={{ border: 0, padding: 0, margin: 0 }}>
          <div style={{ overflowX: 'auto' }}>
            <table className="limits-table">
              <thead>
                <tr>
                  <th style={{ paddingLeft: 18 }}>Agent</th>
                  <th>Paused</th>
                  <th className="num">Max steps / run</th>
                  <th className="num">Max tokens / run</th>
                  <th className="num" title="Stop a run that calls the same tool with the same arguments more than this many times">
                    Max repeated calls
                  </th>
                  <th className="num">Daily budget (USD)</th>
                  <th>Pause over budget</th>
                  <th>Today</th>
                  <th aria-label="Remove" />
                </tr>
              </thead>
              <tbody>
                {rows.map((r, i) => {
                  const agent = r.agent.trim();
                  const st = rowState(
                    { agent, paused: r.paused, daily_budget_usd: Number(r.budget) || 0, pause_over_budget: r.pauseOverBudget },
                    data,
                  );
                  const spent = agent && agent !== '*' ? spend[agent] : undefined;
                  return (
                    <tr key={i}>
                      <td style={{ paddingLeft: 18 }}>
                        <input
                          aria-label={`Row ${i + 1} agent`}
                          list="known-agents"
                          value={r.agent}
                          onChange={(e) => set(i, { agent: e.target.value })}
                          placeholder="agent name or *"
                          style={{ width: 180 }}
                        />
                      </td>
                      <td>
                        <input type="checkbox" aria-label={`Row ${i + 1} paused`} checked={r.paused} onChange={(e) => set(i, { paused: e.target.checked })} />
                      </td>
                      <td className="num">
                        <input aria-label={`Row ${i + 1} max steps`} inputMode="numeric" value={r.maxSteps} onChange={(e) => set(i, { maxSteps: e.target.value })} placeholder="no limit" style={{ width: 100 }} />
                      </td>
                      <td className="num">
                        <input aria-label={`Row ${i + 1} max tokens`} inputMode="numeric" value={r.maxTokens} onChange={(e) => set(i, { maxTokens: e.target.value })} placeholder="no limit" style={{ width: 120 }} />
                      </td>
                      <td className="num">
                        <input
                          aria-label={`Row ${i + 1} max repeated calls`}
                          inputMode="numeric"
                          value={r.maxRepeats}
                          onChange={(e) => set(i, { maxRepeats: e.target.value })}
                          placeholder="no limit"
                          style={{ width: 100 }}
                        />
                      </td>
                      <td className="num">
                        <input aria-label={`Row ${i + 1} daily budget`} inputMode="decimal" value={r.budget} onChange={(e) => set(i, { budget: e.target.value })} placeholder="none" style={{ width: 100 }} />
                      </td>
                      <td>
                        <input
                          type="checkbox"
                          aria-label={`Row ${i + 1} pause over budget`}
                          checked={r.pauseOverBudget}
                          onChange={(e) => set(i, { pauseOverBudget: e.target.checked })}
                        />
                      </td>
                      <td>
                        <span className={`badge ${TONE[st.tone]}`}>{st.label}</span>
                        {spent !== undefined ? <div className="sub">{formatCost(spent, false)} spent today</div> : null}
                      </td>
                      <td>
                        <button type="button" className="btn btn-sm" onClick={() => setRows((rs) => rs.filter((_, j) => j !== i))} aria-label={`Remove row ${i + 1}`}>
                          ✕
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </fieldset>
        {canEdit ? (
          <div style={{ padding: '10px 18px 16px' }}>
            <button type="button" className="btn" onClick={() => setRows((rs) => [...rs, emptyRow()])} disabled={pending || rows.length >= MAX_LIMITS}>
              + Add a limit
            </button>
          </div>
        ) : null}
      </section>
      {canEdit ? (
        <div className="filters" style={{ marginBottom: 0 }}>
          <button type="submit" className="btn btn-primary" disabled={pending || Boolean(preview.error)}>
            {pending ? 'Saving…' : 'Save limits'}
          </button>
          {preview.error ? <span className="sub" role="status">{preview.error}</span> : null}
          {state.kind === 'saved' && !preview.error ? (
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
