import Link from 'next/link';
import { BotIcon, ChartIcon, CheckCircleIcon, LayersIcon } from '@/components/Icons';
import { ApiErrorPanel, Help, settle } from '@/components/ui';
import { type Delta } from '@/lib/insights';
import { api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { toolFlags, toolUsageChanges, verdict, versionRows } from '@/lib/agents';
import { checkMatrix, passRate } from '@/lib/evals';
import { definitionLabel, problemHref } from '@/lib/outcomes';
import { fmtBytes } from '@/lib/context';
import { formatCost, formatCount, formatDuration, formatPercent, formatRelative } from '@/lib/format';
import { WINDOWS, WINDOW_MS, asWindow, one } from '@/lib/ids';
import { topicRows, topicsSummary } from '@/lib/topics';
import { AgentTabs } from './AgentTabs';
import { PassportPanel } from './PassportPanel';

export const dynamic = 'force-dynamic';

const WINDOW_LABEL = { '24h': 'Last 24 hours', '7d': 'Last 7 days', '30d': 'Last 30 days' } as const;
const PREVIOUS = { '24h': 'previous 24 hours', '7d': 'previous 7 days', '30d': 'previous 30 days' } as const;

function Change({ d }: { d?: Delta }) {
  if (!d || d.direction === 'flat' || d.direction === 'new') return null;
  const cls = d.good === undefined ? 'delta-neutral' : d.good ? 'delta-good' : 'delta-bad';
  return <div className={`delta ${cls}`} style={{ fontSize: 11.5, display: 'block' }}>{d.text}</div>;
}

export default async function AgentsPage({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  await requireSession('/agents');
  const sp = await searchParams;
  const window = asWindow(one(sp.window) ?? '7d');
  const [ins, tools, econ, features] = await Promise.all([
    settle(api.insights(window)),
    settle(api.tools(window)),
    settle(api.economics(window)),
    settle(api.features()),
  ]);
  const ledgerOn = features.ok && features.data.features.includes('ledger');
  const passportsOn = features.ok && features.data.features.includes('passports');
  const agents = ins.ok ? ins.data.agents : [];
  const requested = one(sp.agent);
  const agent = requested !== undefined && agents.some((a) => a.agent === requested) ? requested : agents[0]?.agent;
  const [versions, checks, usage, topics] =
    agent !== undefined
      ? await Promise.all([
          settle(api.agentVersions(agent, window)),
          settle(api.agentChecks(agent, window)),
          settle(api.toolUsage(agent, window)),
          settle(api.agentTopics(agent, window)),
        ])
      : [undefined, undefined, undefined, undefined];
  const topicList = topics?.ok ? topicRows(topics.data.topics) : [];
  const topicLine = topics?.ok ? topicsSummary(topics.data.topics, PREVIOUS[window]) : null;
  const usageChange = usage?.ok ? toolUsageChanges(usage.data.versions) : null;
  // A recent run of each compared version, to open side by side.
  const from = new Date(Date.now() - WINDOW_MS[window]).toISOString();
  const latest = async (version: string) => {
    if (!agent || !version) return undefined;
    const r = await settle(api.runs({ agent, version, from, limit: '1' }));
    return r.ok ? r.data.runs[0]?.trace_id : undefined;
  };
  const [runNow, runBefore] = usageChange ? await Promise.all([latest(usageChange.version), latest(usageChange.previous)]) : [undefined, undefined];
  const matrix = checks?.ok ? checkMatrix(checks.data) : null;
  const scoreVersions = checks?.ok ? [...new Set(checks.data.scores.map((s) => s.version))] : [];
  const scoreNames = checks?.ok ? [...new Set(checks.data.scores.map((s) => s.name))] : [];
  const rows = versions?.ok ? versionRows(versions.data.versions) : [];
  const econRows = econ.ok ? econ.data.agents.filter((e) => e.agent === agent) : [];
  const summary = verdict(rows);
  const now = Date.now();
  const q = (o: Record<string, string | undefined>) => {
    const p = new URLSearchParams();
    for (const [k, v] of Object.entries(o)) if (v !== undefined && !(k === 'window' && v === '7d')) p.set(k, v);
    const s = p.toString();
    return s ? `/agents?${s}` : '/agents';
  };

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="title">
            <span className="title-icon" aria-hidden="true">
              <BotIcon />
            </span>
            Agents
          </h1>
          <div className="sub">{WINDOW_LABEL[window]} · compare versions, check tools</div>
        </div>
        <div className="seg" role="group" aria-label="Time range">
          {WINDOWS.map((w) => (
            <Link key={w} href={q({ agent, window: w })} aria-current={w === window ? 'true' : undefined}>
              {w}
            </Link>
          ))}
        </div>
      </div>

      {ledgerOn ? <AgentTabs agent={agent} current="overview" /> : null}

      {!ins.ok ? <ApiErrorPanel error={ins.error} /> : null}

      {passportsOn && agent !== undefined ? <PassportPanel agent={agent} window={window} /> : null}

      <section className="panel panel-flush" style={{ marginBottom: 16 }} aria-labelledby="versions-h">
        <div className="section-title">
          <h2 id="versions-h" className="h-icon">
            <BotIcon />
            Versions
          </h2>
          <span className="sub">Each version against the one before it</span>
        </div>
        {agents.length === 0 ? (
          <div className="empty">No runs in this period.</div>
        ) : (
          <>
            <nav className="chips" aria-label="Agent" style={{ padding: '10px 18px 0' }}>
              {agents.map((a) => (
                <Link key={a.agent || '(none)'} href={q({ agent: a.agent, window })} className="chip" aria-current={a.agent === agent ? 'true' : undefined}>
                  {a.agent || 'Unnamed agent'}
                </Link>
              ))}
            </nav>
            {summary ? (
              <div className="banner-detail" style={{ padding: '10px 18px 0' }} role="status">
                {summary}
              </div>
            ) : null}
            {versions && !versions.ok ? (
              <div style={{ padding: 18 }}>
                <ApiErrorPanel error={versions.error} />
              </div>
            ) : (
              <div style={{ overflowX: 'auto' }}>
                <table style={{ marginTop: 8 }}>
                  <thead>
                    <tr>
                      <th style={{ paddingLeft: 18 }}>Version</th>
                      <th className="num">Runs</th>
                      <th className="num">Failed</th>
                      <th className="num">Cost / run</th>
                      <th className="num">p95</th>
                      <th className="num">Steps / run</th>
                      <th className="num">
                        Cache <Help>Share of input tokens served from the prompt cache.</Help>
                      </th>
                      <th className="num" style={{ paddingRight: 18 }}>
                        Tool errors
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((r) => (
                      <tr key={r.v.version || '(none)'}>
                        <td style={{ paddingLeft: 18 }}>
                          <Link href={`/runs?${new URLSearchParams({ ...(window === '24h' ? {} : { range: window }), ...(agent ? { agent } : {}) })}`}>
                            <b>{r.v.version || 'unversioned'}</b>
                          </Link>
                          <div className="sub">Last run {formatRelative(r.v.last_run_start, now)}</div>
                        </td>
                        <td className="num">{formatCount(r.v.runs)}</td>
                        <td className="num">
                          {formatPercent(r.failureRate)}
                          <Change d={r.vs?.failureRate} />
                        </td>
                        <td className="num">
                          {formatCost(r.costPerRun, r.v.cost_partial)}
                          <Change d={r.vs?.costPerRun} />
                        </td>
                        <td className="num">
                          {formatDuration(r.v.p95_run_ms)}
                          <Change d={r.vs?.p95} />
                        </td>
                        <td className="num">
                          {r.v.avg_steps.toFixed(1)}
                          <Change d={r.vs?.steps} />
                        </td>
                        <td className="num">{r.v.input_tokens ? formatPercent(r.cacheRate) : '–'}</td>
                        <td className="num" style={{ paddingRight: 18 }}>
                          {r.v.tool_calls ? formatPercent(r.toolErrorRate) : '–'}
                          <Change d={r.vs?.toolErrorRate} />
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            {usageChange ? (
              <div style={{ padding: '12px 18px 0' }} aria-labelledby="paths-h">
                <h3 id="paths-h" style={{ margin: '0 0 6px', fontSize: 14 }}>
                  Tool calls per run: {usageChange.version || 'unversioned'} vs {usageChange.previous || 'unversioned'}
                </h3>
                {usageChange.rows.some((r) => r.note) ? (
                  <ul className="story-lines">
                    {usageChange.rows
                      .filter((r) => r.note)
                      .slice(0, 5)
                      .map((r) => (
                        <li key={r.tool} className="t-neutral">
                          <span className="dot" aria-hidden="true">
                            •
                          </span>
                          <span>{r.note}</span>
                        </li>
                      ))}
                  </ul>
                ) : (
                  <div className="sub">Both versions call their tools about as often per run.</div>
                )}
                <table style={{ marginTop: 6 }}>
                  <thead>
                    <tr>
                      <th>Tool</th>
                      <th className="num">{usageChange.version || 'unversioned'}</th>
                      <th className="num">{usageChange.previous || 'unversioned'}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {usageChange.rows.slice(0, 8).map((r) => (
                      <tr key={r.tool}>
                        <td className="mono">{r.tool}</td>
                        <td className="num">{r.now.toFixed(1)}</td>
                        <td className="num">{r.before.toFixed(1)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                {runNow && runBefore ? (
                  <Link href={`/traces/compare?a=${runBefore}&b=${runNow}`} className="btn btn-sm" style={{ marginTop: 8 }}>
                    Compare a recent run of each
                  </Link>
                ) : null}
              </div>
            ) : null}
            <p className="sub" style={{ padding: '8px 18px 14px', margin: 0 }}>
              Versions come from <code>bindle.init(version=…)</code>. Changes are shown when both versions have at least 5 runs.
            </p>
          </>
        )}
      </section>

      {agent !== undefined ? (
        <section className="panel panel-flush" style={{ marginBottom: 16 }} aria-labelledby="economics-h">
          <div className="section-title">
            <h2 id="economics-h" className="h-icon">
              <CheckCircleIcon />
              Cost per successful outcome
            </h2>
            <Link href={problemHref({ kind: 'failed_runs', agent, window })}>Turn failed runs into eval cases →</Link>
          </div>
          {!econ.ok ? (
            <div style={{ padding: 18 }}>
              <ApiErrorPanel error={econ.error} />
            </div>
          ) : econRows.length === 0 ? (
            <div className="empty">No runs in this period.</div>
          ) : (
            <div style={{ overflowX: 'auto' }}>
              <table style={{ marginTop: 8 }}>
                <thead>
                  <tr>
                    <th style={{ paddingLeft: 18 }}>Version</th>
                    <th className="num">Runs</th>
                    <th className="num">
                      Successful <Help>Runs with a good outcome: from bindle.outcome() when the agent sends outcomes, otherwise runs with no failed step and no thumbs down.</Help>
                    </th>
                    <th className="num">Spend</th>
                    <th className="num">
                      Per success <Help>All priced spend of the version divided by its successful outcomes: what one good result costs, failures included.</Help>
                    </th>
                    <th className="num" style={{ paddingRight: 18 }}>
                      Wasted <Help>Spend on failed runs, runs whose outcome failed, and (without outcomes) runs users disliked.</Help>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {econRows.map((e) => {
                    const partial = e.unknown_cost_runs > 0;
                    return (
                      <tr key={e.version || '(none)'}>
                        <td style={{ paddingLeft: 18 }}>
                          <b>{e.version || 'unversioned'}</b>
                          <div className="sub">{definitionLabel(e.definition)}</div>
                        </td>
                        <td className="num">{formatCount(e.runs)}</td>
                        <td className="num">
                          {formatCount(e.successes)}
                          {e.runs ? <div className="sub">{formatPercent(e.successes / e.runs)}</div> : null}
                        </td>
                        <td className="num" title={partial ? `${e.unknown_cost_runs} runs had model calls with no known price` : undefined}>
                          {formatCost(e.cost_usd, partial)}
                        </td>
                        <td className="num">{e.cost_per_success_usd === null ? '–' : formatCost(e.cost_per_success_usd, partial)}</td>
                        <td className="num" style={{ paddingRight: 18 }}>
                          {formatCost(e.wasted_usd, partial)}
                          {e.cost_usd > 0 ? <div className="sub">{formatPercent(e.wasted_usd / e.cost_usd)} of spend</div> : null}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
          <p className="sub" style={{ padding: '8px 18px 14px', margin: 0 }}>
            Send <code>bindle.outcome(&quot;ticket_resolved&quot;)</code> when a run does its job (or <code>success=False</code> when it doesn&apos;t) to measure real outcomes. Unpriced model calls are never estimated: amounts marked as partial are a lower bound.
          </p>
        </section>
      ) : null}

      {agent !== undefined ? (
        <section className="panel panel-flush" style={{ marginBottom: 16 }} aria-labelledby="checks-h">
          <div className="section-title">
            <h2 id="checks-h" className="h-icon">
              <CheckCircleIcon />
              Checks and scores
            </h2>
            <Link href="/evals">Manage checks →</Link>
          </div>
          {checks && !checks.ok ? (
            <div style={{ padding: 18 }}>
              <ApiErrorPanel error={checks.error} />
            </div>
          ) : !matrix || (matrix.rows.length === 0 && scoreNames.length === 0) ? (
            <div className="empty">
              No checks or scores for this agent yet. <Link href="/evals">Add a check</Link>, or send scores with <code>bindle.score()</code>.
            </div>
          ) : (
            <div style={{ overflowX: 'auto' }}>
              <table style={{ marginTop: 8 }}>
                <thead>
                  <tr>
                    <th style={{ paddingLeft: 18 }}>Check</th>
                    {(matrix.rows.length ? matrix.versions : scoreVersions).map((v) => (
                      <th key={v || '(none)'} className="num">
                        {v || 'unversioned'}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {matrix.rows.map((row) => (
                    <tr key={row.name}>
                      <td style={{ paddingLeft: 18 }}>{row.name}</td>
                      {row.cells.map((c, i) => {
                        const pr = c ? passRate(c) : null;
                        return (
                          <td key={i} className="num" title={c ? `${c.passed} passed, ${c.failed} failed, ${c.skipped} skipped` : undefined}>
                            {pr === null ? '–' : formatPercent(pr)}
                          </td>
                        );
                      })}
                    </tr>
                  ))}
                  {scoreNames.map((name) => (
                    <tr key={`score:${name}`}>
                      <td style={{ paddingLeft: 18 }}>
                        {name} <span className="badge">score</span>
                      </td>
                      {(matrix.rows.length ? matrix.versions : scoreVersions).map((v) => {
                        const s = checks!.ok ? checks!.data.scores.find((x) => x.name === name && x.version === v) : undefined;
                        const graded = s ? s.passed + s.failed : 0;
                        return (
                          <td key={v || '(none)'} className="num" title={s ? `${s.count} scores` : undefined}>
                            {!s ? '–' : graded ? formatPercent(s.passed / graded) : s.valued ? s.average.toFixed(2) : '–'}
                          </td>
                        );
                      })}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <p className="sub" style={{ padding: '8px 18px 14px', margin: 0 }}>
            Checks run on up to 100 recent runs per version. Scores come from <code>bindle.score()</code>: pass rate when they pass or fail, else the average value.
          </p>
        </section>
      ) : null}

      {agent !== undefined ? (
        <section className="panel panel-flush" style={{ marginBottom: 16 }} aria-labelledby="topics-h">
          <div className="section-title">
            <h2 id="topics-h" className="h-icon">
              <ChartIcon />
              What users ask about
            </h2>
            <span className="sub">
              {agent || 'Unnamed agent'} · compared with the {PREVIOUS[window]}{' '}
              <Help>
                Words in users&apos; messages (the last user message sent to the model, or the agent&apos;s input) that appear in a clearly larger share of runs
                than in the period before, or that are new. Each run counts once. Needs content capture on.
              </Help>
            </span>
          </div>
          {!topics?.ok ? (
            <div style={{ padding: 18 }}>{topics ? <ApiErrorPanel error={topics.error} /> : null}</div>
          ) : topicList.length === 0 ? (
            <div className="empty">{topicLine ? `${topicLine} No new or rising topics.` : 'No user messages in this period.'}</div>
          ) : (
            <>
              {topicLine ? (
                <p className="sub" style={{ padding: '8px 18px 0', margin: 0 }}>
                  {topicLine}
                </p>
              ) : null}
              <div style={{ overflowX: 'auto' }}>
                <table style={{ marginTop: 8 }}>
                  <thead>
                    <tr>
                      <th style={{ paddingLeft: 18 }}>Topic</th>
                      <th>Change</th>
                      <th style={{ paddingRight: 18 }}>Example runs</th>
                    </tr>
                  </thead>
                  <tbody>
                    {topicList.map((t) => (
                      <tr key={t.term}>
                        <td style={{ paddingLeft: 18 }}>
                          <b>{t.term}</b>{' '}
                          <span className={`badge${t.kind === 'new' ? ' badge-warn' : ''}`} style={{ marginLeft: 6 }}>
                            {t.kind === 'new' ? 'New' : 'Rising'}
                          </span>
                        </td>
                        <td>{t.detail}</td>
                        <td style={{ paddingRight: 18 }}>
                          {t.examples.length === 0 ? (
                            <span className="sub">–</span>
                          ) : (
                            t.examples.map((id, i) => (
                              <span key={id}>
                                {i > 0 ? ', ' : ''}
                                <Link href={`/traces/${id}`} className="mono">
                                  {id.slice(0, 8)}
                                </Link>
                              </span>
                            ))
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </>
          )}
          <p className="sub" style={{ padding: '8px 18px 14px', margin: 0 }}>
            Topics come from word statistics, not a model: no message leaves Bindle. <Link href={`/search?q=${encodeURIComponent(topicList[0]?.term ?? '')}&range=${window}`}>Search runs</Link> to read what users wrote.
          </p>
        </section>
      ) : null}

      <section className="panel panel-flush" aria-labelledby="tools-h">
        <div className="section-title">
          <h2 id="tools-h" className="h-icon">
            <LayersIcon />
            Tools
          </h2>
          <span className="sub">All agents · most failures first</span>
        </div>
        {!tools.ok ? (
          <div style={{ padding: 18 }}>
            <ApiErrorPanel error={tools.error} />
          </div>
        ) : tools.data.tools.length === 0 ? (
          <div className="empty">No tool calls in this period.</div>
        ) : (
          <div style={{ overflowX: 'auto' }}>
            <table style={{ marginTop: 8 }}>
              <thead>
                <tr>
                  <th style={{ paddingLeft: 18 }}>Tool</th>
                  <th className="num">Calls</th>
                  <th className="num">Failed</th>
                  <th className="num">
                    Bad arguments <Help>Failures whose error mentions invalid arguments, a missing parameter or schema validation: the model misread the tool.</Help>
                  </th>
                  <th className="num">p50 / p95</th>
                  <th className="num">
                    Result size <Help>Average and largest result. Large results are carried in every later prompt.</Help>
                  </th>
                  <th style={{ paddingRight: 18 }}>Most common error</th>
                </tr>
              </thead>
              <tbody>
                {tools.data.tools.map((t) => {
                  const flags = toolFlags(t);
                  return (
                    <tr key={t.tool_name}>
                      <td className="truncate" style={{ paddingLeft: 18 }}>
                        {t.sample_trace_id ? (
                          <Link href={`/traces/${t.sample_trace_id}?step=${t.sample_step_id}`}>
                            <b>{t.tool_name}</b>
                          </Link>
                        ) : (
                          <b>{t.tool_name}</b>
                        )}
                        <div className="sub">
                          {t.framework ? `${t.framework} · ` : ''}
                          {formatCount(t.runs)} run{t.runs === 1 ? '' : 's'}
                          {flags.map((f) => (
                            <span key={f} className="badge badge-warn" style={{ marginLeft: 6 }}>
                              {f}
                            </span>
                          ))}
                        </div>
                      </td>
                      <td className="num">{formatCount(t.calls)}</td>
                      <td className="num">{t.failures ? `${formatCount(t.failures)} (${formatPercent(t.failures / t.calls)})` : '0'}</td>
                      <td className="num">{formatCount(t.argument_errors)}</td>
                      <td className="num">
                        {formatDuration(t.p50_ms)} / {formatDuration(t.p95_ms)}
                      </td>
                      <td className="num">{t.max_output_bytes ? `${fmtBytes(Math.round(t.avg_output_bytes))} / ${fmtBytes(t.max_output_bytes)}` : '–'}</td>
                      <td style={{ paddingRight: 18 }}>{t.top_error ? <code style={{ fontSize: 12 }}>{t.top_error}</code> : <span className="sub">–</span>}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
        <p className="sub" style={{ padding: '8px 18px 14px', margin: 0 }}>
          Includes tools behind MCP servers and Amazon Bedrock AgentCore Gateway (through the CloudWatch span connector). Result sizes need content capture on.
        </p>
      </section>
    </>
  );
}
