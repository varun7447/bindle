import Link from 'next/link';
import { ApiErrorPanel, RunsTable, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { WINDOWS, WINDOW_MS, asWindow, one } from '@/lib/ids';
import { requireSession } from '@/lib/auth';

type SP = Record<string, string | string[] | undefined>;

const STATUSES = [
  { value: '', label: 'Any status' },
  { value: 'failed', label: 'Failed' },
  { value: 'ok', label: 'OK' },
];

function hrefWith(base: Record<string, string | undefined>, extra: Record<string, string | undefined>): string {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries({ ...base, ...extra })) if (v) p.set(k, v);
  const s = p.toString();
  return s ? `/runs?${s}` : '/runs';
}

export default async function RunsPage({ searchParams }: { searchParams: Promise<SP> }) {
  await requireSession('/runs');
  const sp = await searchParams;
  const range = asWindow(one(sp.range));
  const status = ['failed', 'ok'].includes(one(sp.status) ?? '') ? one(sp.status) : undefined;
  const agent = one(sp.agent)?.trim().slice(0, 256) || undefined;
  const environment = one(sp.environment)?.trim().slice(0, 256) || undefined;
  const cursor = one(sp.cursor) || undefined;
  // "to" is pinned for the whole pagination session so pages don't shift as new runs arrive.
  const toParam = one(sp.to);
  const to = toParam && !Number.isNaN(Date.parse(toParam)) ? new Date(toParam) : new Date();
  const from = new Date(to.getTime() - WINDOW_MS[range]);
  const filters = { range: range === '24h' ? undefined : range, status, agent, environment };

  const res = await settle(
    api.runs({ from: from.toISOString(), to: to.toISOString(), status, agent, environment, cursor, limit: '50' }),
  );

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Runs</h1>
          <div className="sub">Each run is one trace. Newest first · times in UTC</div>
        </div>
      </div>
      <form className="filters" method="get" action="/runs" aria-label="Filter runs">
        <select name="range" defaultValue={range} aria-label="Time range">
          {WINDOWS.map((w) => (
            <option key={w} value={w}>
              Last {w}
            </option>
          ))}
        </select>
        <select name="status" defaultValue={status ?? ''} aria-label="Status">
          {STATUSES.map((s) => (
            <option key={s.value} value={s.value}>
              {s.label}
            </option>
          ))}
        </select>
        <input name="agent" placeholder="Agent" defaultValue={agent ?? ''} aria-label="Agent" maxLength={256} />
        <input name="environment" placeholder="Environment" defaultValue={environment ?? ''} aria-label="Environment" maxLength={256} />
        <button type="submit">Apply</button>
        {status || agent || environment || range !== '24h' ? <Link href="/runs">Clear</Link> : null}
      </form>

      <section className="panel">
        {!res.ok ? (
          <ApiErrorPanel error={res.error} />
        ) : (
          <>
            <RunsTable runs={res.data.runs} now={Date.now()} />
            <div className="filters" style={{ marginTop: 12, marginBottom: 0, justifyContent: 'flex-end' }}>
              {cursor ? (
                <Link className="btn" href={hrefWith(filters, {})}>
                  ← Newest
                </Link>
              ) : null}
              {res.data.next_cursor ? (
                <Link className="btn" href={hrefWith(filters, { cursor: res.data.next_cursor, to: to.toISOString() })}>
                  Older →
                </Link>
              ) : null}
            </div>
          </>
        )}
      </section>
    </>
  );
}
