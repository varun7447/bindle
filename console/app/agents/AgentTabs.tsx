import Link from 'next/link';

/** Overview / Ledger / Learning switch on the Agents pages. Each extra view
 * is shown only when the workspace has its feature (operator-set flags). */
export function AgentTabs({
  agent,
  current,
  ledger = true,
  learning = false,
}: {
  agent?: string;
  current: 'overview' | 'ledger' | 'learning';
  ledger?: boolean;
  learning?: boolean;
}) {
  const q = agent !== undefined ? `?${new URLSearchParams({ agent }).toString()}` : '';
  return (
    <nav className="seg" aria-label="Agent views" style={{ marginBottom: 16 }}>
      <Link href={`/agents${q}`} aria-current={current === 'overview' ? 'true' : undefined}>
        Overview
      </Link>
      {ledger ? (
        <Link href={`/agents/ledger${q}`} aria-current={current === 'ledger' ? 'true' : undefined}>
          Ledger
        </Link>
      ) : null}
      {learning ? (
        <Link href={`/agents/learning${q}`} aria-current={current === 'learning' ? 'true' : undefined}>
          Learning
        </Link>
      ) : null}
    </nav>
  );
}
