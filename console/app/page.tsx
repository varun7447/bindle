import Link from 'next/link';
import { RunsChart } from '@/components/RunsChart';
import { ActivityIcon, AlertIcon, BotIcon, ChartIcon, ClockIcon, CoinIcon, HomeIcon } from '@/components/Icons';
import { ApiErrorPanel, Help, Severity, Tile, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { fillSeries } from '@/lib/chart';
import { formatCost, formatCount, formatDuration, formatPercent, formatRelative, formatTime } from '@/lib/format';
import { WINDOWS, asWindow, one } from '@/lib/ids';
import { agentLabel, attentionItems, delta, headline } from '@/lib/insights';
import { securityItems } from '@/lib/security';
import type { Insights } from '@/lib/types';

const WINDOW_LABEL = { '24h': 'Last 24 hours', '7d': 'Last 7 days', '30d': 'Last 30 days' } as const;
const PREVIOUS = { '24h': 'previous 24h', '7d': 'previous 7 days', '30d': 'previous 30 days' } as const;
const BANNER_ICON = { good: '✓', warning: '!', critical: '✕', empty: '·' } as const;

const EMPTY_INSIGHTS: Insights = { agents: [], issues: [], previous: { runs: 0, failed_runs: 0, cost_usd: 0, p95_run_ms: 0 } };

export default async function HomePage({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  await requireSession('/');
  const window = asWindow(one((await searchParams).window));
  const [ov, ins, loops, sec] = await Promise.all([
    settle(api.overview(window)),
    settle(api.insights(window)),
    settle(api.alerts('1h')),
    settle(api.security(window)),
  ]);
  const now = Date.now();

  if (!ov.ok) {
    return (
      <>
        <Head window={window} />
        <ApiErrorPanel error={ov.error} />
      </>
    );
  }
  const o = ov.data;
  const insights = ins.ok ? ins.data : EMPTY_INSIGHTS;
  const security = sec.ok ? securityItems(sec.data.signals, sec.data.explanations) : [];
  const items = attentionItems(o, insights, loops.ok ? loops.data.alerts : [], window, security);
  const head = headline(o, insights, items, WINDOW_LABEL[window]);
  const prev = insights.previous;
  const prevRate = prev.runs ? prev.failed_runs / prev.runs : 0;
  const vs = PREVIOUS[window];

  return (
    <>
      <Head window={window} />

      <section className={`banner banner-${head.tone}`} aria-live="polite">
        <span className="banner-icon" aria-hidden="true">
          {BANNER_ICON[head.tone]}
        </span>
        <div>
          <div className="banner-title">{head.title}</div>
          <div className="banner-detail">{head.detail}</div>
        </div>
        {head.tone === 'empty' ? (
          <Link href="/settings/connect" className="btn btn-primary" style={{ marginLeft: 'auto' }}>
            Connect an agent
          </Link>
        ) : null}
      </section>

      {o.runs === 0 ? null : (
        <>
          <section className="panel panel-flush" style={{ marginBottom: 16 }} aria-labelledby="attention-h">
            <div className="section-title">
              <h2 id="attention-h" className="h-icon"><AlertIcon />Needs attention</h2>
              <span className="sub">Most severe first · {WINDOW_LABEL[window].toLowerCase()}</span>
            </div>
            {!ins.ok ? (
              <div style={{ padding: 18 }}>
                <ApiErrorPanel error={ins.error} />
              </div>
            ) : items.length === 0 ? (
              <div className="all-clear">
                <span className="badge badge-ok">✓ OK</span> No failing steps, struggling agents or loops. Nice.
              </div>
            ) : (
              <ul className="attention">
                {items.slice(0, 12).map((it) => (
                  <li key={it.key}>
                    <Severity level={it.severity} />
                    <div>
                      <div className="a-title">{it.title}</div>
                      <div className="a-detail">{it.detail}</div>
                      {it.variants ? (
                        <ul className="a-variants" aria-label="Distinct errors">
                          {it.variants.map((v) => (
                            <li key={v.text}>
                              <Link href={v.href}>
                                <code>{v.text}</code>
                              </Link>
                              <span className="sub"> ×{formatCount(v.count)}</span>
                            </li>
                          ))}
                        </ul>
                      ) : null}
                    </div>
                    <Link href={it.href} className="a-cta">
                      {it.cta} →
                    </Link>
                  </li>
                ))}
              </ul>
            )}
          </section>

          <section className="tiles" aria-label="Key metrics">
            <Tile
              label="Runs"
              icon={<ActivityIcon />}
              help="A run is one end-to-end execution of an agent: one trace, made of steps (LLM calls, tool calls, …)."
              value={formatCount(o.runs)}
              delta={delta(o.runs, prev.runs)}
              vs={vs}
            />
            <Tile
              label="Failure rate"
              icon={<AlertIcon />}
              help="Share of runs where any step failed or the run itself ended in an error."
              value={formatPercent(o.run_failure_rate)}
              delta={delta(o.run_failure_rate, prevRate, { higherIsBetter: false, percentPoints: true })}
              vs={vs}
              note={`${formatCount(o.failed_runs)} failed runs`}
            />
            <Tile
              label="p95 duration"
              icon={<ClockIcon />}
              help="95% of runs finished faster than this. Shows the slow tail better than an average."
              value={formatDuration(o.p95_run_ms)}
              delta={delta(o.p95_run_ms, prev.p95_run_ms, { higherIsBetter: false })}
              vs={vs}
              note={`Median ${formatDuration(o.p50_run_ms)}`}
            />
            <Tile
              label="Cost"
              icon={<CoinIcon />}
              help="Model spend from known list prices. Models without a known price are counted as unknown, never guessed."
              value={formatCost(o.cost_usd, o.cost_partial)}
              delta={o.cost_partial && o.cost_usd === 0 ? undefined : delta(o.cost_usd, prev.cost_usd, { higherIsBetter: false })}
              vs={vs}
              note={`${formatCount(o.input_tokens + o.output_tokens)} tokens`}
            />
          </section>

          <div className="grid-2">
            <section className="panel" aria-labelledby="runs-chart">
              <div className="section-title">
                <h2 id="runs-chart" className="h-icon"><ChartIcon />Runs over time</h2>
                <Link href="/runs?status=failed">Failed runs →</Link>
              </div>
              <RunsChart buckets={fillSeries(o.series, o.from, o.to, o.bucket_seconds)} bucketSeconds={o.bucket_seconds} />
            </section>

            <section className="panel panel-flush" aria-labelledby="agents-h">
              <div className="section-title">
                <h2 id="agents-h" className="h-icon"><BotIcon />Agents</h2>
                <Link href="/agents">Compare versions →</Link>
              </div>
              {insights.agents.length === 0 ? (
                <div className="empty">No agents.</div>
              ) : (
                <table style={{ marginTop: 8 }}>
                  <thead>
                    <tr>
                      <th style={{ paddingLeft: 18 }}>Agent</th>
                      <th className="num">Runs</th>
                      <th className="num" style={{ paddingRight: 18 }}>
                        Failed <Help>Share of this agent&apos;s runs that failed.</Help>
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {insights.agents.slice(0, 8).map((a) => {
                      const rate = a.runs ? a.failed_runs / a.runs : 0;
                      return (
                        <tr key={a.agent || '(none)'}>
                          <td className="truncate" style={{ paddingLeft: 18 }}>
                            <Link href={`/runs?${new URLSearchParams({ ...(window === '24h' ? {} : { range: window }), ...(a.agent ? { agent: a.agent } : {}) })}`}>
                              {agentLabel(a.agent)}
                            </Link>
                            <div className="sub" title={formatTime(a.last_run_start)}>
                              Last run {formatRelative(a.last_run_start, now)} · p95 {formatDuration(a.p95_run_ms)}
                            </div>
                          </td>
                          <td className="num">{formatCount(a.runs)}</td>
                          <td className="num" style={{ paddingRight: 18 }}>
                            <span className="meter" title={`${a.failed_runs} of ${a.runs} runs failed`}>
                              <span className="meter-track" aria-hidden="true">
                                <span className="meter-fill" style={{ width: `${rate * 100}%` }} />
                              </span>
                              {formatPercent(rate)}
                            </span>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              )}
            </section>
          </div>
        </>
      )}
    </>
  );
}

function Head({ window }: { window: keyof typeof WINDOW_LABEL }) {
  return (
    <div className="page-head">
      <div>
        <h1 className="title"><span className="title-icon" aria-hidden="true"><HomeIcon /></span>Home</h1>
        <div className="sub">{WINDOW_LABEL[window]} · times in UTC</div>
      </div>
      <div className="seg" role="group" aria-label="Time range">
        {WINDOWS.map((w) => (
          <Link key={w} href={w === '24h' ? '/' : `/?window=${w}`} aria-current={w === window ? 'true' : undefined}>
            {w}
          </Link>
        ))}
      </div>
    </div>
  );
}
