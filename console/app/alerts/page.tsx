import Link from 'next/link';
import { BellIcon, ChatIcon, HistoryIcon, LinkIcon, MailIcon, SendIcon, SlidersIcon } from '@/components/Icons';
import { ApiErrorPanel, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { CHANNEL_KINDS, deliverySummary, describeRule } from '@/lib/alerting';
import { formatRelative, formatTime } from '@/lib/format';
import { ChannelActions, NewChannelForm, NewRuleForm, RuleRow } from './AlertForms';

export const dynamic = 'force-dynamic';

export default async function AlertsPage() {
  await requireSession('/alerts');
  const [me, rules, channels, events, features] = await Promise.all([
    settle(api.me()),
    settle(api.alertRules()),
    settle(api.alertChannels()),
    settle(api.alertEvents()),
    settle(api.features()),
  ]);
  const canEdit = me.ok && me.data.can_manage_keys;
  const now = Date.now();
  const noChannels = channels.ok && channels.data.channels.filter((c) => c.enabled).length === 0;

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="title"><span className="title-icon" aria-hidden="true"><BellIcon /></span>Alerts</h1>
          <div className="sub">
            Bindle checks these rules every minute and tells you where to look. Alerts carry counts, tool names and a link, never prompt or
            output content.
          </div>
        </div>
      </div>
      {!rules.ok ? (
        <ApiErrorPanel error={rules.error} />
      ) : (
        <div className="stack">
          {!canEdit ? (
            <div className="notice" role="note">
              <span aria-hidden="true">ⓘ</span>
              <div>Only owners and admins can change alerts.</div>
            </div>
          ) : null}
          {noChannels ? (
            <div className="notice notice-warn" role="note">
              <span aria-hidden="true">⚠</span>
              <div>Rules are on, but alerts have nowhere to go yet. Add a Slack, webhook or email channel below.</div>
            </div>
          ) : null}

          <section className="panel" aria-labelledby="rules-h">
            <h2 id="rules-h" className="h-icon"><SlidersIcon />Rules</h2>
            <div className="stack">
              {rules.data.rules.length === 0 ? <div className="empty">No rules.</div> : null}
              {rules.data.rules.map((r) => (
                <div key={r.id}>
                  <RuleRow rule={r} canEdit={canEdit} />
                  <div className="sub">{describeRule(r)}</div>
                </div>
              ))}
              {canEdit ? <NewRuleForm features={features.ok ? features.data.features : []} /> : null}
            </div>
          </section>

          <section className="panel" aria-labelledby="channels-h">
            <h2 id="channels-h" className="h-icon"><SendIcon />Channels</h2>
            {!channels.ok ? (
              <ApiErrorPanel error={channels.error} />
            ) : (
              <div className="stack">
                {channels.data.channels.length === 0 ? (
                  <div className="empty">No channels yet.</div>
                ) : (
                  <table>
                    <thead>
                      <tr>
                        <th>Name</th>
                        <th>Type</th>
                        <th>Destination</th>
                        <th>Status</th>
                        {canEdit ? <th>Actions</th> : null}
                      </tr>
                    </thead>
                    <tbody>
                      {channels.data.channels.map((c) => (
                        <tr key={c.id}>
                          <td className="truncate">{c.name}</td>
                          <td>
                            <span className="with-icon">
                              {c.kind === 'email' ? <MailIcon /> : c.kind === 'webhook' ? <LinkIcon /> : <ChatIcon />}
                              {CHANNEL_KINDS.find((k) => k.kind === c.kind)?.label ?? c.kind}
                            </span>
                          </td>
                          <td className="mono truncate">{c.target_display}</td>
                          <td>
                            {c.enabled ? (
                              <span className="badge badge-ok">
                                <span aria-hidden="true">✓</span> On
                              </span>
                            ) : (
                              <span className="badge">
                                <span aria-hidden="true">–</span> Paused
                              </span>
                            )}
                          </td>
                          {canEdit ? (
                            <td>
                              <ChannelActions id={c.id} name={c.name} enabled={c.enabled} />
                            </td>
                          ) : null}
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
                {canEdit ? <NewChannelForm /> : null}
              </div>
            )}
          </section>

          <section className="panel" aria-labelledby="history-h">
            <h2 id="history-h" className="h-icon"><HistoryIcon />Recent alerts</h2>
            {!events.ok ? (
              <ApiErrorPanel error={events.error} />
            ) : events.data.events.length === 0 ? (
              <div className="empty">Nothing has fired yet.</div>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>When</th>
                    <th>Alert</th>
                    <th>Run</th>
                    <th>Delivery</th>
                  </tr>
                </thead>
                <tbody>
                  {events.data.events.map((e) => {
                    const d = deliverySummary(e.delivery);
                    return (
                      <tr key={e.id}>
                        <td title={formatTime(e.fired_at)}>{formatRelative(e.fired_at, now)}</td>
                        <td>{e.summary}</td>
                        <td>
                          {e.trace_id ? (
                            <Link href={`/traces/${e.trace_id}`} className="mono">
                              {e.trace_id.slice(0, 12)}…
                            </Link>
                          ) : (
                            <span className="sub">–</span>
                          )}
                        </td>
                        <td>
                          <span className={d.ok ? 'badge badge-ok' : 'badge badge-warn'}>
                            <span aria-hidden="true">{d.ok ? '✓' : '!'}</span> {d.text}
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
      )}
    </>
  );
}
