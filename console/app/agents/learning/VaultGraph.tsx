import Link from 'next/link';
import type { Graph } from '@/lib/learning';
import { targetHref } from '@/lib/learning';

const FILL: Record<string, string> = {
  skill: 'var(--accent)',
  lesson: 'var(--k-tool)',
  incident: 'var(--critical)',
  note: 'var(--k-neutral)',
  run: 'var(--k-llm)',
  tool: 'var(--k-retrieval)',
  agent: 'var(--k-handoff)',
  missing: 'var(--muted)',
};

/** The vault's link graph as plain SVG: notes and what they link to. Every
 * node is a link, and the table on the page lists the same data. */
export function VaultGraph({ graph, agent }: { graph: Graph; agent: string }) {
  const at = new Map(graph.nodes.map((n) => [n.id, n]));
  return (
    <svg
      viewBox={`0 0 ${graph.width} ${graph.height}`}
      width="100%"
      style={{ maxWidth: graph.width, display: 'block', margin: '0 auto' }}
      role="img"
      aria-label={`Link graph: ${graph.nodes.length} nodes, ${graph.edges.length} links`}
    >
      {graph.edges.map((e) => {
        const a = at.get(e.from)!;
        const b = at.get(e.to)!;
        return <line key={`${e.from}->${e.to}`} x1={a.x} y1={a.y} x2={b.x} y2={b.y} stroke="var(--axis)" strokeWidth={1.5} />;
      })}
      {graph.nodes.map((n) => (
        <Link key={n.id} href={targetHref(n.id, agent)}>
          <g>
            <title>{n.id}</title>
            <circle cx={n.x} cy={n.y} r={8} fill={FILL[n.kind]} stroke="var(--surface)" strokeWidth={2} />
            <text x={n.x} y={n.y + 22} textAnchor="middle" fontSize={11} fill="var(--text-2)">
              {n.label.length > 22 ? `${n.label.slice(0, 21)}…` : n.label}
            </text>
          </g>
        </Link>
      ))}
    </svg>
  );
}
