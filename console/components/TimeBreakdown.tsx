'use client';

import { useState } from 'react';
import { formatDuration, formatPercent } from '@/lib/format';
import { stepKind } from '@/lib/steps';
import type { KindTime } from '@/lib/story';

/** "Where the time went": one stacked bar by step type, legend always shown. */
export function TimeBreakdown({ time }: { time: KindTime[] }) {
  const [hover, setHover] = useState<string | null>(null);
  if (time.length === 0) return null;
  const h = time.find((t) => t.type === hover);
  return (
    <div className="chart">
      <div className="timebar" role="img" aria-label={time.map((t) => `${stepKind(t.type).label} ${formatPercent(t.share)}`).join(', ')}>
        {time.map((t) => (
          <span
            key={t.type}
            style={{ width: `${Math.max(t.share * 100, 0.6)}%`, background: stepKind(t.type).color, opacity: hover && hover !== t.type ? 0.45 : 1 }}
            onMouseEnter={() => setHover(t.type)}
            onMouseLeave={() => setHover(null)}
          />
        ))}
      </div>
      <div className="time-legend">
        {time.map((t) => (
          <span key={t.type} className="legend-item" onMouseEnter={() => setHover(t.type)} onMouseLeave={() => setHover(null)}>
            <span className="legend-swatch" style={{ background: stepKind(t.type).color }} aria-hidden="true" />
            {stepKind(t.type).label} <b>{formatPercent(t.share)}</b>
          </span>
        ))}
      </div>
      {h ? (
        <div className="sub" style={{ marginTop: 6 }} aria-live="polite">
          {stepKind(h.type).label}: {formatDuration(h.ms)} across {h.steps} step{h.steps === 1 ? '' : 's'}
        </div>
      ) : null}
    </div>
  );
}
