// The Bindle mark: three staggered bars, like a trace waterfall: the steps of
// one run, bundled together. The same drawing is in app/icon.svg (favicon)
// and docs/images/logo.svg.

export function BindleMark({ size = 22, title }: { size?: number; title?: string }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" role={title ? 'img' : undefined} aria-hidden={title ? undefined : true} aria-label={title}>
      <defs>
        <linearGradient id="bindle-mark-g" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="#16b394" />
          <stop offset="1" stopColor="#0b6a86" />
        </linearGradient>
      </defs>
      <rect x="1" y="1" width="22" height="22" rx="6.5" fill="url(#bindle-mark-g)" />
      <rect x="5" y="6" width="14" height="3" rx="1.5" fill="#fff" />
      <rect x="8" y="10.5" width="9" height="3" rx="1.5" fill="#fff" fillOpacity="0.85" />
      <rect x="11" y="15" width="8" height="3" rx="1.5" fill="#fff" fillOpacity="0.7" />
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
