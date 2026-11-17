import { createHash, randomBytes } from 'node:crypto';
import { describe, expect, it, vi } from 'vitest';
import {
  authConfigFrom,
  authorizeUrl,
  exchangeCode,
  logoutUrl,
  originAllowed,
  pkce,
  safeNext,
  sameString,
  seal,
  unseal,
  validSession,
  type AuthConfig,
} from './authcore';
import { keyStatus, validGrace } from './keys';

const key = randomBytes(32);
const env = {
  BINDLE_COGNITO_DOMAIN: 'https://bindle-test.auth.us-west-2.amazoncognito.com',
  BINDLE_COGNITO_CLIENT_ID: 'client123',
  BINDLE_CONSOLE_URL: 'https://app.bindle.example/',
  BINDLE_SESSION_SECRET: key.toString('base64'),
};

describe('auth config', () => {
  it('is off when Cognito is not configured', () => {
    expect(authConfigFrom({})).toBeNull();
  });
  it('parses a full config', () => {
    const cfg = authConfigFrom(env)!;
    expect(cfg.consoleUrl).toBe('https://app.bindle.example');
    expect(cfg.sessionKey.equals(key)).toBe(true);
  });
  it('refuses partial or unsafe config', () => {
    for (const bad of [
      { ...env, BINDLE_COGNITO_CLIENT_ID: undefined },
      { ...env, BINDLE_COGNITO_DOMAIN: 'http://insecure.example' },
      { ...env, BINDLE_CONSOLE_URL: 'http://app.bindle.example' },
      { ...env, BINDLE_CONSOLE_URL: undefined },
      { ...env, BINDLE_SESSION_SECRET: 'short' },
      { ...env, BINDLE_SESSION_SECRET: undefined },
    ]) {
      expect(() => authConfigFrom(bad)).toThrow();
    }
    expect(authConfigFrom({ ...env, BINDLE_CONSOLE_URL: 'http://127.0.0.1:3000' })).not.toBeNull();
  });
});

describe('sealed cookies', () => {
  it('round-trips and rejects tampering, other keys and garbage', () => {
    const s = seal({ at: 'token', exp: 123 }, key);
    expect(unseal(s, key)).toEqual({ at: 'token', exp: 123 });
    expect(s).not.toContain('token');
    const raw = Buffer.from(s, 'base64url');
    raw.writeUInt8(raw.readUInt8(raw.length - 20) ^ 1, raw.length - 20);
    expect(unseal(raw.toString('base64url'), key)).toBeNull();
    expect(unseal(s, randomBytes(32))).toBeNull();
    expect(unseal('garbage', key)).toBeNull();
    expect(unseal(undefined, key)).toBeNull();
    expect(unseal('x'.repeat(7000), key)).toBeNull();
    expect(seal({ a: 1 }, key)).not.toBe(seal({ a: 1 }, key)); // random IV
  });
  it('treats sessions as invalid a minute before expiry', () => {
    expect(validSession({ at: 't', exp: 1000 }, 900)).toBe(true);
    expect(validSession({ at: 't', exp: 1000 }, 950)).toBe(false);
    expect(validSession({ at: '', exp: 5000 }, 900)).toBe(false);
    expect(validSession(null, 0)).toBe(false);
  });
});

describe('OAuth helpers', () => {
  const cfg = authConfigFrom(env) as AuthConfig;
  it('builds PKCE per RFC 7636 (S256)', () => {
    const { verifier, challenge } = pkce();
    expect(verifier.length).toBeGreaterThanOrEqual(43);
    expect(challenge).toBe(createHash('sha256').update(verifier).digest('base64url'));
  });
  it('builds authorize and logout URLs', () => {
    const u = new URL(authorizeUrl(cfg, 'st', 'ch'));
    expect(u.origin + u.pathname).toBe('https://bindle-test.auth.us-west-2.amazoncognito.com/oauth2/authorize');
    expect(Object.fromEntries(u.searchParams)).toMatchObject({
      response_type: 'code', client_id: 'client123', redirect_uri: 'https://app.bindle.example/auth/callback',
      state: 'st', code_challenge: 'ch', code_challenge_method: 'S256',
    });
    expect(new URL(logoutUrl(cfg)).searchParams.get('logout_uri')).toBe('https://app.bindle.example/');
  });
  it('only redirects to local paths after sign-in', () => {
    expect(safeNext('/runs?status=failed')).toBe('/runs?status=failed');
    for (const bad of ['https://evil.example', '//evil.example', '/\\evil.example', 'runs', '', null, '/' + 'a'.repeat(600)]) {
      expect(safeNext(bad)).toBe('/');
    }
  });
  it('compares state in constant time semantics', () => {
    expect(sameString('abc', 'abc')).toBe(true);
    expect(sameString('abc', 'abd')).toBe(false);
    expect(sameString('abc', 'abcd')).toBe(false);
  });
  it('checks request origins', () => {
    expect(originAllowed('https://app.bindle.example', cfg)).toBe(true);
    expect(originAllowed('https://evil.example', cfg)).toBe(false);
    expect(originAllowed('https://app.bindle.example.evil.example', cfg)).toBe(false);
    expect(originAllowed(null, cfg)).toBe(false);
    expect(originAllowed('not a url', cfg)).toBe(false);
  });
  it('exchanges the code with the PKCE verifier and client secret', async () => {
    const f = vi.fn(async () => new Response(JSON.stringify({ access_token: 'at', expires_in: 3600 })));
    const res = await exchangeCode({ ...cfg, clientSecret: 's3cret' }, 'the-code', 'the-verifier', f as unknown as typeof fetch);
    expect(res).toEqual({ accessToken: 'at', expiresIn: 3600 });
    const [url, init] = f.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe('https://bindle-test.auth.us-west-2.amazoncognito.com/oauth2/token');
    const body = new URLSearchParams(init.body as URLSearchParams);
    expect(Object.fromEntries(body)).toMatchObject({ grant_type: 'authorization_code', code: 'the-code', code_verifier: 'the-verifier' });
    expect((init.headers as Record<string, string>).authorization).toBe('Basic ' + Buffer.from('client123:s3cret').toString('base64'));
  });
  it('fails on token endpoint errors', async () => {
    const f = vi.fn(async () => new Response('{"error":"invalid_grant"}', { status: 400 }));
    await expect(exchangeCode(cfg, 'c', 'v', f as unknown as typeof fetch)).rejects.toThrow();
    const g = vi.fn(async () => new Response('{}'));
    await expect(exchangeCode(cfg, 'c', 'v', g as unknown as typeof fetch)).rejects.toThrow();
  });
});

describe('keys', () => {
  const base = { id: 'key_x', name: 'n', display_prefix: 'bd_live_x', created_at: '2026-11-16T00:00:00Z' };
  it('derives status', () => {
    const now = Date.parse('2026-11-16T12:00:00Z');
    expect(keyStatus(base, now)).toBe('active');
    expect(keyStatus({ ...base, revoked_at: '2026-11-16T01:00:00Z' }, now)).toBe('revoked');
    expect(keyStatus({ ...base, expires_at: '2026-11-17T00:00:00Z' }, now)).toBe('rotating');
    expect(keyStatus({ ...base, expires_at: '2026-11-16T11:00:00Z' }, now)).toBe('expired');
  });
  it('accepts only offered grace periods', () => {
    expect(validGrace('24')).toBe(24);
    expect(validGrace('0')).toBe(0);
    expect(validGrace('5')).toBeNull();
    expect(validGrace('-1')).toBeNull();
    expect(validGrace(null)).toBeNull();
    expect(validGrace('')).toBeNull();
    expect(validGrace(undefined)).toBeNull();
  });
});

import { formatPatterns, parsePatterns, parsePercent } from './sdksettings';

describe('sdk settings form', () => {
  it('parses NAME=regex lines', () => {
    const r = parsePatterns('# comment\norder_id = ord_\\d+\n\nACCT=a=b');
    expect(r.error).toBeUndefined();
    expect(r.patterns).toEqual([{ name: 'ORDER_ID', pattern: 'ord_\\d+' }, { name: 'ACCT', pattern: 'a=b' }]);
    expect(formatPatterns(r.patterns)).toBe('ORDER_ID=ord_\\d+\nACCT=a=b');
  });
  it('reports the bad line', () => {
    expect(parsePatterns('OK=x\nnope').error).toContain('Line 2');
    expect(parsePatterns('9BAD=x').error).toContain('Line 1');
    expect(parsePatterns('EMPTY=').error).toContain('empty');
  });
  it('converts percentages', () => {
    expect(parsePercent('25')).toBe(0.25);
    expect(parsePercent('0.5')).toBe(0.005);
    expect(parsePercent('100')).toBe(1);
    for (const bad of ['', '101', '-1', 'abc', null]) expect(parsePercent(bad)).toBeNull();
  });
});
