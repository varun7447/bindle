import { NextResponse, type NextRequest } from 'next/server';
import { OAUTH_COOKIE, authConfig, cookieOptions } from '@/lib/auth';
import { authorizeUrl, pkce, randomState, safeNext, seal } from '@/lib/authcore';

export const dynamic = 'force-dynamic';

// Starts sign-in: remembers state + PKCE verifier in a short-lived sealed
// cookie and sends the browser to Cognito's sign-in page.
export async function GET(req: NextRequest) {
  const cfg = authConfig();
  if (!cfg) return NextResponse.redirect(new URL('/', req.url));
  const { verifier, challenge } = pkce();
  const state = randomState();
  const next = safeNext(req.nextUrl.searchParams.get('next'));
  const res = NextResponse.redirect(authorizeUrl(cfg, state, challenge));
  res.cookies.set(OAUTH_COOKIE, seal({ state, verifier, next, exp: Math.floor(Date.now() / 1000) + 600 }, cfg.sessionKey), {
    ...cookieOptions(cfg, 600),
    path: '/auth',
  });
  return res;
}
