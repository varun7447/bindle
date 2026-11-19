'use client';

import Link from 'next/link';
import { useState } from 'react';
import { formatCount, formatPercent } from '@/lib/format';
import type { ContextCall } from '@/lib/context';

const CACHED = 'var(--k-retrieval)';
const FRESH = 'var(--k-llm)';
const HEIGHT = 120;

/** One column per LLM call: prompt size, with the cached part at the base. */
export function ContextChart({ calls, peak, traceId }: { calls: ContextCall[]; peak: number; traceId: string }) {
  const [hover, setHover] = useState<number | null>(null);
  const h = hover === null ? undefined : calls[hover];
  return (
    <div className="chart">
      <div
        className="ctx-bars"
        style={{ height: HEIGHT }}
        role="img"
        aria-label={`Prompt size per LLM call, peak ${formatCount(peak)} tokens`}
        onMouseLeave={() => setHover(null)}
      >
        {calls.map((c, i) => {
          const total = (c.input / peak) * HEIGHT;
          const cached = (c.cached / peak) * HEIGHT;
          return (
            <Link
              key={c.stepId}
              href={`/traces/${traceId}?step=${c.stepId}`}
              scroll={false}
              className="ctx-col"
              onMouseEnter={() => setHover(i)}
              onFocus={() => setHover(i)}
              aria-label={`Call ${i + 1}: ${formatCount(c.input)} input tokens, ${formatCount(c.cached)} cached`}
              style={{ opacity: hover !== null && hover !== i ? 0.55 : 1 }}
            >
              <span className="ctx-fresh" style={{ height: Math.max(total - cached - (cached > 0 ? 2 : 0), 1), background: FRESH }} />
              {cached > 0 ? <span className="ctx-cached" style={{ height: cached, background: CACHED }} /> : null}
            </Link>
          );
        })}
      </div>
      <div className="time-legend">
        <span className="legend-item">
          <span className="legend-swatch" style={{ background: FRESH }} aria-hidden="true" />
          New input
        </span>
        <span className="legend-item">
          <span className="legend-swatch" style={{ background: CACHED }} aria-hidden="true" />
          From prompt cache
        </span>
        <span className="sub">Peak {formatCount(peak)} tokens</span>
      </div>
      <div className="sub" style={{ marginTop: 6, minHeight: 18 }} aria-live="polite">
        {h
          ? `Call ${(hover ?? 0) + 1}${h.model ? ` · ${h.model}` : ''}: ${formatCount(h.input)} in (${h.input ? formatPercent(h.cached / h.input) : '0%'} cached) · ${formatCount(h.output)} out`
          : 'Hover a column for details; click to open the call.'}
      </div>
      <details className="table-view">
        <summary>Show as a table</summary>
        <table>
          <thead>
            <tr>
              <th>Call</th>
              <th>Model</th>
              <th className="num">Input</th>
              <th className="num">Cached</th>
              <th className="num">Output</th>
            </tr>
          </thead>
          <tbody>
            {calls.map((c, i) => (
              <tr key={c.stepId}>
                <td>
                  <Link href={`/traces/${traceId}?step=${c.stepId}`} scroll={false}>
                    {i + 1}. {c.name}
                  </Link>
                </td>
                <td>{c.model}</td>
                <td className="num">{formatCount(c.input)}</td>
                <td className="num">{formatCount(c.cached)}</td>
                <td className="num">{formatCount(c.output)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </details>
    </div>
  );
}
