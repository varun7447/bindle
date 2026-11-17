'use client';

import { useRouter } from 'next/navigation';
import { useEffect, useMemo, useRef, useState } from 'react';
import { paletteCommands, type Command } from '@/lib/commands';
import { BellIcon, GearIcon, HomeIcon, KeyIcon, PlugIcon, RunsIcon, SlidersIcon, TraceIcon } from './Icons';

const ICONS = { home: HomeIcon, runs: RunsIcon, alerts: BellIcon, settings: GearIcon, connect: PlugIcon, keys: KeyIcon, sdk: SlidersIcon, trace: TraceIcon };

/** Ctrl/⌘+K: jump to a page, a trace ID, or filtered runs. */
export function CommandPalette({ onClose }: { onClose: () => void }) {
  const router = useRouter();
  const [q, setQ] = useState('');
  const [sel, setSel] = useState(0);
  const input = useRef<HTMLInputElement>(null);
  const items = useMemo(() => paletteCommands(q), [q]);

  useEffect(() => input.current?.focus(), []);
  useEffect(() => setSel(0), [q]);

  const run = (c: Command | undefined) => {
    if (!c) return;
    onClose();
    router.push(c.href);
  };

  return (
    <div className="cmdk-backdrop" onMouseDown={onClose}>
      <div className="cmdk" role="dialog" aria-modal="true" aria-label="Search or jump to" onMouseDown={(e) => e.stopPropagation()}>
        <input
          ref={input}
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="Go to a page, paste a trace ID, or type an agent name…"
          aria-label="Search"
          role="combobox"
          aria-expanded="true"
          aria-controls="cmdk-list"
          aria-activedescendant={items[sel] ? `cmdk-${sel}` : undefined}
          onKeyDown={(e) => {
            if (e.key === 'Escape') onClose();
            else if (e.key === 'ArrowDown') {
              e.preventDefault();
              setSel((s) => Math.min(s + 1, items.length - 1));
            } else if (e.key === 'ArrowUp') {
              e.preventDefault();
              setSel((s) => Math.max(s - 1, 0));
            } else if (e.key === 'Enter') run(items[sel]);
          }}
        />
        {items.length === 0 ? (
          <div className="cmdk-empty">No matches.</div>
        ) : (
          <ul id="cmdk-list" role="listbox">
            {items.map((c, i) => {
              const Icon = ICONS[c.icon];
              return (
                <li key={c.id}>
                  <button id={`cmdk-${i}`} type="button" role="option" aria-selected={i === sel} onMouseEnter={() => setSel(i)} onClick={() => run(c)}>
                    <Icon />
                    {c.label}
                    {c.hint ? <span className="cmdk-hint">{c.hint}</span> : null}
                  </button>
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </div>
  );
}
