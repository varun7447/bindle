import Link from 'next/link';
import { ChatIcon } from '@/components/Icons';
import { RunsTabs } from '@/components/RunsTabs';
import { ApiErrorPanel, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { formatCost, formatCount, formatDuration, formatRelative, formatTime } from '@/lib/format';
import { WINDOWS, asWindow, one } from '@/lib/ids';
import { problemHref } from '@/lib/outcomes';
import { feedbackLabel, outcome } from '@/lib/sessions';

export const dynamic = 'force-dynamic';

const TONE = { ok: 'badge-ok', warn: 'badge-warn', failed: 'badge-failed' } as const;

export default async function SessionsPage({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  await requireSession('/sessions');
  const sp = await searchParams;
  const window = asWindow(one(sp.range) ?? '7d');
  const agent = one(sp.agent)?.trim() || undefined;
  const res = await settle(api.sessions(window, agent));
  const now = Date.now();

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="title">
            <span className="title-icon" aria-hidden="true">
              <ChatIcon />
            </span>
            Sessions
          </h1>
          <div className="sub">Conversations: the runs that share a session ID, with how they ended and what users said.</div>
        </div>
        <RunsTabs current="/sessions" />
      </div>
      <form className="filters" method="get" action="/sessions" aria-label="Filter sessions">
        <select name="range" defaultValue={window} aria-label="Time range">
          {WINDOWS.map((w) => (
            <option key={w} value={w}>
              Last {w}
            </option>
          ))}
        </select>
        <input name="agent" placeholder="Agent" defaultValue={agent ?? ''} aria-label="Agent" maxLength={256} />
        <button type="submit" className="btn">
          Apply
        </button>
      </form>
      {!res.ok ? (
        <ApiErrorPanel error={res.error} />
      ) : res.data.sessions.length === 0 ? (
        <section className="panel">
          <div className="empty">
            No sessions in this period. Runs join a session when the agent sets one: <code>bindle.set_session(&quot;conversation-id&quot;)</code> or the
            framework&apos;s own session or conversation ID.
          </div>
        </section>
      ) : (
        <section className="panel panel-flush">
          {res.data.sessions.some((s) => (s.feedback?.negative ?? 0) > 0) ? (
            <div className="banner-detail" style={{ padding: '12px 18px 0' }}>
              {formatCount(res.data.sessions.filter((s) => (s.feedback?.negative ?? 0) > 0).length)} conversation(s) got a thumbs down.{' '}
              <Link href={problemHref({ kind: 'negative_feedback', ...(agent ? { agent } : {}), window })}>Add the disliked runs to a dataset →</Link>
            </div>
          ) : null}
          <table>
            <thead>
              <tr>
                <th style={{ paddingLeft: 18 }}>Session</th>
                <th>Outcome</th>
                <th className="num">Turns</th>
                <th className="num">Cost</th>
                <th className="num">Agent time</th>
                <th style={{ paddingRight: 18 }}>Last turn</th>
              </tr>
            </thead>
            <tbody>
              {res.data.sessions.map((s) => {
                const o = outcome(s);
                const fb = feedbackLabel(s.feedback);
                return (
                  <tr key={s.session_id}>
                    <td className="truncate" style={{ paddingLeft: 18, maxWidth: 360 }}>
                      <Link href={`/sessions/${encodeURIComponent(s.session_id)}`} className="mono">
                        {s.session_id}
                      </Link>
                      <div className="sub">{s.agent || 'Unnamed agent'}</div>
                    </td>
                    <td>
                      <span className={`badge ${TONE[o.tone]}`}>{o.label}</span>
                      {fb ? <div className="sub">{fb}</div> : null}
                    </td>
                    <td className="num">{formatCount(s.runs)}</td>
                    <td className="num">{formatCost(s.cost_usd, s.cost_partial)}</td>
                    <td className="num">{formatDuration(s.duration_ms)}</td>
                    <td style={{ paddingRight: 18 }} title={formatTime(s.last_start)}>
                      {formatRelative(s.last_start, now)}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </section>
      )}
    </>
  );
}
