import Link from 'next/link';
import { SearchIcon } from '@/components/Icons';
import { RunsTabs } from '@/components/RunsTabs';
import { ApiErrorPanel, KindChip, StatusBadge, settle } from '@/components/ui';
import { AddTracesToDataset } from '@/app/evals/Forms';
import { api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { formatRelative, formatTime } from '@/lib/format';
import { WINDOWS, asWindow, one } from '@/lib/ids';
import { segments } from '@/lib/search';

export const dynamic = 'force-dynamic';

export default async function SearchPage({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  await requireSession('/search');
  const sp = await searchParams;
  const q = (one(sp.q) ?? '').trim().slice(0, 200);
  const window = asWindow(one(sp.range) ?? '7d');
  const agent = one(sp.agent)?.trim() || undefined;
  const failed = one(sp.failed) === '1';
  const [res, datasets] = q
    ? await Promise.all([settle(api.search({ q, window, agent, status: failed ? 'failure' : undefined })), settle(api.datasets())])
    : [undefined, undefined];
  const now = Date.now();

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="title">
            <span className="title-icon" aria-hidden="true">
              <SearchIcon />
            </span>
            Search
          </h1>
          <div className="sub">Find runs by what was said, what a tool returned, or the error. Redacted values stay redacted.</div>
        </div>
        <RunsTabs current="/search" />
      </div>
      <form className="filters" method="get" action="/search" role="search">
        <input name="q" defaultValue={q} placeholder='e.g. chargeback, "order not found", timeout -retry' aria-label="Search" style={{ width: 380 }} maxLength={200} autoFocus />
        <select name="range" defaultValue={window} aria-label="Time range">
          {WINDOWS.map((w) => (
            <option key={w} value={w}>
              Last {w}
            </option>
          ))}
        </select>
        <input name="agent" placeholder="Agent" defaultValue={agent ?? ''} aria-label="Agent" maxLength={256} />
        <label className="inline" style={{ display: 'flex', gap: 6, alignItems: 'center', fontSize: 13 }}>
          <input type="checkbox" name="failed" value="1" defaultChecked={failed} /> Failed steps only
        </label>
        <button type="submit" className="btn btn-primary">
          Search
        </button>
      </form>
      {!q ? (
        <section className="panel">
          <div className="empty">
            Search prompts, answers, tool results and errors across runs. Use quotes for a phrase and <code>-word</code> to exclude.
          </div>
        </section>
      ) : !res!.ok ? (
        <ApiErrorPanel error={res!.error} />
      ) : res!.data.hits.length === 0 ? (
        <section className="panel">
          <div className="empty">No runs match “{q}” in the {window === '24h' ? 'last 24 hours' : `last ${window}`}.</div>
        </section>
      ) : (
        <section className="panel panel-flush">
          <div className="section-title">
            <h2>
              {res!.data.hits.length} run{res!.data.hits.length === 1 ? '' : 's'}
              {res!.data.hits.length === 50 ? ' (best 50)' : ''}
            </h2>
            {datasets?.ok ? (
              <AddTracesToDataset traceIds={res!.data.hits.map((h) => h.trace_id)} datasets={datasets.data.datasets.map((d) => ({ id: d.id, name: d.name }))} />
            ) : null}
          </div>
          <ul className="hits">
            {res!.data.hits.map((h) => (
              <li key={h.trace_id}>
                <div className="hit-head">
                  <KindChip type={h.step_type} />
                  <StatusBadge status={h.status} />
                  <Link href={`/traces/${h.trace_id}?step=${h.step_id}`}>
                    <b>{h.step_name}</b>
                  </Link>
                  <span className="sub">
                    {h.agent || 'Unnamed agent'}
                    {h.agent_version ? ` · ${h.agent_version}` : ''} · <span title={formatTime(h.start)}>{formatRelative(h.start, now)}</span>
                    {h.session_id ? (
                      <>
                        {' · '}
                        <Link href={`/sessions/${encodeURIComponent(h.session_id)}`}>conversation</Link>
                      </>
                    ) : null}
                  </span>
                </div>
                {h.snippet ? (
                  <div className="hit-snippet">
                    <span className="sub">{h.field}: </span>
                    {segments(h.snippet).map((s, i) => (s.match ? <mark key={i}>{s.text}</mark> : <span key={i}>{s.text}</span>))}
                  </div>
                ) : null}
              </li>
            ))}
          </ul>
        </section>
      )}
    </>
  );
}
