import { NextResponse, type NextRequest } from 'next/server';
import { SESSION_COOKIE, authConfig } from '@/lib/auth';
import { logoutUrl, originAllowed } from '@/lib/authcore';

export const dynamic = 'force-dynamic';

// POST only (a link can't sign someone out), from this console's origin.
export async function POST(req: NextRequest) {
  const cfg = authConfig();
  if (!cfg) return NextResponse.redirect(new URL('/', req.url), 303);
  if (!originAllowed(req.headers.get('origin'), cfg)) return new NextResponse('Forbidden', { status: 403 });
  const res = NextResponse.redirect(logoutUrl(cfg), 303);
  res.cookies.delete(SESSION_COOKIE);
  return res;
}
