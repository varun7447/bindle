import Link from 'next/link';
import { CheckCircleIcon, LayersIcon } from '@/components/Icons';
import { ApiErrorPanel, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { describeScorer, overall } from '@/lib/evals';
import { formatCount, formatPercent, formatRelative } from '@/lib/format';
import { CheckButtons, NewCheckForm, NewDatasetForm } from './Forms';

export const dynamic = 'force-dynamic';

export default async function EvalsPage() {
  await requireSession('/evals');
  const [me, scorers, datasets, ins] = await Promise.all([settle(api.me()), settle(api.scorers()), settle(api.datasets()), settle(api.insights('30d'))]);
  const canEdit = me.ok && me.data.can_manage_keys;
  const agents = ins.ok ? ins.data.agents.map((a) => a.agent).filter(Boolean) : [];
  const now = Date.now();

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="title">
            <span className="title-icon" aria-hidden="true">
              <CheckCircleIcon />
            </span>
            Evals
          </h1>
          <div className="sub">Checks grade runs; datasets keep runs worth re-checking. Results per version are on the Agents page.</div>
        </div>
      </div>

      <section className="panel panel-flush" style={{ marginBottom: 16 }} aria-labelledby="checks-h">
        <div className="section-title">
          <h2 id="checks-h" className="h-icon">
            <CheckCircleIcon />
            Checks
          </h2>
          <a href="/console-api/scorers/export" className="btn btn-sm">
            Download checks
          </a>
        </div>
        {!scorers.ok ? (
          <div style={{ padding: 18 }}>
            <ApiErrorPanel error={scorers.error} />
          </div>
        ) : scorers.data.scorers.length === 0 ? (
          <div className="empty">No checks yet. Add one below, for example &ldquo;output contains the order ID&rdquo; or &ldquo;at most 15 steps&rdquo;.</div>
        ) : (
          <table style={{ marginTop: 8 }}>
            <thead>
              <tr>
                <th style={{ paddingLeft: 18 }}>Check</th>
                <th>Passes when</th>
                <th>State</th>
                <th aria-label="Actions" />
              </tr>
            </thead>
            <tbody>
              {scorers.data.scorers.map((sc) => (
                <tr key={sc.id}>
                  <td style={{ paddingLeft: 18 }}>
                    <b>{sc.name}</b>
                  </td>
                  <td>{describeScorer(sc)}</td>
                  <td>{sc.enabled ? <span className="badge badge-ok">On</span> : <span className="badge">Off</span>}</td>
                  <td style={{ paddingRight: 18, textAlign: 'right' }}>
                    <CheckButtons id={sc.id} enabled={sc.enabled} canEdit={canEdit} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {canEdit ? (
          <div style={{ padding: '12px 18px 16px' }}>
            <NewCheckForm agents={agents} />
          </div>
        ) : (
          <p className="sub" style={{ padding: '8px 18px 14px', margin: 0 }}>
            Only owners and admins can change checks.
          </p>
        )}
        <p className="sub" style={{ padding: '0 18px 14px', margin: 0 }}>
          Model-graded scores (an LLM judge with your rubric) run in your own environment: export a dataset, grade each line, and send the result with{' '}
          <code>bindle.score(&quot;helpful&quot;, 0.8, trace_id=line[&quot;trace_id&quot;])</code>. Bindle never holds your model keys.
        </p>
        <p className="sub" style={{ padding: '0 18px 14px', margin: 0 }}>
          Before a release: <code>bindle eval --dataset ds.jsonl --checks bindle-checks.json --agent my_app:answer --max-drop 0.05</code> runs your agent on a
          dataset in CI and fails the build if the pass rate drops (example workflow in <code>examples/ci/</code>).
        </p>
      </section>

      <section className="panel panel-flush" aria-labelledby="datasets-h">
        <div className="section-title">
          <h2 id="datasets-h" className="h-icon">
            <LayersIcon />
            Datasets
          </h2>
          <span className="sub">Copies of runs, kept after trace retention</span>
        </div>
        {!datasets.ok ? (
          <div style={{ padding: 18 }}>
            <ApiErrorPanel error={datasets.error} />
          </div>
        ) : datasets.data.datasets.length === 0 ? (
          <div className="empty">No datasets yet.</div>
        ) : (
          <table style={{ marginTop: 8 }}>
            <thead>
              <tr>
                <th style={{ paddingLeft: 18 }}>Dataset</th>
                <th className="num">Runs</th>
                <th className="num">Last result</th>
                <th style={{ paddingRight: 18 }}>Last checked</th>
              </tr>
            </thead>
            <tbody>
              {datasets.data.datasets.map((d) => {
                const rate = d.last_eval ? overall(d.last_eval.tallies) : null;
                return (
                  <tr key={d.id}>
                    <td style={{ paddingLeft: 18 }}>
                      <Link href={`/evals/datasets/${d.id}`}>
                        <b>{d.name}</b>
                      </Link>
                      {d.description ? <div className="sub">{d.description}</div> : null}
                    </td>
                    <td className="num">{formatCount(d.items)}</td>
                    <td className="num">{rate === null ? '–' : `${formatPercent(rate)} passed`}</td>
                    <td style={{ paddingRight: 18 }}>{d.last_eval ? formatRelative(d.last_eval.created_at, now) : <span className="sub">never</span>}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
        <div style={{ padding: '12px 18px 16px' }}>
          <NewDatasetForm />
        </div>
      </section>
    </>
  );
}
