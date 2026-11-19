'use client';

import { useActionState } from 'react';
import { formatTime } from '@/lib/format';
import { stopRunAction, type StopState } from './actions';

/**
 * Stop run and Download for replay, on the run page. A stop reaches the
 * agent at its next bindle.check_limits() call after the SDK refreshes its
 * settings (about once a minute).
 */
export function RunActions({
  traceId,
  stoppedAt,
  pollSeconds,
  canStop,
}: {
  traceId: string;
  stoppedAt?: string;
  pollSeconds: number;
  canStop: boolean;
}) {
  const [state, action, pending] = useActionState<StopState, FormData>(stopRunAction, { kind: 'idle' });
  const at = state.kind === 'stopped' ? (state.at ?? stoppedAt) : stoppedAt;
  const stopped = state.kind === 'stopped' || Boolean(stoppedAt);
  const wait = Math.max(1, Math.round((state.kind === 'stopped' ? state.pollSeconds : pollSeconds) / 60));
  return (
    <span style={{ display: 'inline-flex', gap: 6, alignItems: 'center', flexWrap: 'wrap' }}>
      <a
        className="btn btn-sm"
        href={`/console-api/traces/${traceId}/cassette`}
        download={`run-${traceId}.cassette.json`}
        title="Download this run's input and every tool call's arguments and result, to re-run your agent with bindle.Cassette"
      >
        Download for replay
      </a>
      {stopped ? (
        <span className="badge badge-warn" role="status" title={at ? `Stopped ${formatTime(at)}` : undefined}>
          Stop requested
        </span>
      ) : canStop ? (
        <form action={action} style={{ display: 'inline' }}>
          <input type="hidden" name="trace_id" value={traceId} />
          <button
            type="submit"
            className="btn btn-sm"
            disabled={pending}
            title={`The agent stops at its next bindle.check_limits() call, within about ${wait} minute${wait === 1 ? '' : 's'}`}
          >
            {pending ? 'Stopping…' : 'Stop run'}
          </button>
        </form>
      ) : null}
      {stopped ? (
        <span className="sub">
          The agent stops at its next <code>check_limits()</code> call, within about {wait} minute{wait === 1 ? '' : 's'}.
        </span>
      ) : null}
      {state.kind === 'error' ? (
        <span className="sub" role="alert">
          {state.message}
        </span>
      ) : null}
    </span>
  );
}
