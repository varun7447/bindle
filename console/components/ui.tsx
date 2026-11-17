import Link from 'next/link';
import { ApiError } from '@/lib/api';
import { formatCost, formatDuration, formatRelative, formatTime } from '@/lib/format';
import { stepKind } from '@/lib/steps';
import type { Delta } from '@/lib/insights';
import type { Run } from '@/lib/types';

/** Status: icon + label, color is only reinforcement. */
export function StatusBadge({ status }: { status: string }) {
  if (status === 'failed' || status === 'failure')
    return (
      <span className="badge badge-failed">
        <span aria-hidden="true">✕</span> Failed
      </span>
    );
  if (status === 'ok' || status === 'success')
    return (
      <span className="badge badge-ok">
        <span aria-hidden="true">✓</span> OK
      </span>
    );
  return (
    <span className="badge">
      <span aria-hidden="true">?</span> {status === 'unset' ? 'Unset' : 'Unknown'}
    </span>
  );
}

export function KindChip({ type }: { type: string }) {
  const k = stepKind(type);
  return (
    <span className="kind">
      <span className="kind-swatch" style={{ background: k.color }} aria-hidden="true" />
      {k.label}
    </span>
  );
}

/** A "?" with a plain-language definition (hover or focus). */
export function Help({ children }: { children: React.ReactNode }) {
  return (
    <span className="help" tabIndex={0} role="note" aria-label="Definition">
      ?<span className="help-text">{children}</span>
    </span>
  );
}

export function DeltaText({ d, vs }: { d: Delta; vs: string }) {
  if (d.direction === 'flat') return <span className="delta delta-neutral">No change vs {vs}</span>;
  if (d.direction === 'new') return <span className="delta delta-neutral">No data for the {vs}</span>;
  const cls = d.good === undefined ? 'delta-neutral' : d.good ? 'delta-good' : 'delta-bad';
  return (
    <span className={`delta ${cls}`}>
      <span aria-hidden="true">{d.direction === 'up' ? '▲' : '▼'}</span>
      {d.text} <span style={{ fontWeight: 400 }}>vs {vs}</span>
    </span>
  );
}

export function Tile({ label, value, note, help, delta, vs, icon }: { label: string; value: string; note?: string; help?: string; delta?: Delta; vs?: string; icon?: React.ReactNode }) {
  return (
    <div className="panel tile">
      <div className="tile-label">
        {icon ? <span className="tile-icon" aria-hidden="true">{icon}</span> : null}
        {label}
        {help ? <Help>{help}</Help> : null}
      </div>
      <div className="tile-value">{value}</div>
      {delta && vs ? <DeltaText d={delta} vs={vs} /> : null}
      {note ? <div className="tile-note">{note}</div> : null}
    </div>
  );
}

const SEV_LABEL = { critical: '✕ Critical', warning: '⚠ Warning', info: 'ⓘ Info' } as const;

export function Severity({ level }: { level: 'critical' | 'warning' | 'info' }) {
  return <span className={`sev sev-${level}`}>{SEV_LABEL[level]}</span>;
}

/** Error states for API calls. 401 is the expected state until console auth exists. */
export function ApiErrorPanel({ error }: { error: unknown }) {
  const e = error instanceof ApiError ? error : new ApiError(500, 'internal', 'Something went wrong');
  if (e.status === 401)
    return (
      <div className="notice notice-warn" role="status">
        <span aria-hidden="true">⚠</span>
        <div>
          <strong>Not signed in.</strong> The Bindle API rejected the request. Console sign-in arrives with the
          control plane; for local development, set <code>BINDLE_API_DEV_AUTH_BYPASS_TENANT</code> for{' '}
          <code>bindle-api</code> (see <code>deploy/local/dev-env.sh</code>).
        </div>
      </div>
    );
  return (
    <div className="notice notice-error" role="alert">
      <span aria-hidden="true">✕</span>
      <div>
        <strong>{e.status === 0 ? 'Bindle API unreachable.' : 'Could not load data.'}</strong> {e.message}
      </div>
    </div>
  );
}

export function RunsTable({ runs, now }: { runs: Run[]; now: number }) {
  if (runs.length === 0) return <div className="empty">No runs match.</div>;
  return (
    <div style={{ overflowX: 'auto' }}>
      <table>
        <thead>
          <tr>
            <th>Status</th>
            <th>Agent</th>
            <th>Started</th>
            <th className="num">Duration</th>
            <th className="num">Steps</th>
            <th className="num">Cost</th>
            <th>Environment</th>
            <th>Trace</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => (
            <tr key={r.trace_id} className="row-link">
              <td>
                <StatusBadge status={r.status} />
              </td>
              <td className="truncate">
                <Link href={`/traces/${r.trace_id}`} className="row-anchor">
                  {r.agent || 'Unnamed agent'}
                </Link>
                {r.agent_version ? <span className="sub"> · {r.agent_version}</span> : null}
              </td>
              <td title={formatTime(r.started_at)}>{formatRelative(r.started_at, now)}</td>
              <td className="num">{formatDuration(r.duration_ms)}</td>
              <td className="num">
                {r.steps}
                {r.failed_steps ? <span style={{ color: 'var(--critical-text)' }}> · {r.failed_steps} failed</span> : null}
              </td>
              <td className="num" title={r.cost_partial ? `${r.cost_unknown_steps} model step(s) have no known price` : undefined}>
                {formatCost(r.cost_usd, r.cost_partial)}
              </td>
              <td>{r.environment || <span className="sub">—</span>}</td>
              <td>
                <Link href={`/traces/${r.trace_id}`} className="mono" tabIndex={-1}>
                  {r.trace_id.slice(0, 10)}…
                </Link>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** Awaits a promise, returning data or the error for inline rendering. */
export type Settled<T> = { ok: true; data: T } | { ok: false; error: unknown };

export async function settle<T>(p: Promise<T>): Promise<Settled<T>> {
  try {
    return { ok: true, data: await p };
  } catch (error) {
    return { ok: false, error };
  }
}
