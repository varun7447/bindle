import Link from 'next/link';
import { FRAMEWORKS, envBlock, frameworkById } from '@/lib/snippets';
import { one } from '@/lib/ids';
import { WaitForTrace } from './WaitForTrace';

export default async function ConnectPage({
  searchParams,
}: {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const fw = frameworkById(one((await searchParams).fw));
  const endpoint = process.env.BINDLE_PUBLIC_INGEST_URL ?? 'https://ingest.bindle.ai';

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Connect an agent</h1>
          <div className="sub">Three steps, no agent to host. Your app sends OpenTelemetry straight to Bindle.</div>
        </div>
      </div>

      <div className="grid-2">
        <section className="panel">
          <nav className="tabs" aria-label="Framework">
            {FRAMEWORKS.map((f) => (
              <Link key={f.id} href={`/connect?fw=${f.id}`} scroll={false} aria-current={f.id === fw.id ? 'true' : undefined}>
                {f.label}
              </Link>
            ))}
          </nav>
          <ol className="steps-list">
            <li>
              <h2>Install</h2>
              <pre className="code">{fw.install}</pre>
            </li>
            <li>
              <h2>Set your key and endpoint</h2>
              <pre className="code">{envBlock(endpoint)}</pre>
              <div className="sub">
                Self-serve keys arrive with sign-up. Locally, use <code>BINDLE_API_KEY</code> from <code>deploy/.env</code>.
                Keep keys out of source code.
              </div>
            </li>
            <li>
              <h2>{fw.id === 'otel' ? 'Point your exporter at Bindle' : 'Initialize'}</h2>
              <pre className="code">{fw.code.replace('{{ENDPOINT}}', endpoint)}</pre>
              {fw.note ? <div className="sub">{fw.note}</div> : null}
            </li>
          </ol>
        </section>

        <div className="stack">
          <section className="panel" aria-label="First trace">
            <WaitForTrace />
          </section>
          <section className="panel">
            <h2>What gets captured</h2>
            <ul className="sub" style={{ margin: 0, paddingLeft: 18, display: 'grid', gap: 6 }}>
              <li>Every LLM call, tool call, retrieval, decision, retry and handoff as a step in one trace.</li>
              <li>
                PII and secrets are redacted in your process before export, and again on our servers. Set{' '}
                <code>BINDLE_CAPTURE_CONTENT=false</code> to send no prompts or outputs at all.
              </li>
              <li>Export is asynchronous and bounded: if Bindle is slow or down, spans are dropped, never your agent.</li>
            </ul>
          </section>
        </div>
      </div>
    </>
  );
}
