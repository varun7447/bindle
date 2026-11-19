import { FLAG_HELP, flagLabel, reasonText } from '@/lib/security';

/** A compact badge on a flagged step (waterfall row, step header). */
export function SecurityBadge({ flags }: { flags?: string[] }) {
  if (!flags?.length) return null;
  const labels = flags.map(flagLabel);
  return (
    <span className="badge badge-warn" title={`Security signal: ${labels.join(', ')}`}>
      <span aria-hidden="true">⚠ </span>
      {flags.length === 1 ? labels[0] : `${flags.length} security signals`}
    </span>
  );
}

/** What each flag on a step means, with the reasons it was raised. */
export function SecurityNotice({ flags, reasons }: { flags?: string[]; reasons?: string[] }) {
  if (!flags?.length) return null;
  return (
    <div className="notice notice-warn" role="note">
      <span aria-hidden="true">⚠</span>
      <div>
        <strong>Security signal{flags.length === 1 ? '' : 's'} to review.</strong> These are leads, not verdicts.
        <ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>
          {flags.map((f) => {
            const why = (reasons ?? []).filter((r) => r.startsWith(f + ':')).map((r) => reasonText(r));
            return (
              <li key={f}>
                <strong>{flagLabel(f)}</strong>
                {why.length ? `: ${why.join('; ')}.` : '.'} {FLAG_HELP[f] ?? ''}
              </li>
            );
          })}
        </ul>
      </div>
    </div>
  );
}
