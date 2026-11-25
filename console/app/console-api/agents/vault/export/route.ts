// Download of one agent's knowledge vault as a zip of plain Markdown files.
// Forwards to bindle-api with the signed-in user's session, which enforces
// auth, tenancy and the learning_loop feature flag; this route adds no access.
import { type NextRequest } from 'next/server';
import { apiBase, buildUrl } from '@/lib/api';
import { sessionToken } from '@/lib/auth';

export const dynamic = 'force-dynamic';

export async function GET(req: NextRequest) {
  const agent = req.nextUrl.searchParams.get('agent') ?? '';
  if (agent.length === 0 || agent.length > 128) return new Response('agent is required', { status: 400 });
  const token = await sessionToken();
  const res = await fetch(buildUrl(apiBase(), '/api/vault/export', { agent }), {
    cache: 'no-store',
    headers: token ? { authorization: `Bearer ${token}` } : {},
  }).catch(() => null);
  if (!res) return new Response('Bindle API unreachable', { status: 502 });
  if (!res.ok) return new Response(res.status === 404 ? 'not found' : 'could not export', { status: res.status === 404 || res.status === 401 ? res.status : 502 });
  return new Response(res.body, {
    headers: {
      'content-type': 'application/zip',
      'content-disposition': `attachment; filename="vault-${agent.replace(/[^A-Za-z0-9_-]+/g, '-').slice(0, 60)}.zip"`,
      'cache-control': 'no-store',
      'x-content-type-options': 'nosniff',
    },
  });
}
