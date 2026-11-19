import { ApiErrorPanel, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { LimitsForm } from './LimitsForm';

export const dynamic = 'force-dynamic';

export default async function LimitsPage() {
  await requireSession('/settings/limits');
  const [me, limits, insights] = await Promise.all([settle(api.me()), settle(api.limits()), settle(api.insights('7d'))]);
  const canEdit = me.ok && me.data.can_manage_keys;
  const agents = insights.ok ? insights.data.agents.map((a) => a.agent).filter(Boolean) : [];
  return (
    <>
      <div className="section-intro">
        <div className="sub">
          Pause an agent, cap each run&apos;s steps or tokens, or set a daily budget. Limits take effect where your agent calls{' '}
          <code>bindle.check_limits()</code> (Python) or <code>checkLimits()</code> (TypeScript), which raises an error your code can
          handle. Bindle never stops an agent anywhere else. Budgets use priced spend since 00:00 UTC; when one is exceeded, your alert
          channels are notified once that day.
        </div>
      </div>
      {!limits.ok ? (
        <ApiErrorPanel error={limits.error} />
      ) : (
        <>
          {!canEdit ? (
            <div className="notice" role="note" style={{ marginBottom: 16 }}>
              <span aria-hidden="true">ⓘ</span>
              <div>Only owners and admins can change agent limits.</div>
            </div>
          ) : null}
          <LimitsForm data={limits.data} agents={agents} canEdit={canEdit} />
          <section className="panel" style={{ marginTop: 16 }}>
            <h2>Add the check to your agent</h2>
            <pre className="code mono">{`import bindle

bindle.init(agent="refunds")

while not done:
    bindle.check_limits()   # raises bindle.LimitExceeded when paused or over a limit
    step()`}</pre>
            <p className="sub" style={{ marginBottom: 0 }}>
              A row for <code>*</code> applies to every agent without a row of its own. Steps are counted per run in the SDK, tokens
              from LLM calls. Unpriced models don&apos;t count toward budgets.
            </p>
          </section>
        </>
      )}
    </>
  );
}
