import { DRIFT_HELP, driftDetail, driftLabel } from '@/lib/passports';

/** A step badge for passport drift: observed, never blocked. */
export function DriftBadge({ drift }: { drift?: string[] }) {
  if (!drift?.length) return null;
  const kinds = [...new Set(drift.map((r) => r.split(':')[0]!))];
  return (
    <span className="badge badge-warn" title={`Outside the agent's passport: ${kinds.map(driftLabel).join(', ')}`}>
      <span aria-hidden="true">◇ </span>
      {kinds.length === 1 ? driftLabel(kinds[0]!) : `${kinds.length} passport drifts`}
    </span>
  );
}

/** What each drift on a step means. */
export function DriftNotice({ drift }: { drift?: string[] }) {
  if (!drift?.length) return null;
  return (
    <div className="notice notice-warn" role="note">
      <span aria-hidden="true">◇</span>
      <div>
        <strong>Outside this agent&apos;s passport.</strong> Observed only: the step ran normally.
        <ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>
          {drift.map((r) => {
            const kind = r.split(':')[0]!;
            const detail = driftDetail(r);
            return (
              <li key={r}>
                <strong>{driftLabel(kind)}</strong>
                {detail ? (
                  <>
                    {' '}
                    (<code>{detail}</code>)
                  </>
                ) : null}
                : {DRIFT_HELP[kind] ?? ''}
              </li>
            );
          })}
        </ul>
      </div>
    </div>
  );
}
