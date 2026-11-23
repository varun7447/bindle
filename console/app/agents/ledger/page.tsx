import Link from 'next/link';
import { notFound } from 'next/navigation';
import { BotIcon, LayersIcon } from '@/components/Icons';
import { ApiErrorPanel, Help, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { formatCount } from '@/lib/format';
import { one } from '@/lib/ids';
import { LEDGER_DAYS, asLedgerDays, entryCost, ledgerSentence, shortHash, summaryHref, utcDay, utcStamp } from '@/lib/ledger';
import { AgentTabs } from '../AgentTabs';

export const dynamic = 'force-dynamic';

const ICON = { good: '✓', critical: '✕', empty: '–' } as const;

export default async function LedgerPage({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  await requireSession('/agents/ledger');
  const sp = await searchParams;
  const features = await settle(api.features());
  if (!features.ok || !features.data.features.includes('ledger')) notFound();

  const days = asLedgerDays(one(sp.days));
  const before = Number(one(sp.before)) || undefined;
  const ins = await settle(api.insights('30d'));
  const agents = ins.ok ? ins.data.agents.map((a) => a.agent) : [];
  const requested = one(sp.agent);
  const agent = requested ?? agents[0];
  const choices = agent !== undefined && !agents.includes(agent) ? [agent, ...agents] : agents;

  const [list, check] =
    agent !== undefined ? await Promise.all([settle(api.ledger(agent, days, before)), settle(api.ledgerVerify(agent, days))]) : [undefined, undefined];
  const head = check?.ok ? ledgerSentence(check.data.result, days) : null;
  const q = (o: Record<string, string | undefined>) => {
    const p = new URLSearchParams();
    for (const [k, v] of Object.entries(o)) if (v !== undefined && !(k === 'days' && v === '30')) p.set(k, v);
    const s = p.toString();
    return s ? `/agents/ledger?${s}` : '/agents/ledger';
  };
  const recentDays = [1, 2, 3, 4, 5, 6, 7].map((n) => utcDay(n));

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
          <div className="sub">Ledger · a tamper-evident record of what each agent saw, decided, called and cost</div>
        </div>
        <div className="seg" role="group" aria-label="Period">
          {LEDGER_DAYS.map((d) => (
            <Link key={d} href={q({ agent, days: String(d) })} aria-current={d === days ? 'true' : undefined}>
              {d === 1 ? 'Today' : `${d}d`}
            </Link>
          ))}
        </div>
      </div>

      <AgentTabs agent={agent} current="ledger" />

      {choices.length > 0 ? (
        <nav className="chips" aria-label="Agent" style={{ marginBottom: 16 }}>
          {choices.map((a) => (
            <Link key={a || '(none)'} href={q({ agent: a, days: String(days) })} className="chip" aria-current={a === agent ? 'true' : undefined}>
              {a || 'Unnamed agent'}
            </Link>
          ))}
        </nav>
      ) : null}

      {agent === undefined ? (
        <section className="panel">
          <div className="empty">No agents have sent runs in the last 30 days.</div>
        </section>
      ) : (
        <>
          {check && !check.ok ? <ApiErrorPanel error={check.error} /> : null}
          {head ? (
            <div className={`banner banner-${head.tone}`} role="status">
              <span className="banner-icon" aria-hidden="true">
                {ICON[head.tone]}
              </span>
              <div>
                <div className="banner-title">{head.title}</div>
                <div className="banner-detail">{head.detail}</div>
              </div>
            </div>
          ) : null}

          <section className="panel" style={{ marginBottom: 16 }} aria-labelledby="summary-h">
            <h2 id="summary-h" style={{ marginTop: 0 }}>
              Daily summary{' '}
              <Help>A plain-English account of one UTC day: what the agent did, what failed, what it cost and any security signals, with that day&apos;s first and last hash. Computed from the ledger, so it can be used as audit evidence.</Help>
            </h2>
            <div className="chips" aria-label="Download a day">
              {recentDays.map((d) => (
                <a key={d} className="chip" href={summaryHref(agent, d)} download>
                  {d}
                </a>
              ))}
            </div>
            <form action="/console-api/agents/ledger/summary" method="get" style={{ display: 'flex', gap: 8, alignItems: 'center', marginTop: 10 }}>
              <input type="hidden" name="agent" value={agent} />
              <label className="sub" htmlFor="ledger-day">
                Another day
              </label>
              <input id="ledger-day" type="date" name="day" required defaultValue={recentDays[0]} />
              <button type="submit" className="btn btn-sm">
                Download summary
              </button>
            </form>
          </section>

          <section className="panel panel-flush" aria-labelledby="entries-h">
            <div className="section-title">
              <h2 id="entries-h" className="h-icon">
                <LayersIcon />
                Entries
              </h2>
              <span className="sub">Newest first · times in UTC · no prompts or outputs are stored here</span>
            </div>
            {list && !list.ok ? (
              <div style={{ padding: 18 }}>
                <ApiErrorPanel error={list.error} />
              </div>
            ) : list?.ok && list.data.entries.length === 0 ? (
              <div className="empty">No entries in this period.</div>
            ) : list?.ok ? (
              <div style={{ overflowX: 'auto' }}>
                <table>
                  <thead>
                    <tr>
                      <th className="num" style={{ paddingLeft: 18 }}>
                        #
                      </th>
                      <th>When</th>
                      <th>Step</th>
                      <th>Outcome</th>
                      <th className="num">Tokens</th>
                      <th className="num">Cost</th>
                      <th>Signals</th>
                      <th style={{ paddingRight: 18 }}>
                        Hash <Help>SHA-256 of this entry and the hash of the one before it. Changing any entry changes every hash after it.</Help>
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {list.data.entries.map((e) => (
                      <tr key={e.seq}>
                        <td className="num mono" style={{ paddingLeft: 18 }}>
                          {formatCount(e.seq)}
                        </td>
                        <td style={{ whiteSpace: 'nowrap' }}>{utcStamp(e.at)}</td>
                        <td>
                          {e.trace_id ? (
                            <Link href={`/traces/${e.trace_id}${e.step_id ? `?step=${e.step_id}` : ''}`}>{e.step_name || e.kind}</Link>
                          ) : (
                            e.step_name || e.kind
                          )}
                          <div className="sub">
                            {e.step_type?.replace('_', ' ')}
                            {e.tool_name && e.tool_name !== e.step_name ? ` · ${e.tool_name}` : ''}
                          </div>
                        </td>
                        <td>
                          {e.status === 'failure' ? (
                            <>
                              <span className="badge badge-failed">✕ Failed</span>
                              {e.error_class ? <div className="sub">{e.error_class}</div> : null}
                            </>
                          ) : e.status === 'success' ? (
                            <span className="badge badge-ok">✓ OK</span>
                          ) : (
                            <span className="sub">—</span>
                          )}
                        </td>
                        <td className="num">{e.input_tokens + e.output_tokens > 0 ? formatCount(e.input_tokens + e.output_tokens) : '—'}</td>
                        <td className="num">{entryCost(e)}</td>
                        <td>
                          {e.security_flags?.length ? (
                            e.security_flags.map((f) => (
                              <span key={f} className="badge badge-warn" style={{ marginRight: 4 }}>
                                ! {f.replace(/_/g, ' ')}
                              </span>
                            ))
                          ) : (
                            <span className="sub">—</span>
                          )}
                        </td>
                        <td className="mono" style={{ paddingRight: 18 }} title={e.hash}>
                          {shortHash(e.hash)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                {list.data.next_before_seq ? (
                  <div style={{ padding: '12px 18px' }}>
                    <Link href={q({ agent, days: String(days), before: String(list.data.next_before_seq) })}>Older entries →</Link>
                  </div>
                ) : null}
              </div>
            ) : null}
          </section>
        </>
      )}
    </>
  );
}
