import Link from 'next/link';
import { notFound } from 'next/navigation';
import { ApiErrorPanel, KindChip, StatusBadge, settle } from '@/components/ui';
import { ApiError, api } from '@/lib/api';
import { formatBytes, formatCost, formatCount, formatDuration, formatTime, prettyPayload } from '@/lib/format';
import { isStepId, isTraceId, one } from '@/lib/ids';
import { requireSession } from '@/lib/auth';
import { KIND_ORDER, stepKind } from '@/lib/steps';
import type { StepDetail, StepSummary } from '@/lib/types';
import { buildWaterfall, defaultStepId } from '@/lib/waterfall';

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
  const requested = one((await searchParams).step)?.toLowerCase();
  const selectedId = isStepId(requested) && steps.some((s) => s.step_id === requested) ? requested : defaultStepId(wf.rows);
  const detail = selectedId ? await settle(api.step(id, selectedId)) : undefined;
  const kinds = KIND_ORDER.filter((k) => steps.some((s) => s.type === k)).concat(
    [...new Set(steps.map((s) => s.type))].filter((t) => !KIND_ORDER.includes(t)),
  );

  return (
    <>
      <TraceHead id={id} />
      <section className="panel" style={{ marginBottom: 16 }} aria-label="Run summary">
        <dl className="kv" style={{ gridTemplateColumns: 'repeat(4, max-content 1fr)' }}>
          <dt>Status</dt>
          <dd>
            <StatusBadge status={trace.status} />
          </dd>
          <dt>Agent</dt>
          <dd>
            {trace.agent || '—'}
            {trace.agent_version ? <span className="sub"> · {trace.agent_version}</span> : null}
          </dd>
          <dt>Started</dt>
          <dd>{formatTime(trace.started_at)}</dd>
          <dt>Duration</dt>
          <dd>{formatDuration(trace.duration_ms)}</dd>
          <dt>Steps</dt>
          <dd>
            {trace.steps} <span className="sub">({trace.failed_steps} failed)</span>
          </dd>
          <dt>Cost</dt>
          <dd title={trace.cost_partial ? `${trace.cost_unknown_steps} model step(s) have no known price` : undefined}>
            {formatCost(trace.cost_usd, trace.cost_partial)}
          </dd>
          <dt>Tokens</dt>
          <dd>
            {formatCount(trace.input_tokens)} in · {formatCount(trace.output_tokens)} out
          </dd>
          <dt>Environment</dt>
          <dd>
            {trace.environment || '—'}
            {trace.framework ? <span className="sub"> · {trace.framework}</span> : null}
          </dd>
        </dl>
      </section>

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
                  className="wf-row"
                  aria-current={selected ? 'true' : undefined}
                  aria-label={`${k.label} step ${r.step.name}, ${failed ? 'failed, ' : ''}${formatDuration(r.step.duration_ms)}, starts at +${formatDuration(r.startOffsetMs)}`}
                >
                  <span className="wf-name" style={{ paddingLeft: r.depth * 14 }}>
                    <KindChip type={r.step.type} />
                    <span className="wf-name-text" title={r.step.name}>
                      {r.step.name}
                    </span>
                    {r.step.attempt > 0 ? <span className="badge">retry {r.step.attempt}</span> : null}
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
        <div className="sub">
          <Link href="/runs">Runs</Link> / Trace
        </div>
        <h1 className="mono" style={{ fontSize: 16 }}>
          {id}
        </h1>
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
        </div>
        <h2 style={{ margin: 0, overflowWrap: 'anywhere' }}>{step.name}</h2>
      </div>

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

      <Payload label="Input" text={step.input} bytes={summary?.input_bytes ?? step.input_bytes} />
      <Payload label="Output" text={step.output} bytes={summary?.output_bytes ?? step.output_bytes} />
      {step.reasoning ? <Payload label="Reasoning" text={step.reasoning} bytes={step.reasoning_bytes} /> : null}

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

function Payload({ label, text, bytes }: { label: string; text: string; bytes: number }) {
  return (
    <div>
      <div className="payload-label">
        <span>{label}</span>
        <span>{bytes ? formatBytes(bytes) : ''}</span>
      </div>
      {text ? (
        <pre className="payload">{prettyPayload(text)}</pre>
      ) : (
        <pre className="payload empty-payload">Not captured</pre>
      )}
    </div>
  );
}
