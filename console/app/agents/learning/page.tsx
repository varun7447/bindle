import Link from 'next/link';
import { notFound } from 'next/navigation';
import { BotIcon, LayersIcon } from '@/components/Icons';
import { ApiErrorPanel, Help, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { formatCount, formatRelative } from '@/lib/format';
import { one } from '@/lib/ids';
import { STATUS_LABEL, causeLabel, isFlagged, learningOn, learningSentence, noteHref, vaultGraph } from '@/lib/learning';
import { AgentTabs } from '../AgentTabs';
import { VaultGraph } from './VaultGraph';

export const dynamic = 'force-dynamic';

export default async function LearningPage({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  await requireSession('/agents/learning');
  const sp = await searchParams;
  const features = await settle(api.features());
  if (!features.ok || !learningOn(features.data.features)) notFound();

  const [ins, all] = await Promise.all([settle(api.insights('30d')), settle(api.lessons())]);
  const agents = [...new Set([...(ins.ok ? ins.data.agents.map((a) => a.agent) : []), ...(all.ok ? all.data.lessons.map((l) => l.agent) : [])])].filter(
    (a) => a !== '',
  );
  const requested = one(sp.agent);
  const agent = requested || agents[0];
  const choices = agent && !agents.includes(agent) ? [agent, ...agents] : agents;
  const vault = agent ? await settle(api.vault(agent)) : undefined;
  const lessons = all.ok && agent ? all.data.lessons.filter((l) => l.agent === agent) : [];
  const notes = vault?.ok ? vault.data.notes : [];
  const graph = vault?.ok ? vaultGraph(vault.data.notes, vault.data.links) : null;
  const now = Date.now();

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
          <div className="sub">Learning · lessons from repeated failures, skills drafted from them, and each agent&apos;s knowledge vault</div>
        </div>
      </div>

      <AgentTabs agent={agent} current="learning" ledger={features.data.features.includes('ledger')} learning />

      {choices.length > 0 ? (
        <nav className="chips" aria-label="Agent" style={{ marginBottom: 16 }}>
          {choices.map((a) => (
            <Link key={a} href={`/agents/learning?${new URLSearchParams({ agent: a }).toString()}`} className="chip" aria-current={a === agent ? 'true' : undefined}>
              {a}
            </Link>
          ))}
        </nav>
      ) : null}

      {!agent ? (
        <section className="panel">
          <div className="empty">No named agents have sent runs in the last 30 days.</div>
        </section>
      ) : (
        <>
          {all.ok ? null : <ApiErrorPanel error={all.error} />}
          {vault && !vault.ok ? <ApiErrorPanel error={vault.error} /> : null}
          <div className="banner banner-empty" role="status">
            <span className="banner-icon" aria-hidden="true">
              ·
            </span>
            <div>
              <div className="banner-title">{learningSentence(agent, lessons, notes)}</div>
              <div className="banner-detail">
                {vault?.ok && !vault.data.passport
                  ? 'This agent has no passport: skills can be drafted, but not approved.'
                  : vault?.ok
                    ? `Skills are approved by the agent's owner, ${vault.data.owner}.`
                    : ''}
              </div>
            </div>
          </div>

          <section className="panel panel-flush" aria-labelledby="lessons-h" style={{ marginBottom: 16 }}>
            <div className="section-title">
              <h2 id="lessons-h" className="h-icon">
                <LayersIcon />
                Lessons{' '}
                <Help>
                  A lesson is the same failure (same step, tool and error shape) in at least {all.ok ? all.data.min_runs : 5} runs over the last 7 days,
                  recomputed hourly. Draft a skill from one with your own model: bindle skills draft --lesson &lt;id&gt; --writer module:function.
                </Help>
              </h2>
              <span className="sub">Most runs first</span>
            </div>
            {lessons.length === 0 ? (
              <div className="empty">No repeated failures for this agent in the last 7 days.</div>
            ) : (
              <div style={{ overflowX: 'auto' }}>
                <table>
                  <thead>
                    <tr>
                      <th style={{ paddingLeft: 18 }}>Lesson</th>
                      <th>Likely cause</th>
                      <th className="num">Runs</th>
                      <th>Last seen</th>
                      <th style={{ paddingRight: 18 }}>Evidence</th>
                    </tr>
                  </thead>
                  <tbody>
                    {lessons.map((l) => (
                      <tr key={l.id}>
                        <td style={{ paddingLeft: 18 }}>
                          {l.title}
                          <div className="sub mono">{l.id}</div>
                        </td>
                        <td>{causeLabel(l.cause)}</td>
                        <td className="num">{formatCount(l.runs)}</td>
                        <td>{formatRelative(l.last_seen, now)}</td>
                        <td style={{ paddingRight: 18 }}>
                          {l.evidence.slice(0, 3).map((t, i) => (
                            <span key={t}>
                              {i ? ', ' : ''}
                              <Link href={`/traces/${t}`} className="mono">
                                {t.slice(0, 8)}
                              </Link>
                            </span>
                          ))}
                          {l.evidence.length > 3 ? <span className="sub"> +{l.evidence.length - 3}</span> : null}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>

          <section className="panel panel-flush" aria-labelledby="vault-h" style={{ marginBottom: 16 }}>
            <div className="section-title">
              <h2 id="vault-h" className="h-icon">
                <LayersIcon />
                Knowledge vault{' '}
                <Help>
                  Markdown notes for this agent: skills, lessons, incident write-ups and notes, linked with [[links]]. Every change is versioned and, with the
                  ledger on, written to the agent&apos;s ledger. Sync with your repository: bindle vault pull and bindle vault push.
                </Help>
              </h2>
              <span className="sub">
                <a href={`/console-api/agents/vault/export?${new URLSearchParams({ agent }).toString()}`} download>
                  Export as a folder (zip)
                </a>
              </span>
            </div>
            {notes.length === 0 ? (
              <div className="empty">No notes yet. Push a drafted skill or a note with the CLI, or add one below.</div>
            ) : (
              <div style={{ overflowX: 'auto' }}>
                <table>
                  <thead>
                    <tr>
                      <th style={{ paddingLeft: 18 }}>Note</th>
                      <th>Kind</th>
                      <th>Status</th>
                      <th>Scan</th>
                      <th className="num">Version</th>
                      <th style={{ paddingRight: 18 }}>Changed</th>
                    </tr>
                  </thead>
                  <tbody>
                    {notes.map((n) => (
                      <tr key={n.path}>
                        <td style={{ paddingLeft: 18 }}>
                          <Link href={noteHref(agent, n.path)} className="mono">
                            {n.path}
                          </Link>
                        </td>
                        <td>{n.kind}</td>
                        <td>{n.status ? STATUS_LABEL[n.status] : '—'}</td>
                        <td>{isFlagged(n) ? (n.reviewed_sha256 === n.sha256 ? '✓ Cleared by a reviewer' : '! Flagged') : '✓ Clean'}</td>
                        <td className="num">{n.version}</td>
                        <td style={{ paddingRight: 18 }}>{formatRelative(n.updated_at, now)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            <form action="/agents/learning/note" method="get" style={{ display: 'flex', gap: 8, alignItems: 'center', padding: '12px 18px' }}>
              <input type="hidden" name="agent" value={agent} />
              <label className="sub" htmlFor="new-note">
                New note at
              </label>
              <input id="new-note" name="path" required placeholder="notes/incident-2026-11.md" pattern="[a-z0-9][a-z0-9._\-]*(/[A-Za-z0-9][A-Za-z0-9._\-]*)*\.md" />
              <input type="hidden" name="new" value="1" />
              <button type="submit" className="btn btn-sm">
                Write
              </button>
            </form>
          </section>

          {graph && graph.nodes.length > 1 ? (
            <section className="panel" aria-labelledby="graph-h">
              <h2 id="graph-h" style={{ marginTop: 0 }}>
                Links <Help>Notes and what they link to: other notes, runs, tools and agents. Select a node to open it.</Help>
              </h2>
              <VaultGraph graph={graph} agent={agent} />
            </section>
          ) : null}
        </>
      )}
    </>
  );
}
