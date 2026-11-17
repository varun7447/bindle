// Console sign-in wired to Next.js: session cookie, redirects, origin checks.
// Server-only (reads cookies and request headers).

import { cookies, headers } from 'next/headers';
import { redirect } from 'next/navigation';
import { authConfigFrom, originAllowed, unseal, validSession, type AuthConfig, type Session } from './authcore';

export const SESSION_COOKIE = 'bindle_session';
export const OAUTH_COOKIE = 'bindle_oauth';

let cached: AuthConfig | null | undefined;

/** Sign-in settings, or null when Cognito is not configured (local dev). */
export function authConfig(): AuthConfig | null {
  if (cached === undefined) cached = authConfigFrom(process.env);
  return cached;
}

export function cookieOptions(cfg: AuthConfig, maxAgeSec: number) {
  return {
    httpOnly: true,
    secure: cfg.consoleUrl.startsWith('https://'),
    sameSite: 'lax' as const,
    path: '/',
    maxAge: maxAgeSec,
  };
}

/** The signed-in user's Cognito access token, or null. */
export async function sessionToken(): Promise<string | null> {
  const cfg = authConfig();
  if (!cfg) return null;
  const s = unseal<Session>((await cookies()).get(SESSION_COOKIE)?.value, cfg.sessionKey);
  return validSession(s, Math.floor(Date.now() / 1000)) ? s.at : null;
}

/** Redirects to sign-in when it is enabled and there is no valid session. */
export async function requireSession(currentPath: string): Promise<void> {
  if (!authConfig()) return;
  if (!(await sessionToken())) redirect(`/auth/login?next=${encodeURIComponent(currentPath)}`);
}

/** Rejects state-changing requests from other origins when sign-in is on. */
export async function assertSameOrigin(): Promise<void> {
  const cfg = authConfig();
  if (!cfg) return;
  if (!originAllowed((await headers()).get('origin'), cfg)) throw new Error('Request origin not allowed');
}
