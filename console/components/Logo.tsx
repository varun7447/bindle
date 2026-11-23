// The Bindle mark: three staggered bars, like a trace waterfall: the steps of
// one run, bundled together, on an indigo tile. The same drawing is in
// app/icon.svg (favicon) and docs/images/logo.svg.

export function BindleMark({ size = 22, title }: { size?: number; title?: string }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" role={title ? 'img' : undefined} aria-hidden={title ? undefined : true} aria-label={title}>
      <rect x="1" y="1" width="22" height="22" rx="5" fill="#4c56d8" />
      <rect x="5" y="6.5" width="14" height="2.6" rx="1.3" fill="#fff" />
      <rect x="8" y="10.7" width="9" height="2.6" rx="1.3" fill="#fff" />
      <rect x="11" y="14.9" width="8" height="2.6" rx="1.3" fill="#fff" />
    </svg>
  );
}

export function BindleLogo({ size = 22 }: { size?: number }) {
  return (
    <span className="logo">
      <BindleMark size={size} />
      <span className="logo-word">bindle</span>
    </span>
  );
}
