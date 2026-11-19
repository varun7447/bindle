import Link from 'next/link';
import { notFound } from 'next/navigation';
import { TraceIcon } from '@/components/Icons';
import { ApiErrorPanel, settle } from '@/components/ui';
import { ApiError, api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { isStepId, isTraceId, one } from '@/lib/ids';
import { defaultModel, defaultProvider, toReplayMessages } from '@/lib/replay';
import { ReplayForm } from './ReplayForm';

export const dynamic = 'force-dynamic';

export default async function ReplayPage({ params, searchParams }: { params: Promise<{ id: string }>; searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const id = (await params).id.toLowerCase();
  const stepId = (one((await searchParams).step) ?? '').toLowerCase();
  await requireSession(isTraceId(id) ? `/traces/${id}` : '/runs');
  if (!isTraceId(id) || !isStepId(stepId)) notFound();
  const res = await settle(api.step(id, stepId));
  if (!res.ok && res.error instanceof ApiError && res.error.status === 404) notFound();
  return (
    <>
      <div className="page-head">
        <div>
          <div className="sub">
            <Link href="/runs">Runs</Link> / <Link href={`/traces/${id}?step=${stepId}`}>Run</Link> / Replay
          </div>
          <h1 className="title">
            <span className="title-icon" aria-hidden="true">
              <TraceIcon />
            </span>
            Replay an LLM call
          </h1>
          <div className="sub">Change the prompt or the model, run it again, and compare with what happened.</div>
        </div>
      </div>
      {!res.ok ? (
        <ApiErrorPanel error={res.error} />
      ) : res.data.step.type !== 'llm_call' ? (
        <div className="notice" role="note">
          <span aria-hidden="true">ⓘ</span>
          <div>Only LLM calls can be replayed.</div>
        </div>
      ) : !res.data.step.input ? (
        <div className="notice" role="note">
          <span aria-hidden="true">ⓘ</span>
          <div>This call&apos;s prompt wasn&apos;t captured (content capture was off), so there&apos;s nothing to replay.</div>
        </div>
      ) : (
        <ReplayForm
          traceId={id}
          stepId={stepId}
          messages={toReplayMessages(res.data.step.input)}
          provider={defaultProvider(res.data.step.provider, res.data.step.model)}
          model={defaultModel(res.data.step.model, defaultProvider(res.data.step.provider, res.data.step.model))}
          originalOutput={res.data.step.output}
        />
      )}
    </>
  );
}
