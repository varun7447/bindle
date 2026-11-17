import Link from 'next/link';
import { RunsChart } from '@/components/RunsChart';
import { ApiErrorPanel, RunsTable, Tile, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { fillSeries } from '@/lib/chart';
import { formatCost, formatCount, formatDuration, formatPercent, formatRelative } from '@/lib/format';
import { WINDOWS, asWindow, one } from '@/lib/ids';

const WINDOW_LABEL = { '24h': 'Last 24 hours', '7d': 'Last 7 days', '30d': 'Last 30 days' } as const;

export default async function OverviewPage({
  searchParams,
}: {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const window = asWindow(one((await searchParams).window));
  const [ov, alerts, recent] = await Promise.all([
    settle(api.overview(window)),
    settle(api.alerts('1h')),
    settle(api.runs({ limit: '5' })),
  ]);
  const now = Date.now();

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Overview</h1>
          <div className="sub">{WINDOW_LABEL[window]} · times in UTC</div>
        </div>
      </div>
      <div className="filters">
        <div className="seg" role="group" aria-label="Time range">
          {WINDOWS.map((w) => (
            <Link key={w} href={w === '24h' ? '/' : `/?window=${w}`} aria-current={w === window ? 'true' : undefined}>
              {w}
            </Link>
          ))}
        </div>
      </div>

      {!ov.ok ? (
        // One notice for the page: the other panels would fail the same way.
        <ApiErrorPanel error={ov.error} />
      ) : ov.data.runs === 0 ? (
        <div className="panel">
          <div className="empty">
            No runs in this range yet. <Link href="/connect">Connect an agent</Link> to send your first trace.
          </div>
        </div>
      ) : (
        <>
          <section className="tiles" aria-label="Key metrics">
            <Tile label="Runs" value={formatCount(ov.data.runs)} note={`${formatCount(ov.data.failed_runs)} failed`} />
            <Tile label="Run failure rate" value={formatPercent(ov.data.run_failure_rate)} note={`Step failure rate ${formatPercent(ov.data.step_failure_rate)}`} />
            <Tile label="p95 run duration" value={formatDuration(ov.data.p95_run_ms)} note={`p50 ${formatDuration(ov.data.p50_run_ms)}`} />
            <Tile
              label="Cost"
              value={formatCost(ov.data.cost_usd, ov.data.cost_partial)}
              note={ov.data.cost_partial ? `${formatCount(ov.data.cost_unknown_steps)} model steps have no known price` : 'From known model prices'}
            />
            <Tile label="Tokens" value={formatCount(ov.data.input_tokens + ov.data.output_tokens)} note={`${formatCount(ov.data.input_tokens)} in · ${formatCount(ov.data.output_tokens)} out`} />
          </section>

          <div className="grid-2" style={{ marginBottom: 16 }}>
            <section className="panel" aria-labelledby="runs-chart">
              <h2 id="runs-chart">Runs over time</h2>
              <RunsChart buckets={fillSeries(ov.data.series, ov.data.from, ov.data.to, ov.data.bucket_seconds)} bucketSeconds={ov.data.bucket_seconds} />
            </section>
            <div className="stack">
              <section className="panel" aria-labelledby="alerts-h">
                <h2 id="alerts-h">Alerts · last hour</h2>
                {!alerts.ok ? (
                  <ApiErrorPanel error={alerts.error} />
                ) : alerts.data.alerts.length === 0 ? (
                  <div className="sub">No runaway loops detected.</div>
                ) : (
                  <ul style={{ margin: 0, padding: 0, listStyle: 'none', display: 'grid', gap: 8 }}>
                    {alerts.data.alerts.map((a) => (
                      <li key={`${a.trace_id}-${a.tool_name}`} className="notice notice-warn">
                        <span aria-hidden="true">⚠</span>
                        <div>
                          <strong>Runaway loop:</strong> <code>{a.tool_name}</code> called {a.repeats}× with the same input
                          (threshold {a.threshold}) · {formatRelative(a.last_seen, now)}
                          <div>
                            <Link href={`/traces/${a.trace_id}`}>View trace</Link>
                          </div>
                        </div>
                      </li>
                    ))}
                  </ul>
                )}
              </section>
              <section className="panel" aria-labelledby="tools-h">
                <h2 id="tools-h">Top failing tools</h2>
                {ov.data.top_failing_tools.length === 0 ? (
                  <div className="sub">No tool failures.</div>
                ) : (
                  <table>
                    <thead>
                      <tr>
                        <th>Tool</th>
                        <th className="num">Failures</th>
                        <th className="num">Calls</th>
                        <th className="num">Rate</th>
                      </tr>
                    </thead>
                    <tbody>
                      {ov.data.top_failing_tools.map((t) => (
                        <tr key={t.tool_name}>
                          <td className="mono truncate">{t.tool_name}</td>
                          <td className="num">{formatCount(t.failures)}</td>
                          <td className="num">{formatCount(t.calls)}</td>
                          <td className="num">{formatPercent(t.failures / t.calls)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </section>
            </div>
          </div>
        </>
      )}

      {ov.ok ? (
      <section className="panel" aria-labelledby="recent-h">
        <div className="page-head" style={{ marginBottom: 8 }}>
          <h2 id="recent-h" style={{ margin: 0 }}>Recent runs</h2>
          <Link href="/runs">All runs →</Link>
        </div>
        {!recent.ok ? <ApiErrorPanel error={recent.error} /> : <RunsTable runs={recent.data.runs} now={now} />}
      </section>
      ) : null}
    </>
  );
}
