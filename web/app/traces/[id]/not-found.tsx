import Link from 'next/link';

export default function TraceNotFound() {
  return (
    <div className="panel">
      <h1>Trace not found</h1>
      <p className="sub">It may be older than your retention window, or the ID is wrong.</p>
      <Link href="/runs">Back to runs</Link>
    </div>
  );
}
