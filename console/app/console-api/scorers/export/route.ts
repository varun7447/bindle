// Checks download (JSON) for `bindle eval` in CI. Forwards to bindle-api with
// the signed-in user's session; this route adds no access.
import { apiBase } from '@/lib/api';
import { sessionToken } from '@/lib/auth';

export const dynamic = 'force-dynamic';

export async function GET() {
  const token = await sessionToken();
  const res = await fetch(`${apiBase()}/api/scorers`, { cache: 'no-store', headers: token ? { authorization: `Bearer ${token}` } : {} }).catch(() => null);
  if (!res) return new Response('Bindle API unreachable', { status: 502 });
  if (!res.ok) return new Response('could not export', { status: res.status === 401 ? 401 : 502 });
  const body = (await res.json()) as { scorers: Record<string, unknown>[] };
  const scorers = body.scorers.map(({ id: _id, created_at: _c, ...rest }) => rest);
  return new Response(JSON.stringify({ scorers }, null, 2) + '\n', {
    headers: { 'content-type': 'application/json', 'content-disposition': 'attachment; filename="bindle-checks.json"', 'cache-control': 'no-store' },
  });
}
