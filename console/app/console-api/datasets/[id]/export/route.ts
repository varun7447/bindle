// Dataset download (JSON Lines). Forwards to bindle-api with the signed-in
// user's session, which enforces auth and tenancy; this route adds no access.
import { type NextRequest } from 'next/server';
import { apiBase } from '@/lib/api';
import { sessionToken } from '@/lib/auth';

export const dynamic = 'force-dynamic';

export async function GET(_: NextRequest, { params }: { params: Promise<{ id: string }> }) {
  const id = (await params).id;
  if (!/^ds_[0-9a-f]{20}$/.test(id)) return new Response('invalid dataset ID', { status: 400 });
  const token = await sessionToken();
  const res = await fetch(`${apiBase()}/api/datasets/${id}/export`, {
    cache: 'no-store',
    headers: token ? { authorization: `Bearer ${token}` } : {},
  }).catch(() => null);
  if (!res) return new Response('Bindle API unreachable', { status: 502 });
  if (!res.ok) return new Response(res.status === 404 ? 'not found' : 'could not export', { status: res.status === 404 || res.status === 401 ? res.status : 502 });
  return new Response(res.body, {
    headers: {
      'content-type': 'application/x-ndjson',
      'content-disposition': res.headers.get('content-disposition') ?? 'attachment; filename="dataset.jsonl"',
      'cache-control': 'no-store',
    },
  });
}
