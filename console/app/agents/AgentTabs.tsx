import Link from 'next/link';

/** Overview / Ledger switch on the Agents pages. Rendered only when the
 * workspace has the ledger feature (an operator-set flag). */
export function AgentTabs({ agent, current }: { agent?: string; current: 'overview' | 'ledger' }) {
  const q = agent !== undefined ? `?${new URLSearchParams({ agent }).toString()}` : '';
  return (
    <nav className="seg" aria-label="Agent views" style={{ marginBottom: 16 }}>
      <Link href={`/agents${q}`} aria-current={current === 'overview' ? 'true' : undefined}>
        Overview
      </Link>
      <Link href={`/agents/ledger${q}`} aria-current={current === 'ledger' ? 'true' : undefined}>
        Ledger
      </Link>
    </nav>
  );
}
