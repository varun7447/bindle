// Learning loop (feature learning_loop): lessons from repeated failures,
// skills drafted from them, and each agent's knowledge vault. Pure helpers
// for the console pages; the API enforces every rule.

import type { AttentionItem } from './insights';

export interface Lesson {
  id: string;
  agent: string;
  step_name: string;
  tool?: string;
  error_pattern: string;
  cause: string;
  title: string;
  runs: number;
  failures: number;
  last_seen: string;
  evidence: string[];
}

export interface LessonsResponse {
  lessons: Lesson[];
  min_runs: number;
}

export type NoteKind = 'skill' | 'lesson' | 'incident' | 'note';
export type SkillStatus = 'draft' | 'proven' | 'approved' | 'live' | 'retired';

export interface Scan {
  flagged: boolean;
  reasons: string[] | null;
  redacted?: boolean;
}

export interface PassportCheck {
  passport: boolean;
  ok: boolean;
  tools?: string[];
  classes?: string[];
  bad_tools?: string[];
  bad_classes?: string[];
  reason?: string;
}

export interface VaultNote {
  agent: string;
  path: string;
  kind: NoteKind;
  status?: SkillStatus;
  version: number;
  sha256: string;
  content?: string;
  scan: Scan | Record<string, never>;
  passport_check: PassportCheck | Record<string, never>;
  reviewed_sha256?: string;
  reviewed_by?: string;
  updated_by: string;
  updated_at: string;
  links?: string[];
  backlinks?: string[];
  versions?: { version: number; status?: string; action: string; sha256: string; edited_by: string; edited_at: string }[];
}

export interface VaultLink {
  path: string;
  target: string;
}

export interface VaultResponse {
  agent: string;
  notes: VaultNote[];
  links: VaultLink[];
  passport: boolean;
  owner: string;
}

export interface Reviewers {
  reviewers: { user_id: string; email: string; added_at: string }[];
  me: boolean;
  can_manage: boolean;
}

export const learningOn = (features: string[]): boolean => features.includes('learning_loop');

const CAUSE: Record<string, string> = {
  bad_arguments: 'rejected arguments',
  timeout: 'timeouts',
  rate_limited: 'rate limits',
  not_found: 'not found',
  permission: 'permissions',
  unavailable: 'service unavailable',
  error: 'errors',
};

export const causeLabel = (c: string): string => CAUSE[c] ?? c;

export const STATUS_LABEL: Record<SkillStatus, string> = {
  draft: 'Draft',
  proven: 'Proven',
  approved: 'Approved',
  live: 'Live',
  retired: 'Retired',
};

const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? '' : 's'}`;

/** The page's opening sentence: what the agent should learn, and what's waiting on a person. */
export function learningSentence(agent: string, lessons: Lesson[], notes: VaultNote[]): string {
  const skills = notes.filter((n) => n.kind === 'skill');
  const waiting = skills.filter((n) => n.status === 'proven').length;
  const flagged = skills.filter((n) => isFlagged(n) && n.status !== 'retired').length;
  const name = agent || 'This agent';
  if (lessons.length === 0 && skills.length === 0) return `${name} has no repeated failures to learn from yet.`;
  const parts: string[] = [];
  if (lessons.length > 0) {
    const top = lessons[0]!;
    parts.push(`${plural(lessons.length, 'lesson')} from repeated failures (most often: ${top.tool || top.step_name}, ${plural(top.runs, 'run')})`);
  }
  if (skills.length > 0) {
    const extra: string[] = [];
    if (waiting) extra.push(`${waiting} waiting for approval`);
    if (flagged) extra.push(`${flagged} flagged by the security scan`);
    parts.push(`${plural(skills.length, 'skill')}${extra.length ? ` (${extra.join(', ')})` : ''}`);
  }
  return `${name}: ${parts.join('; ')}. Nothing reaches the agent in this phase.`;
}

export function isFlagged(n: Pick<VaultNote, 'scan'>): boolean {
  return Boolean((n.scan as Scan).flagged);
}

export function checkOf(n: Pick<VaultNote, 'passport_check'>): PassportCheck | undefined {
  const c = n.passport_check as PassportCheck;
  return c && typeof c.ok === 'boolean' ? c : undefined;
}

/** What a person can do next with a skill, with the reason when blocked. */
export function nextStep(n: VaultNote, owner: string, hasPassport: boolean): string {
  if (n.kind !== 'skill') return '';
  switch (n.status) {
    case 'draft':
      return 'Prove it by replaying the lesson’s failures in your environment (bindle skills prove); a passing proof marks it proven.';
    case 'proven':
      if (!hasPassport) return 'The agent needs a passport before skills can be approved.';
      if (isFlagged(n) && n.reviewed_sha256 !== n.sha256) return 'A security reviewer must clear the flagged scan first.';
      return `Waiting for ${owner || 'the agent’s owner'} to approve.`;
    case 'approved':
      return 'Approved: it ships to this agent once the agent opts in to learned skills (Learning → Shipping).';
    case 'live':
      return 'Live: agents are loading it. Bindle measures each release hourly and rolls a worse one back.';
    case 'retired':
      return 'Retired. Restore it to start again as a draft.';
    default:
      return '';
  }
}

/** Human labels for scan reasons ("prompt_injection:ignore_instructions"). */
export function scanReason(r: string): string {
  const [flag, why] = r.split(':');
  const what: Record<string, string> = {
    ignore_instructions: 'tells the model to ignore its instructions',
    reveal_system_prompt: 'asks to reveal the system prompt',
    conceal_from_user: 'asks to hide something from the user',
    role_override: 'tries to override the model’s role',
    chat_markup: 'contains chat control markup',
    hidden_text: 'contains invisible text',
    credential_in_skill: 'contained a credential (removed before storage)',
    credential_in_output: 'contained a credential (removed before storage)',
  };
  return what[why ?? ''] ?? `${flag}: ${why ?? ''}`;
}

// ---- graph ----------------------------------------------------------------

export interface GraphNode {
  id: string;
  label: string;
  kind: NoteKind | 'run' | 'tool' | 'agent' | 'missing';
  x: number;
  y: number;
}

export interface Graph {
  nodes: GraphNode[];
  edges: { from: string; to: string }[];
  width: number;
  height: number;
}

function labelOf(id: string): string {
  if (id.startsWith('run:')) return `run ${id.slice(4, 12)}`;
  if (id.startsWith('tool:') || id.startsWith('agent:')) return id.slice(id.indexOf(':') + 1);
  const parts = id.replace(/\.md$/, '').split('/');
  return parts[0] === 'skills' && parts.length >= 2 ? parts[1]! : parts[parts.length - 1]!;
}

function kindOf(id: string, notes: Map<string, VaultNote>): GraphNode['kind'] {
  if (id.startsWith('run:')) return 'run';
  if (id.startsWith('tool:')) return 'tool';
  if (id.startsWith('agent:')) return 'agent';
  return notes.get(id)?.kind ?? 'missing';
}

/** A deterministic circular layout: notes first (sorted), then link targets. At most 60 nodes. */
export function vaultGraph(notes: VaultNote[], links: VaultLink[], size = 520): Graph {
  const byPath = new Map(notes.map((n) => [n.path, n]));
  const ids: string[] = [...notes.map((n) => n.path).sort()];
  for (const l of [...links].sort((a, b) => (a.target < b.target ? -1 : 1))) {
    if (!ids.includes(l.target)) ids.push(l.target);
  }
  const shown = ids.slice(0, 60);
  const r = size / 2 - 70;
  const c = size / 2;
  const nodes = shown.map((id, i) => {
    const a = (2 * Math.PI * i) / Math.max(shown.length, 1) - Math.PI / 2;
    return {
      id,
      label: labelOf(id),
      kind: kindOf(id, byPath),
      x: Math.round(c + r * Math.cos(a)),
      y: Math.round(c + r * Math.sin(a)),
    };
  });
  const set = new Set(shown);
  const edges = links.filter((l) => set.has(l.path) && set.has(l.target)).map((l) => ({ from: l.path, to: l.target }));
  return { nodes, edges, width: size, height: size };
}

/** Where a vault link target opens in the console. */
export function targetHref(target: string, agent: string): string {
  if (target.startsWith('run:')) return `/traces/${target.slice(4)}`;
  if (target.startsWith('tool:')) return `/agents?${new URLSearchParams({ agent }).toString()}`;
  if (target.startsWith('agent:')) return `/agents?${new URLSearchParams({ agent: target.slice(6) }).toString()}`;
  return noteHref(agent, target);
}

export function noteHref(agent: string, path: string): string {
  return `/agents/learning/note?${new URLSearchParams({ agent, path }).toString()}`;
}

/** Home attention items: agents with lessons waiting to be turned into skills. */
export function lessonItems(lessons: Lesson[]): AttentionItem[] {
  const byAgent = new Map<string, Lesson[]>();
  for (const l of lessons) byAgent.set(l.agent, [...(byAgent.get(l.agent) ?? []), l]);
  return [...byAgent.entries()].slice(0, 3).map(([agent, ls]) => ({
    key: `lessons:${agent}`,
    severity: 'info' as const,
    title: `${agent || 'Unnamed agent'}: ${plural(ls.length, 'lesson')} from repeated failures`,
    detail: ls[0]!.title,
    href: `/agents/learning?${new URLSearchParams({ agent }).toString()}`,
    cta: 'Review lessons',
  }));
}
