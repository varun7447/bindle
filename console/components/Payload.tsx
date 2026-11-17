import Link from 'next/link';
import type { EmptyReason } from '@/lib/content';
import { formatBytes, prettyPayload } from '@/lib/format';
import { parseMessages, type Message } from '@/lib/messages';

const ROLE_LABEL: Record<string, string> = { system: 'System', user: 'User', assistant: 'Assistant', tool: 'Tool result' };

function Conversation({ messages }: { messages: Message[] }) {
  return (
    <ol className="convo">
      {messages.map((m, i) => (
        <li key={i} className={`msg msg-${m.role in ROLE_LABEL ? m.role : 'other'}`}>
          <div className="msg-role">{ROLE_LABEL[m.role] ?? m.role}</div>
          {m.parts.map((p, j) =>
            p.kind === 'text' ? (
              <div key={j} className="msg-text">
                {p.text}
              </div>
            ) : p.kind === 'tool_call' ? (
              <div key={j} className="msg-call">
                <div className="msg-call-head">
                  <span aria-hidden="true">→</span> Calls <code>{p.name || 'tool'}</code>
                </div>
                {p.args && p.args !== '{}' && p.args !== '""' ? <pre>{p.args}</pre> : null}
              </div>
            ) : (
              <div key={j} className="msg-call">
                <div className="msg-call-head">
                  <span aria-hidden="true">←</span> Result{p.name ? <> from <code>{p.name}</code></> : null}
                </div>
                <pre>{p.text}</pre>
              </div>
            ),
          )}
        </li>
      ))}
    </ol>
  );
}

/** A step's input, output or reasoning: a conversation when it is one, the
 * text otherwise, and when empty, the reason it's empty. */
export function Payload({
  label,
  side,
  text,
  bytes,
  truncated,
  empty,
}: {
  label: string;
  side: 'input' | 'output';
  text: string;
  bytes: number;
  truncated?: boolean;
  empty?: EmptyReason;
}) {
  const messages = text ? parseMessages(text, side) : null;
  return (
    <div>
      <div className="payload-label">
        <span>
          {label}
          {messages ? <span className="sub"> · {messages.length} message{messages.length === 1 ? '' : 's'}</span> : null}
        </span>
        <span>{bytes ? formatBytes(bytes) : ''}</span>
      </div>
      {truncated ? (
        <div className="notice notice-warn" role="note" style={{ marginBottom: 6 }}>
          <span aria-hidden="true">⚠</span>
          <div>
            Showing the first {formatBytes(new TextEncoder().encode(text).length)} of {formatBytes(bytes)}.
          </div>
        </div>
      ) : null}
      {!text ? (
        <div className="payload empty-payload" role="note">
          <div className="empty-title">{empty?.title ?? 'Not recorded.'}</div>
          {empty?.detail ? <div className="sub">{empty.detail}</div> : null}
          {empty?.code ? <pre className="code" style={{ margin: '6px 0 0' }}>{empty.code}</pre> : null}
          {empty?.href ? (
            <Link href={empty.href} style={{ fontSize: 13 }}>
              Open SDK settings →
            </Link>
          ) : null}
        </div>
      ) : messages ? (
        <>
          <Conversation messages={messages} />
          <details className="raw-toggle">
            <summary>Raw</summary>
            <pre className="payload">{prettyPayload(text)}</pre>
          </details>
        </>
      ) : (
        <pre className="payload">{prettyPayload(text)}</pre>
      )}
    </div>
  );
}
