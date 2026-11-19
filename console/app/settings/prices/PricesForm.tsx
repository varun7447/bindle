'use client';

import { useActionState, useState } from 'react';
import { formatCount } from '@/lib/format';
import { type AWSPrice, type ModelUsage, type PriceRow, type PricesResponse, MAX_PRICES, covers, emptyRow, fromAWS, parseRows, toRow } from '@/lib/prices';
import { savePricesAction, type SaveState } from './actions';

export function PricesForm({ data, models, canEdit, aws }: { data: PricesResponse; models: ModelUsage[]; canEdit: boolean; aws?: { region: string; prices: AWSPrice[] } | { region: string; error: string } }) {
  const [state, action, pending] = useActionState<SaveState, FormData>(savePricesAction, { kind: 'idle' });
  const [rows, setRows] = useState<PriceRow[]>(data.prices.length ? data.prices.map(toRow) : [emptyRow()]);
  const set = (i: number, patch: Partial<PriceRow>) => setRows((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const preview = parseRows(rows);
  const add = (provider = '', model = '') =>
    setRows((rs) => {
      const blank = rs.findIndex((r) => !r.model.trim() && !r.input.trim() && !r.output.trim());
      if (blank >= 0) return rs.map((r, j) => (j === blank ? emptyRow(provider, model) : r));
      return [...rs, emptyRow(provider, model)];
    });
  const uncovered = models.filter((m) => !preview.prices.some((p) => covers(p, m)));
  const [q, setQ] = useState('');
  const useAWS = (p: AWSPrice) =>
    setRows((rs) => {
      const row = fromAWS(p);
      const blank = rs.findIndex((r) => !r.model.trim() && !r.input.trim() && !r.output.trim());
      return blank >= 0 ? rs.map((r, j) => (j === blank ? row : r)) : [...rs, row];
    });
  const awsRows = aws && 'prices' in aws ? aws.prices.filter((p) => `${p.provider} ${p.model} ${p.tier}`.toLowerCase().includes(q.trim().toLowerCase())) : [];

  return (
    <form action={action} key={data.version} className="stack">
      <input type="hidden" name="version" value={data.version} />
      <input type="hidden" name="rows" value={JSON.stringify(rows)} />
      <section className="panel panel-flush">
        <fieldset disabled={!canEdit || pending} style={{ border: 0, padding: 0, margin: 0 }}>
          <div style={{ overflowX: 'auto' }}>
            <table className="limits-table">
              <thead>
                <tr>
                  <th style={{ paddingLeft: 18 }}>Provider</th>
                  <th>Model</th>
                  <th className="num">Input</th>
                  <th className="num">Output</th>
                  <th className="num">Cached input</th>
                  <th>Note</th>
                  <th aria-label="Remove" />
                </tr>
              </thead>
              <tbody>
                {rows.map((r, i) => (
                  <tr key={i}>
                    <td style={{ paddingLeft: 18 }}>
                      <input aria-label={`Row ${i + 1} provider`} value={r.provider} onChange={(e) => set(i, { provider: e.target.value })} placeholder="any" style={{ width: 110 }} />
                    </td>
                    <td>
                      <input aria-label={`Row ${i + 1} model`} value={r.model} onChange={(e) => set(i, { model: e.target.value })} placeholder="model ID" style={{ width: 230 }} />
                    </td>
                    <td className="num">
                      <input aria-label={`Row ${i + 1} input price`} inputMode="decimal" value={r.input} onChange={(e) => set(i, { input: e.target.value })} style={{ width: 80 }} />
                    </td>
                    <td className="num">
                      <input aria-label={`Row ${i + 1} output price`} inputMode="decimal" value={r.output} onChange={(e) => set(i, { output: e.target.value })} style={{ width: 80 }} />
                    </td>
                    <td className="num">
                      <input aria-label={`Row ${i + 1} cached input price`} inputMode="decimal" value={r.cached} onChange={(e) => set(i, { cached: e.target.value })} placeholder="unknown" style={{ width: 90 }} />
                    </td>
                    <td>
                      <input aria-label={`Row ${i + 1} note`} value={r.note} onChange={(e) => set(i, { note: e.target.value })} placeholder="e.g. 2026 contract" style={{ width: 170 }} />
                    </td>
                    <td>
                      <button type="button" className="btn btn-sm" onClick={() => setRows((rs) => rs.filter((_, j) => j !== i))} aria-label={`Remove row ${i + 1}`}>
                        ✕
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="sub" style={{ padding: '8px 18px 0' }}>USD per million tokens.</div>
        </fieldset>
        {canEdit ? (
          <div style={{ padding: '10px 18px 16px' }}>
            <button type="button" className="btn" onClick={() => add()} disabled={pending || rows.length >= MAX_PRICES}>
              + Add a model price
            </button>
          </div>
        ) : null}
      </section>
      {canEdit ? (
        <div className="filters" style={{ marginBottom: 0 }}>
          <button type="submit" className="btn btn-primary" disabled={pending || Boolean(preview.error)}>
            {pending ? 'Saving…' : 'Save prices'}
          </button>
          {preview.error ? <span className="sub" role="status">{preview.error}</span> : null}
          {state.kind === 'saved' && !preview.error ? (
            <span className="sub" role="status">
              <span aria-hidden="true" style={{ color: 'var(--good)' }}>✓</span> Saved (version {state.version}). New runs use these prices within a minute.
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

      <section className="panel panel-flush">
        <div className="section-title">
          <h2>Amazon Bedrock list prices</h2>
          <span className="sub">From AWS&apos;s public price list; pick the row for how you call the model</span>
        </div>
        <div style={{ padding: '10px 18px 0' }} className="eval-form">
          <label>
            Region{' '}
            <input form="aws-region" name="aws_region" defaultValue={aws?.region ?? ''} placeholder="us-east-1" style={{ width: 130 }} spellCheck={false} />
          </label>
          <button type="submit" form="aws-region" className="btn">
            Show prices
          </button>
          {aws && 'prices' in aws ? (
            <label>
              Filter <input value={q} onChange={(e) => setQ(e.target.value)} placeholder="claude, nova, llama…" style={{ width: 200 }} />
            </label>
          ) : null}
        </div>
        {aws && 'error' in aws ? (
          <div className="sub" role="alert" style={{ padding: '10px 18px', color: 'var(--critical-text)' }}>
            {aws.error}
          </div>
        ) : aws && 'prices' in aws ? (
          <div style={{ maxHeight: 360, overflowY: 'auto', marginTop: 8 }}>
            <table>
              <thead>
                <tr>
                  <th style={{ paddingLeft: 18 }}>Model</th>
                  <th>Tier</th>
                  <th className="num">Input</th>
                  <th className="num">Output</th>
                  <th className="num">Cache read</th>
                  <th aria-label="Use" />
                </tr>
              </thead>
              <tbody>
                {awsRows.map((p, i) => (
                  <tr key={`${p.provider}/${p.model}/${p.tier}/${i}`}>
                    <td style={{ paddingLeft: 18 }}>
                      {p.model}
                      <span className="sub"> · {p.provider}</span>
                    </td>
                    <td>{p.tier || 'standard'}</td>
                    <td className="num">{p.input_per_mtok ?? '–'}</td>
                    <td className="num">{p.output_per_mtok ?? '–'}</td>
                    <td className="num">{p.cache_read_per_mtok ?? '–'}</td>
                    <td style={{ paddingRight: 18, textAlign: 'right' }}>
                      {canEdit && p.input_per_mtok !== null && p.output_per_mtok !== null ? (
                        <button type="button" className="btn btn-sm" onClick={() => useAWS(p)}>
                          Use
                        </button>
                      ) : null}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
        <p className="sub" style={{ padding: '8px 18px 14px', margin: 0 }}>
          “Global” is global cross-region inference; “standard” is in-region. <b>Use</b> copies the prices into a row above; enter the Bedrock model ID as it
          appears in your runs (see “Models used” below) and save. USD per million tokens.
        </p>
      </section>

      <section className="panel panel-flush">
        <div className="section-title">
          <h2>Models used in the last 7 days</h2>
          <span className="sub">Without a workspace price, list prices apply</span>
        </div>
        {models.length === 0 ? (
          <div className="empty">No LLM calls yet.</div>
        ) : (
          <table style={{ marginTop: 8 }}>
            <thead>
              <tr>
                <th style={{ paddingLeft: 18 }}>Model</th>
                <th className="num">Calls</th>
                <th className="num">Unpriced</th>
                <th>Price</th>
                <th aria-label="Action" />
              </tr>
            </thead>
            <tbody>
              {models.map((m) => {
                const own = !uncovered.includes(m);
                return (
                  <tr key={`${m.provider}/${m.model}`}>
                    <td className="truncate" style={{ paddingLeft: 18 }}>
                      <code>{m.model}</code>
                      {m.provider ? <span className="sub"> · {m.provider}</span> : null}
                    </td>
                    <td className="num">{formatCount(m.calls)}</td>
                    <td className="num">{m.unpriced_calls ? <span className="badge badge-warn">{formatCount(m.unpriced_calls)}</span> : '0'}</td>
                    <td>{own ? <span className="badge badge-ok">Workspace</span> : m.unpriced_calls === m.calls ? <span className="badge">Unknown</span> : <span className="badge">List</span>}</td>
                    <td style={{ paddingRight: 18, textAlign: 'right' }}>
                      {canEdit && !own ? (
                        <button type="button" className="btn btn-sm" onClick={() => add(m.provider, m.model)}>
                          Set price
                        </button>
                      ) : null}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </section>
    </form>
  );
}
