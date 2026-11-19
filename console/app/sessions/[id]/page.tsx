import Link from 'next/link';
import { notFound } from 'next/navigation';
import { ChatIcon } from '@/components/Icons';
import { ApiErrorPanel, StatusBadge, settle } from '@/components/ui';
import { ApiError, api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { formatCost, formatCount, formatDuration, formatTime } from '@/lib/format';
import { agentText, feedbackLabel, userText } from '@/lib/sessions';

export const dynamic = 'force-dynamic';

export default async function SessionPage({ params }: { params: Promise<{ id: string }> }) {
  const id = decodeURIComponent((await params).id);
  await requireSession(`/sessions/${encodeURIComponent(id)}`);
  const res = await settle(api.session(id));
  if (!res.ok && res.error instanceof ApiError && (res.error.status === 404 || res.error.status === 400)) notFound();

  return (
    <>
      <div className="page-head">
        <div>
          <div className="sub">
            <Link href="/sessions">Sessions</Link> / Conversation
          </div>
          <h1 className="title">
            <span className="title-icon" aria-hidden="true">
              <ChatIcon />
            </span>
            <span className="mono" style={{ fontSize: '0.8em', overflowWrap: 'anywhere' }}>
              {id}
            </span>
          </h1>
          {res.ok ? (
            <div className="sub">
              {res.data.agent || 'Unnamed agent'} · {formatCount(res.data.turns.length)} turn{res.data.turns.length === 1 ? '' : 's'} ·{' '}
              {formatCost(res.data.cost_usd, res.data.cost_partial)} · {formatDuration(res.data.duration_ms)} agent time
              {feedbackLabel(res.data.feedback) ? ` · ${feedbackLabel(res.data.feedback)}` : ''}
            </div>
          ) : null}
        </div>
      </div>
      {!res.ok ? (
        <ApiErrorPanel error={res.error} />
      ) : (
        <section className="panel stack" aria-label="Conversation">
          {res.data.truncated ? <div className="sub">Showing the first 200 turns.</div> : null}
          {res.data.turns.map((t, i) => {
            const user = userText(t.input);
            const answer = agentText(t.output);
            const fb = feedbackLabel(t.feedback);
            return (
              <article key={t.run.trace_id} className="turn">
                <div className="turn-head">
                  <span className="sub">Turn {i + 1}</span>
                  <StatusBadge status={t.run.status} />
                  <span className="sub" title={formatTime(t.run.started_at)}>
                    {formatTime(t.run.started_at)} · {formatCount(t.run.steps)} steps · {formatCost(t.run.cost_usd, t.run.cost_partial)}
                  </span>
                  {fb ? <span className={`badge ${t.feedback && t.feedback.negative > t.feedback.positive ? 'badge-failed' : 'badge-ok'}`}>{fb}</span> : null}
                  <Link href={`/traces/${t.run.trace_id}`} style={{ marginLeft: 'auto' }}>
                    Open run →
                  </Link>
                </div>
                <div className="bubble bubble-user">{user || <span className="sub">No input captured</span>}</div>
                <div className="bubble bubble-agent">{answer || <span className="sub">No answer captured</span>}</div>
              </article>
            );
          })}
        </section>
      )}
    </>
  );
}
