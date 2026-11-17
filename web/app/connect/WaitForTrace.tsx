'use client';

import Link from 'next/link';
import { useEffect, useRef, useState } from 'react';
import type { Run } from '@/lib/types';

type State = { kind: 'waiting' } | { kind: 'received'; run: Run } | { kind: 'unauthenticated' } | { kind: 'error' };

const POLL_MS = 3000;

export function WaitForTrace() {
  // Look back a little so a trace sent while the page was loading still counts.
  const since = useRef(new Date(Date.now() - 60_000).toISOString());
  const [state, setState] = useState<State>({ kind: 'waiting' });

  useEffect(() => {
    if (state.kind === 'received' || state.kind === 'unauthenticated') return;
    let stopped = false;
    const tick = async () => {
      try {
        const res = await fetch(`/console-api/latest-run?since=${encodeURIComponent(since.current)}`, { cache: 'no-store' });
        if (stopped) return;
        if (res.status === 401) return setState({ kind: 'unauthenticated' });
        if (!res.ok) return setState({ kind: 'error' });
        const body = (await res.json()) as { run: Run | null };
        if (body.run) setState({ kind: 'received', run: body.run });
        else setState({ kind: 'waiting' });
      } catch {
        if (!stopped) setState({ kind: 'error' });
      }
    };
    void tick();
    const t = setInterval(tick, POLL_MS);
    return () => {
      stopped = true;
      clearInterval(t);
    };
  }, [state.kind]);

  if (state.kind === 'received')
    return (
      <div className="notice" role="status" style={{ borderColor: 'rgba(61,214,176,0.45)' }}>
        <span aria-hidden="true" style={{ color: 'var(--good)' }}>
          ✓
        </span>
        <div>
          <strong>First trace received</strong> from {state.run.agent || 'your agent'} · {state.run.steps} steps.{' '}
          <Link href={`/traces/${state.run.trace_id}`}>Open the trace →</Link>
        </div>
      </div>
    );
  if (state.kind === 'unauthenticated')
    return (
      <div className="notice notice-warn" role="status">
        <span aria-hidden="true">⚠</span>
        <div>
          <strong>Not signed in.</strong> Can't check for traces until the console is signed in to the Bindle API.
        </div>
      </div>
    );
  return (
    <div className="waiting" role="status" aria-live="polite">
      <span className="pulse" aria-hidden="true" />
      <div>
        <strong>Waiting for your first trace…</strong>
        <div className="sub">
          {state.kind === 'error' ? "Can't reach the Bindle API right now; still retrying." : 'Run your agent; this updates automatically.'}
        </div>
      </div>
    </div>
  );
}
