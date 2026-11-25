// Learned skills: approved SKILL.md files Bindle ships to an agent.
//
// init() starts a background fetch of the skills shipped to this agent (and
// version) from GET {endpoint}/v1/skills. It is time-boxed (2 s), cached on
// disk with an ETag, and silent on failure: if Bindle is down the agent keeps
// the last cached skills, or runs without any. Nothing here throws into the
// agent.
//
//   for (const s of bindle.skills.relevant(taskText)) prompt += bindle.skills.load(s);
//
// load() records a "bindle.skill.used" span event (path and release) on the
// current step, so runs show which skills were loaded. The console's kill
// switches arrive with the SDK config, within one config poll.

import { createHash } from 'node:crypto';
import { mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { trace } from '@opentelemetry/api';

export const FETCH_TIMEOUT_MS = 2000;
export const REFRESH_MS = 300_000;
export const IDLE_REFRESH_MS = 3_600_000;
export const USED_EVENT = 'bindle.skill.used';
const MAX_SKILLS = 100;
const MAX_BODY = 64 * 1024;
const STOP = new Set(
  `a an and are as at be but by can do for from has have how i if in into is it its me my not of on or our should so than that
the their then there these this to use used uses using was we what when where which while who why will with you your`.split(/\s+/),
);

export interface Skill {
  name: string;
  description: string;
  path: string;
  release: number;
  sha256: string;
  /** The full SKILL.md; prefer load(), which also records the use. */
  body: string;
  agentVersions: string[];
}

type FetchFn = (url: string, init: { headers: Record<string, string>; signal: AbortSignal }) => Promise<{
  status: number;
  headers: { get(name: string): string | null };
  text(): Promise<string>;
}>;

function parse(doc: unknown): Skill[] {
  const out: Skill[] = [];
  const list = (doc as { skills?: unknown })?.skills;
  for (const s of Array.isArray(list) ? list.slice(0, MAX_SKILLS) : []) {
    const r = s as Record<string, unknown>;
    if (typeof r.name !== 'string' || typeof r.path !== 'string' || typeof r.body !== 'string' || !Number.isInteger(r.release)) continue;
    if (r.body.length > MAX_BODY) continue;
    out.push({
      name: r.name,
      description: typeof r.description === 'string' ? r.description : '',
      path: r.path,
      release: r.release as number,
      sha256: typeof r.sha256 === 'string' ? r.sha256 : '',
      body: r.body,
      agentVersions: Array.isArray(r.agent_versions) ? r.agent_versions.filter((v): v is string => typeof v === 'string') : [],
    });
  }
  return out;
}

function cacheDir(): string {
  return process.env.BINDLE_CACHE_DIR || join(homedir(), '.cache', 'bindle');
}

export interface SkillStoreOptions {
  endpoint: string;
  apiKey: string;
  agent: string;
  version?: string;
  killed?: () => readonly string[];
  userAgent?: string;
  fetch?: FetchFn;
  cacheDir?: string;
}

/** The skills shipped to one agent, refreshed in the background. */
export class SkillStore {
  private readonly url: string;
  private readonly file: string;
  private skills: Skill[] = [];
  private etag: string | undefined;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private stopped = false;

  constructor(private readonly o: SkillStoreOptions) {
    const q = new URLSearchParams({ agent: o.agent, version: o.version ?? '' });
    this.url = `${o.endpoint.replace(/\/+$/, '')}/v1/skills?${q}`;
    const digest = createHash('sha256').update(`${o.endpoint}\x00${o.agent}\x00${o.version ?? ''}`).digest('hex').slice(0, 24);
    this.file = join(o.cacheDir ?? cacheDir(), `skills-${digest}.json`);
    this.loadCache();
  }

  private loadCache(): void {
    try {
      const doc = JSON.parse(readFileSync(this.file, 'utf8')) as { etag?: string };
      this.skills = parse(doc);
      this.etag = typeof doc.etag === 'string' ? doc.etag : undefined;
    } catch {
      // no or unreadable cache: start empty
    }
  }

  private saveCache(doc: Record<string, unknown>): void {
    try {
      mkdirSync(dirname(this.file), { recursive: true, mode: 0o700 });
      const tmp = `${this.file}.${process.pid}.tmp`;
      writeFileSync(tmp, JSON.stringify(doc), { mode: 0o600 });
      renameSync(tmp, this.file);
    } catch {
      // the cache is an optimization
    }
  }

  /** Fetches once; resolves to the delay before the next fetch. Never rejects. */
  async refresh(): Promise<number> {
    try {
      const headers: Record<string, string> = { authorization: `Bearer ${this.o.apiKey}`, 'user-agent': this.o.userAgent ?? 'bindle-sdk-typescript' };
      if (this.etag) headers['if-none-match'] = this.etag;
      const f = this.o.fetch ?? (globalThis.fetch as unknown as FetchFn);
      const res = await f(this.url, { headers, signal: AbortSignal.timeout(FETCH_TIMEOUT_MS) });
      if (res.status === 304) return REFRESH_MS;
      if (res.status === 404) {
        // The workspace doesn't ship skills: none, and ask rarely.
        this.skills = [];
        this.etag = undefined;
        this.saveCache({ skills: [] });
        return IDLE_REFRESH_MS;
      }
      if (res.status !== 200) return REFRESH_MS;
      const doc = JSON.parse(await res.text()) as Record<string, unknown>;
      doc.etag = res.headers.get('etag') ?? undefined;
      this.skills = parse(doc);
      this.etag = doc.etag as string | undefined;
      this.saveCache(doc);
    } catch {
      // Bindle down or slow: keep the last skills
    }
    return REFRESH_MS;
  }

  start(): void {
    const loop = async (): Promise<void> => {
      const wait = await this.refresh();
      if (this.stopped) return;
      this.timer = setTimeout(() => void loop(), wait);
      this.timer.unref?.();
    };
    void loop();
  }

  stop(): void {
    this.stopped = true;
    if (this.timer) clearTimeout(this.timer);
  }

  /** Every shipped skill the kill switches allow. */
  list(): Skill[] {
    let killed: Set<string>;
    try {
      killed = new Set(this.o.killed?.() ?? []);
    } catch {
      killed = new Set();
    }
    if (killed.has(`${this.o.agent}/*`)) return [];
    return this.skills.filter((s) => !killed.has(`${this.o.agent}/${s.path}`));
  }
}

let store: SkillStore | undefined;

/** @internal */
export function setStore(s: SkillStore | undefined): void {
  store?.stop();
  store = s;
}

function words(text: string): Set<string> {
  return new Set((text.toLowerCase().match(/[a-z0-9]+/g) ?? []).filter((w) => w.length > 2 && !STOP.has(w)));
}

/** The skills shipped to this agent (none before init or when off). */
export function list(): Skill[] {
  try {
    return store?.list() ?? [];
  } catch {
    return [];
  }
}

/**
 * Skills whose name and description share words with the task, best match
 * first. Only names and descriptions are compared (agentskills.io
 * progressive disclosure); call load() for the ones you use.
 */
export function relevant(task: string, limit = 3): Skill[] {
  try {
    const want = words(task ?? '');
    return list()
      .map((s) => ({ s, n: [...words(`${s.name.replace(/-/g, ' ')} ${s.description}`)].filter((w) => want.has(w)).length }))
      .filter((x) => x.n > 0)
      .sort((a, b) => b.n - a.n || a.s.name.localeCompare(b.s.name))
      .slice(0, Math.max(limit, 0))
      .map((x) => x.s);
  } catch {
    return [];
  }
}

/** A shipped skill by name (undefined if not shipped or switched off). */
export function get(name: string): Skill | undefined {
  return list().find((s) => s.name === name);
}

/**
 * The skill's full SKILL.md text ('' if it isn't shipped or was switched
 * off), recording a "bindle.skill.used" event on the current step.
 */
export function load(skill: Skill | string): string {
  try {
    const name = typeof skill === 'string' ? skill : skill?.name;
    const s = get(name);
    if (!s) return '';
    const span = trace.getActiveSpan();
    if (span?.isRecording()) {
      span.addEvent(USED_EVENT, { 'bindle.skill.name': s.name, 'bindle.skill.path': s.path, 'bindle.skill.release': s.release });
    }
    return s.body;
  } catch {
    return '';
  }
}
