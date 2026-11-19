import Link from 'next/link';
import { TraceIcon } from '@/components/Icons';
import { ApiErrorPanel, KindChip, StatusBadge, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { formatCost, formatDuration, formatRelative } from '@/lib/format';
import { isTraceId, one } from '@/lib/ids';
import { argsDiffer, compareRuns, type AlignedStep } from '@/lib/trajectory';
import type { Run, StepSummary } from '@/lib/types';

export const dynamic = 'force-dynamic';

/** Tool calls on both paths whose arguments are fetched and compared. */
const MAX_ARG_PAIRS = 20;

export default async function ComparePage({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const sp = await searchParams;
  const a = one(sp.a)?.toLowerCase().trim();
  const b = one(sp.b)?.toLowerCase().trim();
  const here = `/traces/compare${a ? `?a=${a}${b ? `&b=${b}` : ''}` : ''}`;
  await requireSession(isTraceId(a) && (!b || isTraceId(b)) ? here : '/traces/compare');

  if (!isTraceId(a) || !isTraceId(b)) return <Pick a={isTraceId(a) ? a : undefined} bad={(a !== undefined && !isTraceId(a)) || (b !== undefined && b !== '' && !isTraceId(b))} />;

  const [ra, rb] = await Promise.all([settle(api.trace(a)), settle(api.trace(b))]);
  if (!ra.ok || !rb.ok) {
    return (
      <>
        <Head />
        <ApiErrorPanel error={!ra.ok ? ra.error : !rb.ok ? rb.error : undefined} />
      </>
    );
  }
  const t = compareRuns(ra.data.steps, rb.data.steps);

  // Compare the arguments of tool calls both runs made.
  const toolPairs = t.rows.filter((r) => r.kind === 'same' && r.a!.type === 'tool_call').slice(0, MAX_ARG_PAIRS);
  const details = await Promise.all(
    toolPairs.map(async (r) => {
      const [da, db] = await Promise.all([settle(api.step(a, r.a!.step_id)), settle(api.step(b, r.b!.step_id))]);
      return { r, ai: da.ok ? da.data.step.input : '', bi: db.ok ? db.data.step.input : '' };
    }),
  );
  for (const { r, ai, bi } of details) {
    if (ai && bi && argsDiffer(ai, bi)) r.changes.push({ field: 'arguments', text: 'different arguments' });
  }
  const changed = t.rows.filter((r) => r.kind !== 'same' || r.changes.length).length;

  return (
    <>
      <Head />
      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16, marginBottom: 16 }}>
        <RunCard side="A" run={ra.data.trace} />
        <RunCard side="B" run={rb.data.trace} />
      </div>

      <section className="panel story" style={{ marginBottom: 16 }} aria-labelledby="cmp-h">
        <div className="story-head">
          <h2 id="cmp-h">How the paths differ</h2>
          <Link href={`/traces/compare?a=${b}&b=${a}`} className="btn btn-sm" style={{ marginLeft: 'auto' }}>
            Swap A and B
          </Link>
        </div>
        <ul className="story-lines">
          {t.summary.map((s, i) => (
            <li key={i} className={i === 0 && t.divergeAt !== -1 ? 't-warning' : 't-neutral'}>
              <span className="dot" aria-hidden="true">
                {i === 0 && t.divergeAt !== -1 ? '!' : '•'}
              </span>
              <span>{s}</span>
            </li>
          ))}
        </ul>
        {ra.data.truncated || rb.data.truncated ? <div className="sub">Only the first steps of a long run are compared.</div> : null}
      </section>

      <section className="panel panel-flush" aria-labelledby="steps-h">
        <div className="section-title">
          <h2 id="steps-h">Steps side by side</h2>
          <span className="sub">
            {t.same} in common · {t.onlyA} only in A · {t.onlyB} only in B · {changed} differ
          </span>
        </div>
        <div style={{ overflowX: 'auto' }}>
          <table style={{ marginTop: 8 }}>
            <thead>
              <tr>
                <th style={{ paddingLeft: 18, width: 40 }}>#</th>
                <th>Run A</th>
                <th>Run B</th>
                <th style={{ paddingRight: 18 }}>Difference</th>
              </tr>
            </thead>
            <tbody>
              {t.rows.map((r, i) => (
                <Row key={i} r={r} i={i} diverge={i === t.divergeAt} a={a} b={b} />
              ))}
            </tbody>
          </table>
          {t.rows.length === 0 ? <div className="empty">Neither run has steps.</div> : null}
        </div>
        <p className="sub" style={{ padding: '8px 18px 14px', margin: 0 }}>
          Steps are matched by type and the tool they call (LLM calls match each other, so a model change shows as a difference). Agent wrapper steps are left out.
          Arguments are compared for the first {MAX_ARG_PAIRS} matched tool calls, when content capture is on.
        </p>
      </section>
    </>
  );
}

function Row({ r, i, diverge, a, b }: { r: AlignedStep; i: number; diverge: boolean; a: string; b: string }) {
  const tone = r.kind === 'same' ? (r.changes.length ? 'cmp-changed' : '') : r.kind === 'only_a' ? 'cmp-only-a' : 'cmp-only-b';
  return (
    <tr className={tone} style={diverge ? { boxShadow: 'inset 3px 0 0 var(--warning, #b45309)' } : undefined}>
      <td className="num sub" style={{ paddingLeft: 18 }}>
        {i + 1}
      </td>
      <td>{r.a ? <Cell s={r.a} trace={a} /> : <span className="sub">—</span>}</td>
      <td>{r.b ? <Cell s={r.b} trace={b} /> : <span className="sub">—</span>}</td>
      <td style={{ paddingRight: 18 }}>
        {diverge ? <div className="badge badge-warn">paths diverge here</div> : null}
        {r.kind === 'only_a' ? <span className="sub">Only in A</span> : r.kind === 'only_b' ? <span className="sub">Only in B</span> : null}
        {r.changes.map((c) => (
          <div key={c.field} style={{ fontSize: 12.5 }}>
            <b>{c.field}</b> {c.text}
          </div>
        ))}
      </td>
    </tr>
  );
}

function Cell({ s, trace }: { s: StepSummary; trace: string }) {
  return (
    <Link href={`/traces/${trace}?step=${s.step_id}`} style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
      <KindChip type={s.type} />
      <span className="truncate" title={s.name}>
        {s.tool_name || s.name}
      </span>
      {s.status === 'failure' ? <span className="wf-fail" aria-label="failed">✕</span> : null}
      <span className="sub" style={{ marginLeft: 'auto' }}>
        {formatDuration(s.duration_ms)}
      </span>
    </Link>
  );
}

function RunCard({ side, run }: { side: 'A' | 'B'; run: Run }) {
  return (
    <section className="panel" aria-label={`Run ${side}`}>
      <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
        <span className="badge">{side}</span>
        <StatusBadge status={run.status} />
        <Link href={`/traces/${run.trace_id}`} className="mono truncate">
          {run.trace_id}
        </Link>
      </div>
      <div className="facts" style={{ marginTop: 8 }}>
        <span>
          Agent <b>{run.agent || 'Unnamed agent'}</b>
          {run.agent_version ? ` · ${run.agent_version}` : ''}
        </span>
        <span>
          Duration <b>{formatDuration(run.duration_ms)}</b>
        </span>
        <span>
          Steps <b>{run.steps}</b>
          {run.failed_steps ? ` · ${run.failed_steps} failed` : ''}
        </span>
        <span>
          Cost <b>{formatCost(run.cost_usd, run.cost_partial)}</b>
        </span>
      </div>
    </section>
  );
}

function Head() {
  return (
    <div className="page-head">
      <div>
        <div className="crumbs">
          <Link href="/runs">Runs</Link> / Compare
        </div>
        <h1 className="title">
          <span className="title-icon" aria-hidden="true">
            <TraceIcon />
          </span>
          Compare runs
        </h1>
        <div className="sub">Where two runs took different paths, and what changed in the steps they share</div>
      </div>
    </div>
  );
}

/** Pick the second run: paste a trace ID, or choose a recent run of the same agent. */
async function Pick({ a, bad }: { a?: string; bad: boolean }) {
  const base = a ? await settle(api.trace(a)) : undefined;
  const agent = base?.ok ? base.data.trace.agent : undefined;
  const recent = a ? await settle(api.runs({ ...(agent ? { agent } : {}), limit: '15' })) : undefined;
  const now = Date.now();
  const runs = recent?.ok ? recent.data.runs.filter((r) => r.trace_id !== a) : [];
  return (
    <>
      <Head />
      {bad ? (
        <div className="notice notice-error" role="alert" style={{ marginBottom: 16 }}>
          <span aria-hidden="true">✕</span>
          <div>A trace ID is 32 hexadecimal characters.</div>
        </div>
      ) : null}
      <section className="panel" style={{ marginBottom: 16 }}>
        <form className="filters" method="get" action="/traces/compare">
          <input name="a" defaultValue={a ?? ''} placeholder="Run A trace ID" aria-label="Run A trace ID" maxLength={32} style={{ width: 300 }} className="mono" />
          <input name="b" placeholder="Run B trace ID" aria-label="Run B trace ID" maxLength={32} style={{ width: 300 }} className="mono" autoFocus={!!a} />
          <button type="submit" className="btn btn-primary">
            Compare
          </button>
        </form>
      </section>
      {a ? (
        <section className="panel panel-flush" aria-labelledby="recent-h">
          <div className="section-title">
            <h2 id="recent-h">Or pick a recent run{agent ? ` of ${agent}` : ''}</h2>
          </div>
          {recent && !recent.ok ? (
            <div style={{ padding: 18 }}>
              <ApiErrorPanel error={recent.error} />
            </div>
          ) : runs.length === 0 ? (
            <div className="empty">No other recent runs.</div>
          ) : (
            <table>
              <tbody>
                {runs.map((r) => (
                  <tr key={r.trace_id}>
                    <td style={{ paddingLeft: 18 }}>
                      <StatusBadge status={r.status} />
                    </td>
                    <td>
                      <Link href={`/traces/compare?a=${a}&b=${r.trace_id}`}>
                        <b>{r.agent || 'Unnamed agent'}</b>
                        {r.agent_version ? ` · ${r.agent_version}` : ''}
                      </Link>
                      <div className="sub mono">{r.trace_id}</div>
                    </td>
                    <td className="num">{r.steps} steps</td>
                    <td className="num">{formatDuration(r.duration_ms)}</td>
                    <td className="num" style={{ paddingRight: 18 }}>
                      {formatRelative(r.started_at, now)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </section>
      ) : null}
    </>
  );
}
