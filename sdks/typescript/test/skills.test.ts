import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { InMemorySpanExporter } from '@opentelemetry/sdk-trace';
import { afterEach, describe, expect, it } from 'vitest';
import * as bindle from '../src/index.js';
import * as skills from '../src/skills.js';
import { parseSettings } from '../src/remote.js';

process.env.BINDLE_SKILLS = 'off'; // no background fetches from init() in tests

const SKILL = '---\nname: retry-refund\ndescription: Retry a refund once when the payments API times out.\n---\n\nRetry once.\n';
const DOC = {
  agent: 'refunds',
  skills: [
    { name: 'retry-refund', description: 'Retry a refund once when the payments API times out.', path: 'skills/retry-refund/SKILL.md', release: 2, sha256: 's1', body: SKILL },
    { name: 'check-address', description: 'Validate a shipping address before creating a label.', path: 'skills/check-address/SKILL.md', release: 1, sha256: 's2', body: 'x' },
  ],
};

function fakeFetch() {
  const f = {
    mode: 'ok' as 'ok' | 'down' | 'slow' | 304 | 404 | 500,
    calls: [] as { ifNoneMatch?: string; aborted: boolean }[],
    fetch: async (_url: string, init: { headers: Record<string, string>; signal: AbortSignal }) => {
      f.calls.push({ ifNoneMatch: init.headers['if-none-match'], aborted: init.signal.aborted });
      if (f.mode === 'down') throw new Error('ECONNREFUSED');
      if (f.mode === 'slow') {
        await new Promise((_, reject) => init.signal.addEventListener('abort', () => reject(new Error('aborted'))));
      }
      const status = f.mode === 'ok' ? 200 : (f.mode as number);
      return { status, headers: { get: () => '"e1"' }, text: async () => JSON.stringify(DOC) };
    },
  };
  return f;
}

function store(dir: string, f: ReturnType<typeof fakeFetch>, killed: string[] = []) {
  return new skills.SkillStore({ endpoint: 'http://localhost:4318', apiKey: 'bd_test_x', agent: 'refunds', version: 'v12', killed: () => killed, fetch: f.fetch, cacheDir: dir });
}

afterEach(async () => {
  skills.setStore(undefined);
  await bindle.shutdown();
});

describe('learned skills', () => {
  it('fetches, caches on disk and keeps the last skills when Bindle is down', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'bindle-skills-'));
    const f = fakeFetch();
    const s = store(dir, f);
    expect(s.list()).toEqual([]);
    expect(await s.refresh()).toBe(skills.REFRESH_MS);
    expect(s.list().map((x) => x.name)).toEqual(['retry-refund', 'check-address']);
    await s.refresh();
    expect(f.calls[1]!.ifNoneMatch).toBe('"e1"');

    f.mode = 'down';
    await expect(s.refresh()).resolves.toBe(skills.REFRESH_MS);
    expect(s.list()).toHaveLength(2);
    f.mode = 500;
    await s.refresh();
    expect(s.list()).toHaveLength(2);
    expect(store(dir, f).list().map((x) => x.release)).toEqual([2, 1]); // a new process starts from the cache

    f.mode = 404;
    expect(await s.refresh()).toBe(skills.IDLE_REFRESH_MS);
    expect(s.list()).toEqual([]);
    expect(store(dir, f).list()).toEqual([]);
  });

  it('gives up after the time box', async () => {
    const f = fakeFetch();
    f.mode = 'slow';
    const s = store(mkdtempSync(join(tmpdir(), 'bindle-skills-')), f);
    const t0 = Date.now();
    await expect(s.refresh()).resolves.toBe(skills.REFRESH_MS);
    expect(Date.now() - t0).toBeLessThan(skills.FETCH_TIMEOUT_MS + 1500);
  }, 10_000);

  it('honors the kill switches', async () => {
    const killed = ['refunds/skills/retry-refund/SKILL.md'];
    const s = store(mkdtempSync(join(tmpdir(), 'bindle-skills-')), fakeFetch(), killed);
    await s.refresh();
    expect(s.list().map((x) => x.name)).toEqual(['check-address']);
    killed.push('refunds/*');
    expect(s.list()).toEqual([]);
    expect(parseSettings(JSON.stringify({ sample_rate: 1, capture_content: true, killed_skills: ['refunds/*', 3, 'bad'] }))?.killedSkills).toEqual(['refunds/*']);
  });

  it('picks relevant skills and records each load on the current step', async () => {
    const s = store(mkdtempSync(join(tmpdir(), 'bindle-skills-')), fakeFetch());
    await s.refresh();
    skills.setStore(s);
    expect(skills.relevant('The refund call to the payments API timed out').map((x) => x.name)).toEqual(['retry-refund']);
    expect(skills.relevant('nothing in common here')).toEqual([]);

    const mem = new InMemorySpanExporter();
    expect(bindle.init({ apiKey: 'bd_test_' + 'a'.repeat(40), endpoint: 'http://localhost:4318', agent: 'refunds', remoteConfig: false }, { exporter: () => mem })).toBe(true);
    skills.setStore(s);
    bindle.step('plan', { type: 'llm_call' }, () => {
      expect(skills.load('retry-refund')).toBe(SKILL);
      expect(skills.load('not-shipped')).toBe('');
    });
    await bindle.flush();
    const plan = mem.getFinishedSpans().find((x) => x.name === 'plan')!;
    const ev = plan.events;
    expect(ev).toHaveLength(1);
    expect(ev[0]!.name).toBe(skills.USED_EVENT);
    expect(ev[0]!.attributes).toMatchObject({ 'bindle.skill.path': 'skills/retry-refund/SKILL.md', 'bindle.skill.release': 2 });
  });

  it('never throws into the agent', async () => {
    const s = new skills.SkillStore({
      endpoint: 'http://localhost:4318', apiKey: 'k', agent: 'refunds',
      killed: () => { throw new Error('boom'); },
      fetch: () => { throw new Error('boom'); },
      cacheDir: join(mkdtempSync(join(tmpdir(), 'bindle-skills-')), 'x'),
    });
    await expect(s.refresh()).resolves.toBe(skills.REFRESH_MS);
    skills.setStore(s);
    expect(skills.list()).toEqual([]);
    expect(skills.relevant(undefined as unknown as string)).toEqual([]);
    expect(skills.load(undefined as unknown as string)).toBe('');
  });
});
