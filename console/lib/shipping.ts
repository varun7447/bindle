// Learning loop, phase 2: proofs, shipped releases, opt-in, kill switches and
// rollbacks. Everything is behind the learning_loop feature (404 otherwise).

import type { AttentionItem } from './insights';
import { noteHref } from './learning';

export interface ProofVerdict {
  total: number;
  fixed: number;
  fixed_rate: number;
  threshold: number;
  regressions: number;
  passed: boolean;
  reason: string;
}

export interface ProofReport {
  agent: string;
  path: string;
  sha256: string;
  lesson: string;
  runs: { trace_id: string; fixed: boolean; covered: boolean; detail?: string }[];
  eval_runs: number;
  regressions: number;
  gaps: string[];
}

export interface Proof {
  id: number;
  sha256: string;
  passed: boolean;
  verdict: ProofVerdict;
  report: ProofReport;
  uploaded_by: string;
  uploaded_at: string;
}

export interface Side {
  runs: number;
  successes: number;
  success_rate: number;
  recurred: number;
  recurrence: number;
  avg_tokens: number;
  avg_ms: number;
}

export interface Measurement {
  agent: string;
  path: string;
  release: number;
  with: Side;
  without: Side;
  decision: 'rollback' | 'keep' | 'too_few_runs';
  reason: string;
  computed_at: string;
}

export interface Release {
  path: string;
  release: number;
  sha256: string;
  state: 'active' | 'rolled_back' | 'withdrawn';
  state_reason?: string;
  state_at?: string;
  released_by: string;
  released_at: string;
  serving: boolean;
  measurement?: Measurement;
}

export interface ShipSettings {
  agent: string;
  ship: boolean;
  kill_all: boolean;
  killed: string[];
}

export interface Rollback {
  agent: string;
  path: string;
  release: number;
  reason: string;
  at: string;
}

const pct = (f: number) => `${Math.round(f * 100)}%`;

/** One sentence on how skills reach an agent right now. */
export function shippingSentence(agent: string, s: ShipSettings, releases: Release[]): string {
  const serving = releases.filter((r) => r.serving && !s.killed.includes(r.path));
  if (!s.ship) return `Approved skills don't reach ${agent || 'this agent'}: shipping is off (opt in below).`;
  if (s.kill_all) return `Every learned skill of ${agent || 'this agent'} is switched off; agents drop them within a minute.`;
  if (serving.length === 0) return `${agent || 'This agent'} opted in, but no approved skill is shipping yet.`;
  return `${serving.length === 1 ? '1 skill is' : `${serving.length} skills are`} shipping to ${agent || 'this agent'}.`;
}

/** A release's state in words. */
export function releaseState(r: Release, killed: string[]): string {
  if (r.state === 'rolled_back') return `Rolled back${r.state_reason ? `: ${r.state_reason}` : ''}`;
  if (r.state === 'withdrawn') return 'Withdrawn (the skill was retired)';
  if (killed.includes(r.path)) return 'Switched off';
  return r.serving ? 'Shipping' : 'Replaced by a newer release';
}

/** What the latest measurement says, in one line. */
export function measurementLine(m: Measurement | undefined): string {
  if (!m) return 'Not measured yet (hourly, once agents load it).';
  if (m.decision === 'too_few_runs') return `Measuring: ${m.with.runs} runs with it, ${m.without.runs} without (20 of each needed).`;
  const overhead =
    m.without.avg_tokens > 0 ? `; ${m.with.avg_tokens >= m.without.avg_tokens ? '+' : ''}${pct(m.with.avg_tokens / m.without.avg_tokens - 1)} tokens per run` : '';
  return `Success ${pct(m.with.success_rate)} with it, ${pct(m.without.success_rate)} without (${m.with.runs} and ${m.without.runs} runs)${overhead}.`;
}

/** The proof verdict in one line. */
export function proofLine(p: Proof): string {
  const gaps = p.report.gaps.length ? ` ${p.report.gaps.length} thing${p.report.gaps.length === 1 ? '' : 's'} replay couldn't cover.` : '';
  return `${p.passed ? '✓ Passed' : '✕ Failed'}: ${p.verdict.reason}.${gaps}`;
}

/** Home: releases Bindle rolled back because they made things worse. */
export function rollbackItems(rs: Rollback[]): AttentionItem[] {
  return rs.slice(0, 3).map((r) => ({
    key: `rollback:${r.agent}:${r.path}:${r.release}`,
    severity: 'warning' as const,
    title: `${r.agent || 'Unnamed agent'}: skill ${r.path.replace(/^skills\//, '').replace(/\/SKILL\.md$/, '')} release ${r.release} was rolled back`,
    detail: `${r.reason}. The release before it is shipping again.`,
    href: noteHref(r.agent, r.path),
    cta: 'Open the skill',
  }));
}

/** "skills/retry-refund/SKILL.md@2" -> { name: "retry-refund", release: 2, path }. */
export function parseSkillRef(ref: string): { path: string; name: string; release: number } | null {
  const m = /^(skills\/([a-z0-9]+(?:-[a-z0-9]+)*)\/SKILL\.md)@(\d+)$/.exec(ref);
  return m ? { path: m[1]!, name: m[2]!, release: Number(m[3]) } : null;
}

/** A run-summary line naming the learned skills the run loaded (none: null). */
export function skillsLine(steps: { step_id: string; skills_used?: string[] }[]): { tone: 'neutral'; text: string; stepId: string } | null {
  const seen = new Map<string, string>();
  let first = '';
  for (const st of steps) {
    for (const ref of st.skills_used ?? []) {
      const p = parseSkillRef(ref);
      if (!p) continue;
      if (!first) first = st.step_id;
      seen.set(ref, `${p.name} (release ${p.release})`);
    }
  }
  if (seen.size === 0) return null;
  const names = [...seen.values()];
  return { tone: 'neutral', text: `It loaded ${names.length === 1 ? 'a learned skill' : `${names.length} learned skills`}: ${names.join(', ')}.`, stepId: first };
}
