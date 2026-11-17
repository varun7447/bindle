'use client';

import { useState } from 'react';
import { bucketLabel, niceTicks, type Bucket } from '@/lib/chart';
import { formatCount } from '@/lib/format';

const W = 720;
const H = 200;
const PAD = { top: 8, right: 8, bottom: 24, left: 36 };
const OK = '#3DD6B0';
const FAILED = '#F47067';
const GAP = 2; // surface gap between stacked segments
const RADIUS = 4;

/** Column path: square at the baseline, 4px rounded data-end on top. */
function columnPath(x: number, y: number, w: number, h: number, roundTop: boolean): string {
  if (h <= 0) return '';
  const r = roundTop ? Math.min(RADIUS, w / 2, h) : 0;
  return `M${x},${y + h}V${y + r}${r ? `Q${x},${y} ${x + r},${y}` : ''}H${x + w - r}${r ? `Q${x + w},${y} ${x + w},${y + r}` : ''}V${y + h}Z`;
}

export function RunsChart({ buckets, bucketSeconds }: { buckets: Bucket[]; bucketSeconds: number }) {
  const [hover, setHover] = useState<number | null>(null);
  if (buckets.length === 0) return <div className="empty">No data.</div>;

  const max = Math.max(...buckets.map((b) => b.runs));
  const ticks = niceTicks(max);
  const top = ticks.at(-1) ?? 1;
  const innerW = W - PAD.left - PAD.right;
  const innerH = H - PAD.top - PAD.bottom;
  const slot = innerW / buckets.length;
  const barW = Math.max(2, Math.min(24, slot * 0.7));
  const y = (v: number) => PAD.top + innerH - (v / top) * innerH;
  const labelEvery = Math.ceil(buckets.length / 6);
  const hovered = hover === null ? undefined : buckets[hover];

  return (
    <div className="chart">
      <div className="legend" aria-hidden="true">
        <span className="legend-item">
          <span className="legend-swatch" style={{ background: OK }} /> OK runs
        </span>
        <span className="legend-item">
          <span className="legend-swatch" style={{ background: FAILED }} /> ✕ Failed runs
        </span>
      </div>
      <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label="Runs per time bucket, split into OK and failed">
        {ticks.map((t) => (
          <g key={t}>
            <line x1={PAD.left} x2={W - PAD.right} y1={y(t)} y2={y(t)} stroke="var(--grid)" strokeWidth={1} />
            <text x={PAD.left - 8} y={y(t)} dy="0.32em" textAnchor="end" fontSize={11} fill="var(--muted)">
              {formatCount(t)}
            </text>
          </g>
        ))}
        {buckets.map((b, i) => {
          const x = PAD.left + i * slot + (slot - barW) / 2;
          const ok = b.runs - b.failed;
          const okH = (ok / top) * innerH;
          const failH = (b.failed / top) * innerH;
          const okY = PAD.top + innerH - okH;
          // Failed sits on top of OK, separated by the surface gap.
          const failY = okY - failH - (ok > 0 && b.failed > 0 ? GAP : 0);
          const label = `${bucketLabel(b.start, bucketSeconds)}: ${b.runs} runs, ${b.failed} failed`;
          return (
            <g
              key={b.start}
              className="col"
              tabIndex={0}
              aria-label={label}
              onMouseEnter={() => setHover(i)}
              onMouseLeave={() => setHover(null)}
              onFocus={() => setHover(i)}
              onBlur={() => setHover(null)}
            >
              {/* Hit target spans the whole slot, larger than the mark. */}
              <rect className="hit" x={PAD.left + i * slot} y={PAD.top} width={slot} height={innerH} fill="transparent" />
              <path d={columnPath(x, okY, barW, okH, b.failed === 0)} fill={OK} />
              <path d={columnPath(x, failY, barW, failH, true)} fill={FAILED} />
              {i % labelEvery === 0 ? (
                <text x={x + barW / 2} y={H - 6} textAnchor="middle" fontSize={11} fill="var(--muted)">
                  {bucketLabel(b.start, bucketSeconds)}
                </text>
              ) : null}
            </g>
          );
        })}
        <line x1={PAD.left} x2={W - PAD.right} y1={y(0)} y2={y(0)} stroke="var(--border)" strokeWidth={1} />
      </svg>
      {hovered && hover !== null ? (
        <div className="tooltip" style={{ left: `${((PAD.left + (hover + 0.5) * slot) / W) * 100}%`, top: `${(y(hovered.runs) / H) * 100}%` }}>
          <div className="tooltip-title">{bucketLabel(hovered.start, bucketSeconds)} UTC</div>
          <div className="tooltip-row">
            <strong>{formatCount(hovered.runs - hovered.failed)}</strong>
            <span className="tooltip-key" style={{ background: OK }} /> OK
          </div>
          <div className="tooltip-row">
            <strong>{formatCount(hovered.failed)}</strong>
            <span className="tooltip-key" style={{ background: FAILED }} /> Failed
          </div>
        </div>
      ) : null}
      <details className="table-view">
        <summary>Show as table</summary>
        <table>
          <thead>
            <tr>
              <th>Bucket (UTC)</th>
              <th className="num">Runs</th>
              <th className="num">Failed</th>
            </tr>
          </thead>
          <tbody>
            {buckets
              .filter((b) => b.runs > 0)
              .map((b) => (
                <tr key={b.start}>
                  <td>{bucketLabel(b.start, bucketSeconds)}</td>
                  <td className="num">{b.runs}</td>
                  <td className="num">{b.failed}</td>
                </tr>
              ))}
          </tbody>
        </table>
      </details>
    </div>
  );
}
