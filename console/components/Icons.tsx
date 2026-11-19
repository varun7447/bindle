// Inline 16px stroke icons (no icon dependency). Decorative: always paired
// with a text label, so they are aria-hidden.

const base = { width: 16, height: 16, viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', strokeWidth: 2, strokeLinecap: 'round' as const, strokeLinejoin: 'round' as const, 'aria-hidden': true };

export const HomeIcon = () => (
  <svg {...base}>
    <path d="M3 10.5 12 3l9 7.5V20a1 1 0 0 1-1 1h-5v-6H9v6H4a1 1 0 0 1-1-1z" />
  </svg>
);
export const RunsIcon = () => (
  <svg {...base}>
    <path d="M4 6h16M4 12h10M4 18h13" />
  </svg>
);
export const BellIcon = () => (
  <svg {...base}>
    <path d="M6 8a6 6 0 1 1 12 0c0 7 3 8 3 8H3s3-1 3-8M10 20a2 2 0 0 0 4 0" />
  </svg>
);
export const GearIcon = () => (
  <svg {...base}>
    <circle cx="12" cy="12" r="3" />
    <path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z" />
  </svg>
);
export const SearchIcon = () => (
  <svg {...base}>
    <circle cx="11" cy="11" r="7" />
    <path d="m20 20-3.5-3.5" />
  </svg>
);
export const PlugIcon = () => (
  <svg {...base}>
    <path d="M9 2v6M15 2v6M6 8h12v4a6 6 0 0 1-12 0zM12 18v4" />
  </svg>
);
export const KeyIcon = () => (
  <svg {...base}>
    <circle cx="7.5" cy="15.5" r="4.5" />
    <path d="m10.5 12.5 9-9M16 7l3 3M14 9l2 2" />
  </svg>
);
export const SlidersIcon = () => (
  <svg {...base}>
    <path d="M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6" />
  </svg>
);
export const ShieldIcon = () => (
  <svg {...base}>
    <path d="M12 3 4 6v6c0 4.5 3.4 8.3 8 9 4.6-.7 8-4.5 8-9V6z" />
    <path d="M9.5 12.5 11 14l3.5-3.5" />
  </svg>
);
export const MoonIcon = () => (
  <svg {...base}>
    <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" />
  </svg>
);
export const SunIcon = () => (
  <svg {...base}>
    <circle cx="12" cy="12" r="4" />
    <path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
  </svg>
);
export const LogoutIcon = () => (
  <svg {...base}>
    <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9" />
  </svg>
);
export const TraceIcon = () => (
  <svg {...base}>
    <path d="M4 5h8M8 10h10M6 15h7M10 20h9" />
  </svg>
);
export const CheckCircleIcon = () => (
  <svg {...base}>
    <circle cx="12" cy="12" r="9" />
    <path d="m8 12 3 3 5-6" />
  </svg>
);
export const XCircleIcon = () => (
  <svg {...base}>
    <circle cx="12" cy="12" r="9" />
    <path d="m9 9 6 6M15 9l-6 6" />
  </svg>
);
export const AlertIcon = () => (
  <svg {...base}>
    <path d="M10.3 3.9 2.4 18a2 2 0 0 0 1.7 3h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0zM12 9v4M12 17h.01" />
  </svg>
);
export const ActivityIcon = () => (
  <svg {...base}>
    <path d="M3 12h4l3-8 4 16 3-8h4" />
  </svg>
);
export const ClockIcon = () => (
  <svg {...base}>
    <circle cx="12" cy="12" r="9" />
    <path d="M12 7v5l3 2" />
  </svg>
);
export const CoinIcon = () => (
  <svg {...base}>
    <circle cx="12" cy="12" r="9" />
    <path d="M14.5 9.5c-.4-1-1.4-1.5-2.5-1.5-1.5 0-2.5.8-2.5 2s1.2 1.6 2.5 2 2.5.8 2.5 2-1 2-2.5 2c-1.1 0-2.1-.5-2.5-1.5M12 6.5V8M12 16v1.5" />
  </svg>
);
export const ChartIcon = () => (
  <svg {...base}>
    <path d="M4 20V10M10 20V4M16 20v-7M22 20H2" />
  </svg>
);
export const BotIcon = () => (
  <svg {...base}>
    <rect x="4" y="8" width="16" height="12" rx="3" />
    <path d="M12 8V4M9 13h.01M15 13h.01M9.5 17h5" />
  </svg>
);
export const SendIcon = () => (
  <svg {...base}>
    <path d="m22 2-7 20-4-9-9-4zM22 2 11 13" />
  </svg>
);
export const HistoryIcon = () => (
  <svg {...base}>
    <path d="M3 12a9 9 0 1 0 3-6.7L3 8M3 3v5h5M12 7v5l3 2" />
  </svg>
);
export const MailIcon = () => (
  <svg {...base}>
    <rect x="3" y="5" width="18" height="14" rx="2" />
    <path d="m3 7 9 6 9-6" />
  </svg>
);
export const LinkIcon = () => (
  <svg {...base}>
    <path d="M10 14a5 5 0 0 0 7.1 0l3-3a5 5 0 0 0-7.1-7.1l-1 1M14 10a5 5 0 0 0-7.1 0l-3 3a5 5 0 0 0 7.1 7.1l1-1" />
  </svg>
);
export const ChatIcon = () => (
  <svg {...base}>
    <path d="M21 12a8 8 0 0 1-11.6 7.1L4 20l1-4.6A8 8 0 1 1 21 12z" />
  </svg>
);
export const LayersIcon = () => (
  <svg {...base}>
    <path d="m12 3 9 5-9 5-9-5zM3 13l9 5 9-5" />
  </svg>
);
