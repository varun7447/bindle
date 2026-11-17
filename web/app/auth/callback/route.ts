import { NextResponse, type NextRequest } from 'next/server';
import { OAUTH_COOKIE, SESSION_COOKIE, authConfig, cookieOptions } from '@/lib/auth';
import { exchangeCode, safeNext, sameString, seal, unseal, type OAuthState } from '@/lib/authcore';

export const dynamic = 'force-dynamic';

function fail(cfg: { consoleUrl: string }, reason: 'cancelled' | 'mismatch' | 'exchange') {
  const res = NextResponse.redirect(`${cfg.consoleUrl}/auth/error?reason=${reason}`);
  res.cookies.delete({ name: OAUTH_COOKIE, path: '/auth' });
  return res;
}

// Cognito redirects here with ?code&state. The state must match the sealed
// cookie set by /auth/login; the code is exchanged with the PKCE verifier.
export async function GET(req: NextRequest) {
  const cfg = authConfig();
  if (!cfg) return NextResponse.redirect(new URL('/', req.url));
  const q = req.nextUrl.searchParams;
  if (q.get('error')) return fail(cfg, 'cancelled');
  const code = q.get('code');
  const state = q.get('state');
  const saved = unseal<OAuthState>(req.cookies.get(OAUTH_COOKIE)?.value, cfg.sessionKey);
  if (!code || !state || !saved || saved.exp < Math.floor(Date.now() / 1000) || !sameString(state, saved.state)) {
    return fail(cfg, 'mismatch');
  }
  let tokens: { accessToken: string; expiresIn: number };
  try {
    tokens = await exchangeCode(cfg, code, saved.verifier);
  } catch {
    return fail(cfg, 'exchange');
  }
  const exp = Math.floor(Date.now() / 1000) + tokens.expiresIn;
  const res = NextResponse.redirect(`${cfg.consoleUrl}${safeNext(saved.next)}`);
  res.cookies.set(SESSION_COOKIE, seal({ at: tokens.accessToken, exp }, cfg.sessionKey), cookieOptions(cfg, tokens.expiresIn));
  res.cookies.delete({ name: OAUTH_COOKIE, path: '/auth' });
  return res;
}
