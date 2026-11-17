import Link from 'next/link';
import { ApiError } from '@/lib/api';
import { formatCost, formatDuration, formatRelative, formatTime } from '@/lib/format';
import { stepKind } from '@/lib/steps';
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

export function Tile({ label, value, note }: { label: string; value: string; note?: string }) {
  return (
    <div className="panel">
      <div className="tile-label">{label}</div>
      <div className="tile-value">{value}</div>
      {note ? <div className="tile-note">{note}</div> : null}
    </div>
  );
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
          <code>bindle-api</code> (see <code>deploy/dev-env.sh</code>).
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
  if (runs.length === 0) return <div className="empty">No runs in this range.</div>;
  return (
    <div style={{ overflowX: 'auto' }}>
      <table>
        <thead>
          <tr>
            <th>Status</th>
            <th>Started</th>
            <th>Agent</th>
            <th>Environment</th>
            <th className="num">Steps</th>
            <th className="num">Failed</th>
            <th className="num">Duration</th>
            <th className="num">Cost</th>
            <th>Trace</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => (
            <tr key={r.trace_id}>
              <td>
                <StatusBadge status={r.status} />
              </td>
              <td title={formatTime(r.started_at)}>{formatRelative(r.started_at, now)}</td>
              <td className="truncate">
                {r.agent || <span className="sub">—</span>}
                {r.agent_version ? <span className="sub"> · {r.agent_version}</span> : null}
              </td>
              <td>{r.environment || <span className="sub">—</span>}</td>
              <td className="num">{r.steps}</td>
              <td className="num">{r.failed_steps}</td>
              <td className="num">{formatDuration(r.duration_ms)}</td>
              <td className="num" title={r.cost_partial ? `${r.cost_unknown_steps} model step(s) have no known price` : undefined}>
                {formatCost(r.cost_usd, r.cost_partial)}
              </td>
              <td>
                <Link href={`/traces/${r.trace_id}`} className="mono">
                  {r.trace_id.slice(0, 12)}…
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
