// Polled by the Connect page's "waiting for first trace" state. Forwards to
// bindle-api, which enforces auth and tenancy; this route adds no access.
import { NextResponse, type NextRequest } from 'next/server';
import { ApiError, api } from '@/lib/api';
import { validSince } from '@/lib/ids';

export const dynamic = 'force-dynamic';

export async function GET(req: NextRequest) {
  const since = validSince(req.nextUrl.searchParams.get('since'));
  if (!since) return NextResponse.json({ error: { code: 'bad_request', message: 'since must be a recent past timestamp' } }, { status: 400 });
  try {
    const res = await api.runs({ from: since, to: new Date().toISOString(), limit: '1' });
    return NextResponse.json({ run: res.runs[0] ?? null }, { headers: { 'cache-control': 'no-store' } });
  } catch (e) {
    const status = e instanceof ApiError && e.status === 401 ? 401 : 502;
    return NextResponse.json({ error: { code: status === 401 ? 'unauthenticated' : 'upstream', message: 'Could not check for traces' } }, { status });
  }
}
