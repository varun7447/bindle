// Download of one agent's daily ledger summary (Markdown, audit evidence).
// Forwards to bindle-api with the signed-in user's session, which enforces
// auth, tenancy and the ledger feature flag; this route adds no access.
import { type NextRequest } from 'next/server';
import { apiBase, buildUrl } from '@/lib/api';
import { sessionToken } from '@/lib/auth';

export const dynamic = 'force-dynamic';

export async function GET(req: NextRequest) {
  const agent = req.nextUrl.searchParams.get('agent') ?? '';
  const day = req.nextUrl.searchParams.get('day') ?? '';
  if (agent.length > 256) return new Response('agent is too long', { status: 400 });
  if (!/^\d{4}-\d{2}-\d{2}$/.test(day)) return new Response('day must be YYYY-MM-DD', { status: 400 });
  const token = await sessionToken();
  const res = await fetch(buildUrl(apiBase(), '/api/agents/ledger/summary', { agent, day }), {
    cache: 'no-store',
    headers: token ? { authorization: `Bearer ${token}` } : {},
  }).catch(() => null);
  if (!res) return new Response('Bindle API unreachable', { status: 502 });
  if (!res.ok) return new Response(res.status === 404 ? 'not found' : 'could not export', { status: res.status === 404 || res.status === 401 ? res.status : 502 });
  return new Response(res.body, {
    headers: {
      'content-type': 'text/markdown; charset=utf-8',
      'content-disposition': res.headers.get('content-disposition') ?? `attachment; filename="ledger-${day}.md"`,
      'cache-control': 'no-store',
      'x-content-type-options': 'nosniff',
    },
  });
}
