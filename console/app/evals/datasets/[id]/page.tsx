import Link from 'next/link';
import { notFound } from 'next/navigation';
import { LayersIcon } from '@/components/Icons';
import { ApiErrorPanel, settle } from '@/components/ui';
import { ApiError, api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { overall, passRate } from '@/lib/evals';
import { formatCost, formatCount, formatDuration, formatPercent, formatTime } from '@/lib/format';
import { AddRunsForm, DatasetButtons, RemoveItemButton } from '../../Forms';

export const dynamic = 'force-dynamic';

export default async function DatasetPage({ params }: { params: Promise<{ id: string }> }) {
  const id = (await params).id;
  await requireSession(`/evals/datasets/${id}`);
  if (!/^ds_[0-9a-f]{20}$/.test(id)) notFound();
  const [me, ds, evs, ins] = await Promise.all([settle(api.me()), settle(api.dataset(id)), settle(api.datasetEvals(id)), settle(api.insights('30d'))]);
  if (!ds.ok && ds.error instanceof ApiError && ds.error.status === 404) notFound();
  const canDelete = me.ok && me.data.can_manage_keys;
  const agents = ins.ok ? ins.data.agents.map((a) => a.agent).filter(Boolean) : [];
  const latest = evs.ok ? evs.data.evals[0] : undefined;
  const failing = new Map<string, string[]>();
  for (const f of latest?.failures ?? []) failing.set(f.trace_id, [...(failing.get(f.trace_id) ?? []), `${f.scorer}: ${f.detail}`]);

  return (
    <>
      <div className="page-head">
        <div>
          <div className="sub">
            <Link href="/evals">Evals</Link> / Dataset
          </div>
          <h1 className="title">
            <span className="title-icon" aria-hidden="true">
              <LayersIcon />
            </span>
            {ds.ok ? ds.data.dataset.name : 'Dataset'}
          </h1>
          {ds.ok && ds.data.dataset.description ? <div className="sub">{ds.data.dataset.description}</div> : null}
        </div>
      </div>
      {!ds.ok ? (
        <ApiErrorPanel error={ds.error} />
      ) : (
        <>
          <section className="panel stack" style={{ marginBottom: 16 }}>
            <DatasetButtons datasetId={id} canDelete={canDelete} />
            {latest ? (
              <div>
                <div className="sub" style={{ marginBottom: 6 }}>
                  Last run {formatTime(latest.created_at)} on {formatCount(latest.items)} runs:{' '}
                  <b>{overall(latest.tallies) === null ? 'nothing graded' : `${formatPercent(overall(latest.tallies)!)} passed`}</b>
                </div>
                <table>
                  <thead>
                    <tr>
                      <th>Check</th>
                      <th className="num">Passed</th>
                      <th className="num">Failed</th>
                      <th className="num">
                        Skipped
                      </th>
                      <th className="num">Pass rate</th>
                    </tr>
                  </thead>
                  <tbody>
                    {latest.tallies.map((t) => (
                      <tr key={t.scorer_id}>
                        <td>{t.name}</td>
                        <td className="num">{formatCount(t.passed)}</td>
                        <td className="num">{t.failed ? <span className="badge badge-failed">{formatCount(t.failed)}</span> : '0'}</td>
                        <td className="num">{formatCount(t.skipped)}</td>
                        <td className="num">{passRate(t) === null ? '–' : formatPercent(passRate(t)!)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                <p className="sub" style={{ margin: '6px 0 0' }}>
                  Skipped: the check couldn&apos;t apply, for example no output was captured or a model had no known price.
                </p>
              </div>
            ) : (
              <div className="sub">Checks haven&apos;t been run on this dataset yet.</div>
            )}
          </section>

          <section className="panel panel-flush" aria-labelledby="items-h">
            <div className="section-title">
              <h2 id="items-h">Runs ({formatCount(ds.data.items.length)})</h2>
              <span className="sub">Up to 500</span>
            </div>
            <div style={{ padding: '10px 18px 0' }}>
              <AddRunsForm datasetId={id} agents={agents} />
            </div>
            {ds.data.items.length === 0 ? (
              <div className="empty">No runs yet. Add some with the filter above, or from a run&apos;s page.</div>
            ) : (
              <div style={{ overflowX: 'auto' }}>
                <table style={{ marginTop: 8 }}>
                  <thead>
                    <tr>
                      <th style={{ paddingLeft: 18 }}>Run</th>
                      <th>Output</th>
                      <th className="num">Steps</th>
                      <th className="num">Cost</th>
                      <th className="num">Duration</th>
                      <th aria-label="Remove" />
                    </tr>
                  </thead>
                  <tbody>
                    {ds.data.items.map((it) => {
                      const fails = failing.get(it.trace_id);
                      return (
                        <tr key={it.trace_id}>
                          <td style={{ paddingLeft: 18 }} className="truncate">
                            <Link href={`/traces/${it.trace_id}`} className="mono">
                              {it.trace_id.slice(0, 12)}
                            </Link>
                            <div className="sub">
                              {it.agent || 'Unnamed agent'}
                              {it.version ? ` · ${it.version}` : ''}
                              {it.failed ? ' · failed' : ''}
                            </div>
                            {it.source ? <div className="sub" title="Where this item came from">From {it.source}</div> : null}
                          </td>
                          <td style={{ maxWidth: 480 }}>
                            <div className="clamp-2" style={{ overflowWrap: 'anywhere' }}>
                              {it.output || <span className="sub">No output captured</span>}
                            </div>
                            {fails ? (
                              <div className="sub" style={{ color: 'var(--critical-text)', marginTop: 2 }}>
                                ✕ {fails.join(' · ')}
                              </div>
                            ) : null}
                          </td>
                          <td className="num">{formatCount(it.steps)}</td>
                          <td className="num">{formatCost(it.cost_usd, !it.cost_known)}</td>
                          <td className="num">{formatDuration(it.duration_ms)}</td>
                          <td style={{ paddingRight: 18 }}>
                            <RemoveItemButton datasetId={id} traceId={it.trace_id} />
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        </>
      )}
    </>
  );
}
