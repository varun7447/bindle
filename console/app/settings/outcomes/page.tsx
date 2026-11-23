import { notFound } from 'next/navigation';
import { ApiErrorPanel, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { formatRelative } from '@/lib/format';
import { eventState, gradingOn, outcomeLabel } from '@/lib/grading';
import { CreateEndpointForm, RevokeEndpointForm, SettingsForm } from './Forms';

export const dynamic = 'force-dynamic';

const STATUS_BADGE = {
  pending: { cls: 'badge badge-warn', icon: '…', text: 'Pending' },
  matched: { cls: 'badge badge-ok', icon: '✓', text: 'Matched' },
  unmatched: { cls: 'badge badge-failed', icon: '✕', text: 'Unmatched' },
} as const;

// Settings → Outcomes (feature outcome_grading): reversal outcomes, signed
// webhook endpoints, and the events business systems reported.
export default async function OutcomesSettingsPage() {
  await requireSession('/settings/outcomes');
  const features = await settle(api.features());
  if (!features.ok || !gradingOn(features.data.features)) notFound();
  const [me, settings, endpoints, events] = await Promise.all([
    settle(api.me()),
    settle(api.outcomeSettings()),
    settle(api.outcomeEndpoints()),
    settle(api.outcomeEvents()),
  ]);
  const canManage = me.ok && me.data.can_manage_keys;
  const ingestUrl = (process.env.BINDLE_PUBLIC_INGEST_URL ?? '').replace(/\/+$/, '') || '<your Bindle ingest URL>';
  const evs = events.ok ? events.data.events : [];
  const unmatched = evs.filter((e) => e.status === 'unmatched').length;
  const now = Date.now();

  return (
    <>
      <div className="section-intro">
        <div className="sub">
          Business systems report what happened after a run (a ticket reopened, a refund reversed) without its trace ID. The agent tags the run with{' '}
          <code>bindle.correlate(&quot;ticket_id&quot;, id)</code>; only a hash of the value is stored.
        </div>
      </div>
      <div className="stack">
        <section className="panel" aria-labelledby="rev-h">
          <h2 id="rev-h">Reversals</h2>
          {!settings.ok ? (
            <ApiErrorPanel error={settings.error} />
          ) : (
            <SettingsForm reversals={settings.data.reversal_outcomes} pendingHours={settings.data.pending_hours} canManage={canManage} />
          )}
        </section>

        <section className="panel" aria-labelledby="send-h">
          <h2 id="send-h">Sending events</h2>
          <p className="sub" style={{ marginTop: 0 }}>With an ingest key:</p>
          <pre className="code">{`curl -X POST ${ingestUrl}/v1/outcome-events \\
  -H "Authorization: Bearer $BINDLE_API_KEY" -H "Content-Type: application/json" \\
  -d '{"correlation": {"ticket_id": "T-123"}, "name": "ticket_reopened", "success": false}'`}</pre>
          <p className="sub">
            Or from a system that can sign webhooks, without a key: create an endpoint below. Each event is stored once (a retry is the same event) and
            matched to its run within about a minute, or retried while the run may still be arriving.
          </p>
          {canManage ? <CreateEndpointForm ingestUrl={ingestUrl} /> : null}
          {!endpoints.ok ? (
            <ApiErrorPanel error={endpoints.error} />
          ) : endpoints.data.endpoints.length === 0 ? null : (
            <table style={{ marginTop: 12 }}>
              <thead>
                <tr>
                  <th>Endpoint</th>
                  <th>Created</th>
                  <th>Status</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {endpoints.data.endpoints.map((e) => (
                  <tr key={e.id}>
                    <td>
                      <b>{e.name}</b>
                      <div className="sub mono">/v1/outcome-events/webhook/{e.id}</div>
                    </td>
                    <td>{formatRelative(e.created_at, now)}</td>
                    <td>{e.revoked_at ? <span className="badge">Revoked</span> : <span className="badge badge-ok">✓ Active</span>}</td>
                    <td>{canManage && !e.revoked_at ? <RevokeEndpointForm id={e.id} /> : null}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </section>

        <section className="panel panel-flush" aria-labelledby="events-h">
          <div className="section-title">
            <h2 id="events-h">Recent events</h2>
            {unmatched > 0 ? <span className="sub">{unmatched} never matched a run</span> : null}
          </div>
          {!events.ok ? (
            <div style={{ padding: 18 }}>
              <ApiErrorPanel error={events.error} />
            </div>
          ) : evs.length === 0 ? (
            <div className="empty">No events yet.</div>
          ) : (
            <div style={{ overflowX: 'auto' }}>
              <table>
                <thead>
                  <tr>
                    <th style={{ paddingLeft: 18 }}>Outcome</th>
                    <th>Correlation</th>
                    <th>Received</th>
                    <th style={{ paddingRight: 18 }}>Status</th>
                  </tr>
                </thead>
                <tbody>
                  {evs.map((e) => {
                    const b = STATUS_BADGE[e.status];
                    return (
                      <tr key={e.id}>
                        <td style={{ paddingLeft: 18 }}>
                          <b>{outcomeLabel(e.name)}</b> {e.success ? '' : <span className="sub">(failed)</span>}
                          {e.comment ? <div className="sub">{e.comment}</div> : null}
                        </td>
                        <td className="mono">{e.correlation_keys.join(', ')}</td>
                        <td>
                          {formatRelative(e.received_at, now)}
                          <div className="sub">{e.source === 'webhook' ? 'webhook' : 'API'}</div>
                        </td>
                        <td style={{ paddingRight: 18 }}>
                          <span className={b.cls}>
                            <span aria-hidden="true">{b.icon}</span> {b.text}
                          </span>
                          <div className="sub">{eventState(e, now)}</div>
                          {e.matched_traces.slice(0, 3).map((t) => (
                            <div key={t} className="sub">
                              <a href={`/traces/${t}`}>
                                <code>{t.slice(0, 12)}…</code>
                              </a>
                            </div>
                          ))}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </section>
      </div>
    </>
  );
}
