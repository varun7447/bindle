// Load balancer health check: no session, no API call, no data.
export const dynamic = 'force-dynamic';

export function GET() {
  return new Response('ok', { headers: { 'content-type': 'text/plain', 'cache-control': 'no-store' } });
}
