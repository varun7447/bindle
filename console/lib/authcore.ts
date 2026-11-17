// Pure, framework-free pieces of console sign-in (Amazon Cognito, OAuth 2.0
// authorization code flow with PKCE). Unit-tested; lib/auth.ts wires them
// to Next.js cookies and redirects.

import { createCipheriv, createDecipheriv, createHash, randomBytes, timingSafeEqual } from 'node:crypto';

export interface AuthConfig {
  domain: string; // https://<prefix>.auth.<region>.amazoncognito.com
  clientId: string;
  clientSecret?: string;
  consoleUrl: string; // public base URL of this console, e.g. https://app.bindle.ai
  sessionKey: Buffer; // 32 bytes, AES-256-GCM
}

/**
 * Reads sign-in settings. Returns null when Cognito is not configured (local
 * development with the API's dev bypass). Partial or unsafe configuration
 * throws, so a misconfigured deploy fails loudly instead of running open.
 */
export function authConfigFrom(env: Record<string, string | undefined>): AuthConfig | null {
  const domain = env.BINDLE_COGNITO_DOMAIN;
  const clientId = env.BINDLE_COGNITO_CLIENT_ID;
  if (!domain && !clientId) return null;
  if (!domain || !clientId) throw new Error('Set both BINDLE_COGNITO_DOMAIN and BINDLE_COGNITO_CLIENT_ID');
  if (!domain.startsWith('https://')) throw new Error('BINDLE_COGNITO_DOMAIN must be an https URL');
  const consoleUrl = (env.BINDLE_CONSOLE_URL ?? '').replace(/\/+$/, '');
  if (!/^https:\/\/|^http:\/\/(127\.0\.0\.1|localhost)(:\d+)?$/.test(consoleUrl)) {
    throw new Error('BINDLE_CONSOLE_URL must be https (or http://127.0.0.1 for local testing)');
  }
  const key = Buffer.from(env.BINDLE_SESSION_SECRET ?? '', 'base64');
  if (key.length !== 32) throw new Error('BINDLE_SESSION_SECRET must be 32 random bytes, base64-encoded');
  return { domain: domain.replace(/\/+$/, ''), clientId, clientSecret: env.BINDLE_COGNITO_CLIENT_SECRET || undefined, consoleUrl, sessionKey: key };
}

const b64url = (b: Buffer) => b.toString('base64url');

/** Encrypts and authenticates a JSON payload (AES-256-GCM, random IV). */
export function seal(payload: object, key: Buffer): string {
  const iv = randomBytes(12);
  const cipher = createCipheriv('aes-256-gcm', key, iv);
  const ct = Buffer.concat([cipher.update(JSON.stringify(payload), 'utf8'), cipher.final()]);
  return b64url(Buffer.concat([iv, ct, cipher.getAuthTag()]));
}

/** Reverses seal; null if tampered, malformed or sealed with another key. */
export function unseal<T>(sealed: string | undefined, key: Buffer): T | null {
  if (!sealed || sealed.length > 6000) return null;
  try {
    const raw = Buffer.from(sealed, 'base64url');
    if (raw.length < 12 + 16 + 1) return null;
    const decipher = createDecipheriv('aes-256-gcm', key, raw.subarray(0, 12));
    decipher.setAuthTag(raw.subarray(raw.length - 16));
    const pt = Buffer.concat([decipher.update(raw.subarray(12, raw.length - 16)), decipher.final()]);
    return JSON.parse(pt.toString('utf8')) as T;
  } catch {
    return null;
  }
}

export interface Session {
  at: string; // Cognito access token
  exp: number; // epoch seconds
}

export interface OAuthState {
  state: string;
  verifier: string;
  next: string;
  exp: number;
}

/** A session is usable until a minute before its token expires. */
export function validSession(s: Session | null, nowSec: number): s is Session {
  return !!s && typeof s.at === 'string' && s.at.length > 0 && typeof s.exp === 'number' && s.exp - 60 > nowSec;
}

export function pkce(): { verifier: string; challenge: string } {
  const verifier = b64url(randomBytes(32));
  return { verifier, challenge: b64url(createHash('sha256').update(verifier).digest()) };
}

export function randomState(): string {
  return b64url(randomBytes(24));
}

export function redirectUri(cfg: AuthConfig): string {
  return `${cfg.consoleUrl}/auth/callback`;
}

export function authorizeUrl(cfg: AuthConfig, state: string, challenge: string): string {
  const u = new URL(`${cfg.domain}/oauth2/authorize`);
  u.searchParams.set('response_type', 'code');
  u.searchParams.set('client_id', cfg.clientId);
  u.searchParams.set('redirect_uri', redirectUri(cfg));
  u.searchParams.set('scope', 'openid email');
  u.searchParams.set('state', state);
  u.searchParams.set('code_challenge', challenge);
  u.searchParams.set('code_challenge_method', 'S256');
  return u.toString();
}

export function logoutUrl(cfg: AuthConfig): string {
  const u = new URL(`${cfg.domain}/logout`);
  u.searchParams.set('client_id', cfg.clientId);
  u.searchParams.set('logout_uri', `${cfg.consoleUrl}/`);
  return u.toString();
}

/** Constant-time string comparison for the OAuth state. */
export function sameString(a: string, b: string): boolean {
  const x = Buffer.from(a);
  const y = Buffer.from(b);
  return x.length === y.length && timingSafeEqual(x, y);
}

/** Only same-site relative paths are allowed as post-login destinations. */
export function safeNext(next: string | null | undefined): string {
  if (!next || !next.startsWith('/') || next.startsWith('//') || next.startsWith('/\\') || next.length > 512) return '/';
  return next;
}

/** Exchanges an authorization code for tokens at the Cognito token endpoint. */
export async function exchangeCode(
  cfg: AuthConfig,
  code: string,
  verifier: string,
  fetchImpl: typeof fetch = fetch,
): Promise<{ accessToken: string; expiresIn: number }> {
  const body = new URLSearchParams({
    grant_type: 'authorization_code',
    client_id: cfg.clientId,
    code,
    redirect_uri: redirectUri(cfg),
    code_verifier: verifier,
  });
  const headers: Record<string, string> = { 'content-type': 'application/x-www-form-urlencoded' };
  if (cfg.clientSecret) {
    headers.authorization = 'Basic ' + Buffer.from(`${cfg.clientId}:${cfg.clientSecret}`).toString('base64');
  }
  const res = await fetchImpl(`${cfg.domain}/oauth2/token`, { method: 'POST', headers, body, cache: 'no-store' });
  if (!res.ok) throw new Error(`token endpoint returned HTTP ${res.status}`);
  const json = (await res.json()) as { access_token?: string; expires_in?: number };
  if (!json.access_token || !json.expires_in) throw new Error('token endpoint returned no access token');
  return { accessToken: json.access_token, expiresIn: json.expires_in };
}

/** Origin check for state-changing requests when sign-in is enabled. */
export function originAllowed(origin: string | null, cfg: AuthConfig): boolean {
  if (!origin) return false;
  try {
    return new URL(origin).origin === new URL(cfg.consoleUrl).origin;
  } catch {
    return false;
  }
}
