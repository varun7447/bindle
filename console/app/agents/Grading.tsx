import Link from 'next/link';
import { CheckCircleIcon } from '@/components/Icons';
import { ApiErrorPanel, Help } from '@/components/ui';
import type { Settled } from '@/components/ui';
import { formatCount, formatPercent } from '@/lib/format';
import { leadGrade, outcomeLabel, type GradingResponse, type OutcomeSettings } from '@/lib/grading';
import { problemHref } from '@/lib/outcomes';

// "Was it actually right?" on the Agents page (feature outcome_grading): what
// happened in the business after each version's runs, next to its evals.
export function Grading({
  agent,
  window,
  grading,
  settings,
}: {
  agent: string;
  window: '24h' | '7d' | '30d';
  grading: Settled<GradingResponse>;
  settings: Settled<OutcomeSettings>;
}) {
  const noReversals = settings.ok && settings.data.reversal_outcomes.length === 0;
  const grades = grading.ok ? grading.data.grades : [];
  const lead = leadGrade(grades);
  return (
    <section className="panel panel-flush" style={{ marginBottom: 16 }} aria-labelledby="grading-h">
      <div className="section-title">
        <h2 id="grading-h" className="h-icon">
          <CheckCircleIcon />
          Was it actually right?
        </h2>
        <Link href="/settings/outcomes">Outcome settings →</Link>
      </div>
      {!grading.ok ? (
        <div style={{ padding: 18 }}>
          <ApiErrorPanel error={grading.error} />
        </div>
      ) : grades.length === 0 ? (
        <div className="empty">No runs in this period.</div>
      ) : (
        <>
          {lead ? (
            <p style={{ padding: '4px 18px 0', margin: 0, fontWeight: 600 }}>
              {lead.headline}
            </p>
          ) : null}
          {noReversals ? (
            <p className="sub" style={{ padding: '6px 18px 0', margin: 0 }}>
              No outcome names are marked as reversals yet. Add them (for example <code>ticket_reopened</code>) in{' '}
              <Link href="/settings/outcomes">Settings → Outcomes</Link>.
            </p>
          ) : null}
          <div style={{ overflowX: 'auto' }}>
            <table style={{ marginTop: 8 }}>
              <thead>
                <tr>
                  <th style={{ paddingLeft: 18 }}>Version</th>
                  <th className="num">Runs</th>
                  <th className="num">
                    Passed evals <Help>Runs with at least one check or score result where none failed, out of runs that had results.</Help>
                  </th>
                  <th className="num">
                    Reversed <Help>Runs with a reversal outcome (such as a ticket reopened), reported by the agent or later by your business system.</Help>
                  </th>
                  <th className="num" style={{ paddingRight: 18 }}>
                    Outcome-graded success <Help>Runs where no step failed, no outcome failed and nothing was reversed, even if every eval passed.</Help>
                  </th>
                </tr>
              </thead>
              <tbody>
                {grades.map((g) => (
                  <tr key={g.version || '(none)'}>
                    <td style={{ paddingLeft: 18 }}>
                      <b>{g.version || 'unversioned'}</b>
                    </td>
                    <td className="num">{formatCount(g.runs)}</td>
                    <td className="num">{g.evaluated_runs ? formatPercent(g.passed_evals / g.evaluated_runs) : '–'}</td>
                    <td className="num">
                      {formatCount(g.reversed)}
                      {g.runs ? <div className="sub">{formatPercent(g.reversed / g.runs)}</div> : null}
                    </td>
                    <td className="num" style={{ paddingRight: 18 }}>
                      {formatPercent(g.success_rate)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {grades.some((g) => g.reversed_runs.length > 0) ? (
            <div style={{ padding: '12px 18px 14px' }}>
              <div className="section-title" style={{ padding: 0 }}>
                <h3 style={{ margin: 0, fontSize: 14 }}>Passed evals, then reversed</h3>
                <Link href={problemHref({ kind: 'reversed', agent, window })}>Copy to eval dataset →</Link>
              </div>
              <ul className="sub" style={{ margin: '6px 0 0', paddingLeft: 18, display: 'grid', gap: 4 }}>
                {grades.flatMap((g) =>
                  g.reversed_runs.slice(0, 10).map((r) => (
                    <li key={r.trace_id}>
                      <Link href={`/traces/${r.trace_id}`}>
                        <code>{r.trace_id.slice(0, 12)}…</code>
                      </Link>{' '}
                      {g.version || 'unversioned'}: {outcomeLabel(r.outcome)}
                    </li>
                  )),
                )}
              </ul>
            </div>
          ) : null}
          {grading.data.truncated ? (
            <p className="sub" style={{ padding: '0 18px 14px', margin: 0 }}>
              Based on the 5,000 most recent runs in this period.
            </p>
          ) : null}
        </>
      )}
    </section>
  );
}
