import Link from 'next/link';
import { LayersIcon } from '@/components/Icons';
import { ApiErrorPanel, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { formatCount } from '@/lib/format';
import { parseProblemSource, problemParams, problemTitle } from '@/lib/outcomes';
import { FromProblemsForm } from '../Forms';

export const dynamic = 'force-dynamic';

const KIND_NAME = { failure: 'failures', negative_feedback: 'thumbs down', failed_score: 'failed scores', failed_runs: 'failed runs', reversed: 'reversals' } as const;

export default async function FromProblemsPage({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const sp = await searchParams;
  const src = parseProblemSource(sp);
  await requireSession(src ? `/evals/from-problems?${new URLSearchParams(problemParams(src))}` : '/evals');

  const head = (
    <div className="page-head">
      <div>
        <div className="sub">
          <Link href="/evals">Evals</Link> / From production
        </div>
        <h1 className="title">
          <span className="title-icon" aria-hidden="true">
            <LayersIcon />
          </span>
          Turn problem runs into eval cases
        </h1>
        <div className="sub">Copy the runs into a dataset (copies outlive trace retention) and keep checks that would have caught them.</div>
      </div>
    </div>
  );
  if (!src) {
    return (
      <>
        {head}
        <section className="panel">
          <div className="empty">
            Nothing to add: open this from a failure on <Link href="/">Home</Link>, from <Link href="/sessions">Sessions</Link>, or from an agent on the{' '}
            <Link href="/agents">Agents</Link> page.
          </div>
        </section>
      </>
    );
  }
  const [sugg, datasets] = await Promise.all([settle(api.problemSuggestions(src)), settle(api.datasets())]);
  const title = problemTitle(src);
  return (
    <>
      {head}
      <section className="panel stack">
        <h2 style={{ margin: 0, fontSize: 15 }}>{title}</h2>
        {!sugg.ok ? (
          <ApiErrorPanel error={sugg.error} />
        ) : sugg.data.matched === 0 ? (
          <div className="empty">No matching runs in the last {src.window ?? '7d'}.</div>
        ) : !datasets.ok ? (
          <ApiErrorPanel error={datasets.error} />
        ) : (
          <>
            <div className="sub">
              {formatCount(sugg.data.matched)} recent run{sugg.data.matched === 1 ? '' : 's'} (up to 50, newest first) in the last {src.window ?? '7d'}
              {sugg.data.agent ? ` · ${sugg.data.agent}` : ''}. Examples:{' '}
              {sugg.data.trace_ids.slice(0, 5).map((t, i) => (
                <span key={t}>
                  {i ? ', ' : ''}
                  <Link href={`/traces/${t}`} className="mono">
                    {t.slice(0, 8)}
                  </Link>
                </span>
              ))}
            </div>
            <FromProblemsForm
              source={problemParams(src)}
              datasets={datasets.data.datasets.map((d) => ({ id: d.id, name: d.name }))}
              suggestions={sugg.data.suggestions}
              defaultName={`${src.agent || sugg.data.agent || 'production'} ${KIND_NAME[src.kind]}`.slice(0, 80)}
            />
          </>
        )}
      </section>
    </>
  );
}
