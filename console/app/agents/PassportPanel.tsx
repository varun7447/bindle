import { ApiErrorPanel, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { formatRelative } from '@/lib/format';
import { passportFacts, passportSentence } from '@/lib/passports';

/** The agent's passport (what it may do) and how often it drifted outside it. */
export async function PassportPanel({ agent, window }: { agent: string; window: string }) {
  const [res, drift] = await Promise.all([settle(api.agentPassport(agent)), settle(api.passportDrift(window))]);
  const now = Date.now();
  return (
    <section className="panel" style={{ marginBottom: 16 }} aria-labelledby="passport-h">
      <div className="section-title" style={{ padding: 0 }}>
        <h2 id="passport-h">Passport</h2>
        <span className="sub">What this agent may do · observe-only</span>
      </div>
      {!res.ok ? (
        <ApiErrorPanel error={res.error} />
      ) : (
        <>
          <div className="banner-detail" role="status" style={{ marginTop: 8 }}>
            {passportSentence(agent, res.data.active, drift.ok ? drift.data : undefined)}
          </div>
          {res.data.active ? (
            <>
              <div className="facts" style={{ marginTop: 12 }}>
                {passportFacts(res.data.active.passport).map((f, i) => (
                  <span key={`${f.label}-${i}`}>
                    {f.label} <b>{f.value}</b>
                  </span>
                ))}
              </div>
              <div className="sub" style={{ marginTop: 8 }}>
                {res.data.history.length} version{res.data.history.length === 1 ? '' : 's'} · v{res.data.active.version} pushed{' '}
                {formatRelative(res.data.active.pushed_at, now)} by <span className="mono">{res.data.active.pushed_by}</span>
              </div>
            </>
          ) : (
            <div className="sub" style={{ marginTop: 8 }}>
              Push one from CI with a management key: <code>bindle passport push passport.json</code>.
            </div>
          )}
        </>
      )}
    </section>
  );
}
