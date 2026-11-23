import Link from 'next/link';
import { TraceIcon } from '@/components/Icons';
import { notFound } from 'next/navigation';
import { SecurityBadge, SecurityNotice } from '@/components/Security';
import { DriftBadge, DriftNotice } from '@/components/Drift';
import { ApiErrorPanel, KindChip, StatusBadge, settle } from '@/components/ui';
import { ApiError, api } from '@/lib/api';
import { formatCost, formatCount, formatDuration, formatTime } from '@/lib/format';
import { isStepId, isTraceId, one } from '@/lib/ids';
import { requireSession } from '@/lib/auth';
import { KIND_ORDER, stepKind } from '@/lib/steps';
import type { StepDetail, StepSummary } from '@/lib/types';
import { buildWaterfall, defaultStepId } from '@/lib/waterfall';
import { buildStory } from '@/lib/story';
import { bothEmpty, emptyReason } from '@/lib/content';
import { Payload } from '@/components/Payload';
import { TimeBreakdown } from '@/components/TimeBreakdown';
import { ContextChart } from '@/components/ContextChart';
import { contextProfile, fmtBytes } from '@/lib/context';
import { AddToDataset } from '@/app/evals/Forms';
import { groupLinks, linkSentence } from '@/lib/links';
import type { StoryLine } from '@/lib/story';
import { RunActions } from './RunActions';

export default async function TracePage({
  params,
  searchParams,
}: {
  params: Promise<{ id: string }>;
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const id = (await params).id.toLowerCase();
  await requireSession(isTraceId(id) ? `/traces/${id}` : '/runs');
  const res = await settle(api.trace(id));
  if (!res.ok) {
    if (res.error instanceof ApiError && (res.error.status === 404 || res.error.status === 400)) notFound();
    return (
      <>
        <TraceHead id={id} />
        <ApiErrorPanel error={res.error} />
      </>
    );
  }
  const { trace, steps, truncated } = res.data;
  const wf = buildWaterfall(steps);
  const story = buildStory(trace, steps);
  const ctx = contextProfile(steps);
  const [datasets, linkRes, stop, me] = await Promise.all([settle(api.datasets()), settle(api.traceLinks(id)), settle(api.runStop(id)), settle(api.me())]);
  const links = linkRes.ok ? linkRes.data.links : [];
  const linkGroups = groupLinks(links);
  const linkLine = linkSentence(links, trace.started_at);
  const lines: StoryLine[] = linkLine ? [{ tone: 'neutral', text: linkLine }, ...story.lines] : story.lines;
  const requested = one((await searchParams).step)?.toLowerCase();
  const selectedId =
    isStepId(requested) && steps.some((s) => s.step_id === requested) ? requested : (story.causeId ?? story.firstFailureId ?? defaultStepId(wf.rows));
  const detail = selectedId ? await settle(api.step(id, selectedId)) : undefined;
  const kinds = KIND_ORDER.filter((k) => steps.some((s) => s.type === k)).concat(
    [...new Set(steps.map((s) => s.type))].filter((t) => !KIND_ORDER.includes(t)),
  );

  return (
    <>
      <TraceHead id={id} />
      <section className="panel story" style={{ marginBottom: 16 }} aria-labelledby="story-h">
        <div className="story-head">
          <StatusBadge status={trace.status} />
          <h2 id="story-h">{story.headline}</h2>
          <span style={{ marginLeft: 'auto', display: 'inline-flex', gap: 6, alignItems: 'center', flexWrap: 'wrap' }}>
            <RunActions
              traceId={id}
              stoppedAt={stop.ok && stop.data.stopped ? stop.data.stopped_at : undefined}
              pollSeconds={stop.ok ? stop.data.poll_seconds : 60}
              canStop={me.ok && me.data.can_manage_keys}
            />
            <Link href={`/traces/compare?a=${id}`} className="btn btn-sm">
              Compare with…
            </Link>
            <AddToDataset traceId={id} datasets={datasets.ok ? datasets.data.datasets.map((d) => ({ id: d.id, name: d.name })) : []} />
          </span>
          {story.firstFailureId && story.firstFailureId !== selectedId ? (
            <Link href={`/traces/${id}?step=${story.firstFailureId}`} scroll={false} className="btn btn-sm" style={{ marginLeft: 'auto' }}>
              Jump to failure
            </Link>
          ) : null}
        </div>
        {lines.length ? (
          <ul className="story-lines">
            {lines.map((l, i) => (
              <li key={i} className={`t-${l.tone}`}>
                <span className="dot" aria-hidden="true">
                  {l.tone === 'critical' ? '✕' : l.tone === 'warning' ? '!' : '•'}
                </span>
                <span>
                  {l.text}
                  {l.stepId && l.stepId !== selectedId ? (
                    <Link href={`/traces/${id}?step=${l.stepId}`} scroll={false}>
                      Show step
                    </Link>
                  ) : null}
                </span>
              </li>
            ))}
          </ul>
        ) : null}
        {story.time.length > 1 ? (
          <div>
            <div className="sub">Where the time went</div>
            <TimeBreakdown time={story.time} />
          </div>
        ) : null}
        <div className="facts">
          <span>
            Agent <b>{trace.agent || 'Unnamed agent'}</b>
            {trace.agent_version ? ` · ${trace.agent_version}` : ''}
          </span>
          <span>
            Started <b>{formatTime(trace.started_at)}</b>
          </span>
          <span>
            Duration <b>{formatDuration(trace.duration_ms)}</b>
          </span>
          <span>
            Steps <b>{trace.steps}</b>
            {trace.failed_steps ? ` · ${trace.failed_steps} failed` : ''}
          </span>
          <span title={trace.cost_partial ? `${trace.cost_unknown_steps} model step(s) have no known price` : undefined}>
            Cost <b>{formatCost(trace.cost_usd, trace.cost_partial)}</b>
          </span>
          <span>
            Tokens <b>{formatCount(trace.input_tokens)}</b> in · <b>{formatCount(trace.output_tokens)}</b> out
          </span>
          {trace.environment ? (
            <span>
              Environment <b>{trace.environment}</b>
            </span>
          ) : null}
          {trace.framework ? (
            <span>
              Framework <b>{trace.framework}</b>
            </span>
          ) : null}
        </div>
      </section>

      {linkGroups.length ? (
        <section className="panel story" style={{ marginBottom: 16 }} aria-labelledby="links-h">
          <div className="story-head">
            <h2 id="links-h">Linked runs</h2>
          </div>
          <ul className="story-lines">
            {linkGroups.flatMap((g) =>
              g.links.map((l) => (
                <li key={`${g.label}-${l.trace_id}`} className="t-neutral">
                  <span className="dot" aria-hidden="true">
                    •
                  </span>
                  <span>
                    {g.label}{' '}
                    <Link href={`/traces/${l.trace_id}`}>
                      <b>{l.run?.agent || 'a run'}</b>
                    </Link>
                    {l.run ? ` · ${formatTime(l.run.started_at)} · ${l.run.status === 'failed' ? 'failed' : `${l.run.steps} steps`}` : ' · not stored (sampled out or past retention)'}
                    {l.step_name ? ` · via ${l.step_name}` : ''}
                  </span>
                </li>
              )),
            )}
          </ul>
        </section>
      ) : null}

      {ctx ? (
        <section className="panel story" style={{ marginBottom: 16 }} aria-labelledby="ctx-h">
          <div className="story-head">
            <h2 id="ctx-h">Context and cache</h2>
          </div>
          <div className="ctx-stats">
            <span>
              LLM calls <b>{formatCount(ctx.calls.length)}</b>
            </span>
            <span>
              Peak prompt <b>{formatCount(ctx.peak)}</b> tokens
            </span>
            <span>
              Total input <b>{formatCount(ctx.input)}</b>
            </span>
            <span>
              From cache <b>{Math.round(ctx.cacheRate * 100)}%</b>
            </span>
            {ctx.bigResults[0] ? (
              <span>
                Largest tool result{' '}
                <Link href={`/traces/${id}?step=${ctx.bigResults[0].stepId}`} scroll={false}>
                  <b>{ctx.bigResults[0].name}</b>
                </Link>{' '}
                ({fmtBytes(ctx.bigResults[0].bytes)})
              </span>
            ) : null}
          </div>
          <ContextChart calls={ctx.calls} peak={ctx.peak} traceId={id} />
          {ctx.hints.length ? (
            <ul className="story-lines">
              {ctx.hints.map((h, i) => (
                <li key={i} className="t-neutral">
                  <span className="dot" aria-hidden="true">
                    •
                  </span>
                  <span>{h}</span>
                </li>
              ))}
            </ul>
          ) : null}
        </section>
      ) : null}

      {truncated ? (
        <div className="notice notice-warn" style={{ marginBottom: 16 }} role="status">
          <span aria-hidden="true">⚠</span>
          <div>
            Showing the first {formatCount(steps.length)} of {formatCount(trace.steps)} steps.
          </div>
        </div>
      ) : null}

      <div className="trace-layout">
        <section className="panel wf" aria-label="Waterfall">
          <div className="legend">
            {kinds.map((k) => (
              <KindChip key={k} type={k} />
            ))}
          </div>
          <div className="wf-head" aria-hidden="true">
            <span>Step</span>
            <span className="wf-axis">
              <span>0</span>
              <span>{formatDuration(wf.spanMs / 2)}</span>
              <span>{formatDuration(wf.spanMs)}</span>
            </span>
            <span style={{ textAlign: 'right' }}>Duration</span>
          </div>
          <div role="list">
            {wf.rows.map((r) => {
              const k = stepKind(r.step.type);
              const failed = r.step.status === 'failure';
              const selected = r.step.step_id === selectedId;
              return (
                <Link
                  role="listitem"
                  key={r.step.step_id}
                  href={`/traces/${id}?step=${r.step.step_id}`}
                  scroll={false}
                  className={`wf-row${failed ? ' wf-failed' : ''}`}
                  aria-current={selected ? 'true' : undefined}
                  aria-label={`${k.label} step ${r.step.name}, ${failed ? 'failed, ' : ''}${r.step.security_flags?.length ? 'security signal, ' : ''}${formatDuration(r.step.duration_ms)}, starts at +${formatDuration(r.startOffsetMs)}`}
                >
                  <span className="wf-name" style={{ paddingLeft: r.depth * 14 }}>
                    <KindChip type={r.step.type} />
                    <span className="wf-name-text" title={r.step.name}>
                      {r.step.name}
                    </span>
                    {r.step.attempt > 0 ? <span className="badge">retry {r.step.attempt}</span> : null}
                    <SecurityBadge flags={r.step.security_flags} />
                    <DriftBadge drift={r.step.passport_drift} />
                    {failed ? (
                      <span className="wf-fail" aria-hidden="true">
                        ✕
                      </span>
                    ) : null}
                  </span>
                  <span className="wf-track" aria-hidden="true">
                    <span
                      className={`wf-bar${failed ? ' wf-bar-failed' : ''}`}
                      style={{ left: `${r.leftPct}%`, width: `${r.widthPct}%`, background: k.color }}
                    />
                  </span>
                  <span className="wf-dur">{formatDuration(r.step.duration_ms)}</span>
                </Link>
              );
            })}
          </div>
          {wf.rows.length === 0 ? <div className="empty">This trace has no steps.</div> : null}
        </section>

        <aside className="panel detail" aria-label="Step detail">
          {!detail ? (
            <div className="empty">Select a step.</div>
          ) : !detail.ok ? (
            <ApiErrorPanel error={detail.error} />
          ) : (
            <StepPanel step={detail.data.step} summary={steps.find((s) => s.step_id === selectedId)} />
          )}
        </aside>
      </div>
    </>
  );
}

function TraceHead({ id }: { id: string }) {
  return (
    <div className="page-head">
      <div>
        <div className="crumbs">
          <Link href="/runs">Runs</Link> / Run
        </div>
        <h1 className="title"><span className="title-icon" aria-hidden="true"><TraceIcon /></span>Run details</h1>
        <div className="sub mono" title="Trace ID">
          {id}
        </div>
      </div>
    </div>
  );
}

function StepPanel({ step, summary }: { step: StepDetail; summary?: StepSummary }) {
  const attrs = Object.entries(step.attributes ?? {}).sort(([a], [b]) => a.localeCompare(b));
  return (
    <>
      <div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap', marginBottom: 6 }}>
          <KindChip type={step.type} />
          <StatusBadge status={step.status} />
          {step.attempt > 0 ? <span className="badge">retry {step.attempt}</span> : null}
          <SecurityBadge flags={step.security_flags} />
          <DriftBadge drift={step.passport_drift} />
        </div>
        <h2 style={{ margin: 0, overflowWrap: 'anywhere' }}>{step.name}</h2>
        {step.type === 'llm_call' && step.input ? (
          <Link href={`/traces/${step.trace_id}/replay?step=${step.step_id}`} className="btn btn-sm" style={{ marginTop: 8 }}>
            Replay this call
          </Link>
        ) : null}
      </div>

      <SecurityNotice flags={step.security_flags} reasons={step.security_reasons} />
      <DriftNotice drift={step.passport_drift} />

      {step.redacted_kinds.length > 0 ? (
        <div className="notice" role="note">
          <span aria-hidden="true">🛡</span>
          <div>
            <strong>Sensitive data was redacted before storage.</strong> Matches for{' '}
            {step.redacted_kinds.map((k, i) => (
              <span key={k}>
                {i > 0 ? ', ' : ''}
                <code>{k}</code>
              </span>
            ))}{' '}
            were replaced with <code>[REDACTED:…]</code> markers. Bindle never stored the original values.
          </div>
        </div>
      ) : null}

      {step.error_message ? (
        <div className="notice notice-error" role="note">
          <span aria-hidden="true">✕</span>
          <div style={{ overflowWrap: 'anywhere' }}>
            <strong>Error:</strong> {step.error_message}
          </div>
        </div>
      ) : null}

      <dl className="kv">
        <dt>Duration</dt>
        <dd>{formatDuration(step.duration_ms)}</dd>
        <dt>Started</dt>
        <dd>{formatTime(step.start_time)}</dd>
        {step.model ? (
          <>
            <dt>Model</dt>
            <dd>
              {step.provider ? `${step.provider} / ` : ''}
              {step.model}
            </dd>
          </>
        ) : null}
        {step.tool_name ? (
          <>
            <dt>Tool</dt>
            <dd className="mono">{step.tool_name}</dd>
          </>
        ) : null}
        {step.input_tokens || step.output_tokens ? (
          <>
            <dt>Tokens</dt>
            <dd>
              {formatCount(step.input_tokens)} in · {formatCount(step.output_tokens)} out
              {step.cached_tokens ? ` · ${formatCount(step.cached_tokens)} cached` : ''}
            </dd>
          </>
        ) : null}
        {step.model || step.cost_usd > 0 ? (
          <>
            <dt>Cost</dt>
            <dd>{step.cost_known ? formatCost(step.cost_usd) : <span className="sub">Price unknown for this model</span>}</dd>
          </>
        ) : null}
        <dt>Step ID</dt>
        <dd className="mono">{step.step_id}</dd>
      </dl>

      {!step.input && !step.output && emptyReason(step, 'input').title.replace('input', 'output') === emptyReason(step, 'output').title ? (
        // Both empty for the same reason: say it once.
        <Payload label="Input and output" side="input" text="" bytes={0} empty={bothEmpty(emptyReason(step, 'input'))} />
      ) : (
        <>
          <Payload
            label="Input"
            side="input"
            text={step.input}
            bytes={summary?.input_bytes ?? step.input_bytes}
            truncated={step.truncated_fields?.includes('input')}
            empty={emptyReason(step, 'input')}
          />
          <Payload
            label="Output"
            side="output"
            text={step.output}
            bytes={summary?.output_bytes ?? step.output_bytes}
            truncated={step.truncated_fields?.includes('output')}
            empty={emptyReason(step, 'output')}
          />
        </>
      )}
      {step.reasoning ? (
        <Payload label="Reasoning" side="output" text={step.reasoning} bytes={step.reasoning_bytes} truncated={step.truncated_fields?.includes('reasoning')} />
      ) : null}

      {attrs.length > 0 ? (
        <details>
          <summary className="sub" style={{ cursor: 'pointer' }}>
            Attributes ({attrs.length})
          </summary>
          <table style={{ marginTop: 8 }}>
            <tbody>
              {attrs.map(([k, v]) => (
                <tr key={k}>
                  <td className="mono sub" style={{ whiteSpace: 'nowrap' }}>
                    {k}
                  </td>
                  <td className="mono" style={{ overflowWrap: 'anywhere' }}>
                    {v}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </details>
      ) : null}

      {step.links.length > 0 ? (
        <div>
          <div className="payload-label">Linked traces</div>
          {step.links.map((l) => (
            <div key={l} className="mono">
              {l}
            </div>
          ))}
        </div>
      ) : null}
    </>
  );
}
