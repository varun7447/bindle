// Run download for replay with recorded tool outputs (bindle.Cassette).
// Forwards to bindle-api with the signed-in user's session, which enforces
// auth and tenancy; this route adds no access.
import { type NextRequest } from 'next/server';
import { apiBase } from '@/lib/api';
import { sessionToken } from '@/lib/auth';
import { isTraceId } from '@/lib/ids';

export const dynamic = 'force-dynamic';

export async function GET(_: NextRequest, { params }: { params: Promise<{ id: string }> }) {
  const id = (await params).id.toLowerCase();
  if (!isTraceId(id)) return new Response('invalid trace ID', { status: 400 });
  const token = await sessionToken();
  const res = await fetch(`${apiBase()}/api/traces/${id}/cassette`, {
    cache: 'no-store',
    headers: token ? { authorization: `Bearer ${token}` } : {},
  }).catch(() => null);
  if (!res) return new Response('Bindle API unreachable', { status: 502 });
  if (!res.ok) return new Response(res.status === 404 ? 'not found' : 'could not export', { status: res.status === 404 || res.status === 401 ? res.status : 502 });
  return new Response(res.body, {
    headers: {
      'content-type': 'application/json',
      'content-disposition': res.headers.get('content-disposition') ?? `attachment; filename="run-${id}.cassette.json"`,
      'cache-control': 'no-store',
    },
  });
}
