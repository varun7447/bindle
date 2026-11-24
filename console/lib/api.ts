// Server-side client for bindle-api. Only import from Server Components and
// route handlers: the console calls the API from its own server, so the API
// needs no CORS and credentials never reach the browser.
//
// The console holds no credentials of its own and never chooses a tenant.
// It forwards the signed-in user's Cognito access token; bindle-api verifies
// it and resolves the tenant. Without Cognito configured (local dev), no
// token is sent and the API's dev bypass, if enabled, applies.

import { sessionToken } from './auth';
import type { SecurityResponse } from './security';
import type { AgentPassportResponse, DriftResponse, MgmtKey, PassportVersion } from './passports';
import type { TopicsResponse } from './topics';
import type { GradingResponse, OutcomeEndpoint, OutcomeEvent, OutcomeEventStatus, OutcomeSettings } from './grading';
import type { AgentLimit, LimitsResponse } from './limits';
import type { AWSPrice, ModelUsage, PriceOverride, PricesResponse } from './prices';
import type { ToolHealth, VersionStats, VersionToolUsage } from './agents';
import type { ReplayMessage, ReplayResponse } from './replay';
import type { AgentChecks, Dataset, DatasetItem, EvalRun, Scorer } from './evals';
import { type Economics, type ProblemSource, type SuggestionsResponse, problemParams } from './outcomes';
import type { Session, SessionDetail } from './sessions';
import type { SearchHit } from './search';
import type { TraceLink } from './links';
import type { LessonsResponse, Reviewers, VaultNote, VaultResponse } from './learning';
import type { Proof, Release, Rollback, ShipSettings } from './shipping';
import type {
  APIKey,
  AlertsResponse,
  CreatedKey,
  Insights,
  Me,
  OverviewResponse,
  RunsResponse,
  StepResponse,
  TraceResponse,
} from './types';
import { isStepId, isTraceId } from './ids';
import type { SDKConfig } from './sdksettings';
import type { AlertChannel, AlertEvent, AlertRule, ChannelKind, RuleKind } from './alerting';
import type { LedgerEntry, LedgerResult } from './ledger';

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

type Params = Record<string, string | undefined>;

export function apiBase(): string {
  return (process.env.BINDLE_API_URL ?? 'http://127.0.0.1:8080').replace(/\/+$/, '');
}

/** Builds an API URL. Undefined/empty params are dropped. */
export function buildUrl(base: string, path: string, params: Params = {}): string {
  const url = new URL(base + path);
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '') url.searchParams.set(k, v);
  }
  return url.toString();
}

export async function apiGet<T>(path: string, params: Params = {}, fetchImpl: typeof fetch = fetch, token?: string | null): Promise<T> {
  return apiRequest<T>('GET', buildUrl(apiBase(), path, params), undefined, fetchImpl, token);
}

export async function apiSend<T>(method: 'POST' | 'PUT' | 'DELETE', path: string, body: object | undefined, fetchImpl: typeof fetch = fetch, token?: string | null): Promise<T> {
  return apiRequest<T>(method, buildUrl(apiBase(), path), body, fetchImpl, token);
}

async function apiRequest<T>(method: string, url: string, body: object | undefined, fetchImpl: typeof fetch, token?: string | null): Promise<T> {
  const headers: Record<string, string> = { accept: 'application/json' };
  if (token) headers.authorization = `Bearer ${token}`;
  if (body) headers['content-type'] = 'application/json';
  let res: Response;
  try {
    res = await fetchImpl(url, { method, cache: 'no-store', headers, body: body ? JSON.stringify(body) : undefined });
  } catch {
    throw new ApiError(0, 'unreachable', `Can't reach the Bindle API at ${apiBase()}`);
  }
  if (!res.ok) {
    let code = 'error';
    let message = `Bindle API returned HTTP ${res.status}`;
    try {
      const body = (await res.json()) as { error?: { code?: string; message?: string } };
      code = body.error?.code ?? code;
      message = body.error?.message ?? message;
    } catch {
      // non-JSON error body: keep the generic message
    }
    throw new ApiError(res.status, code, message);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

const KEY_ID = /^key_[0-9a-f]{20}$/;
const MGMT_KEY_ID = /^mkey_[0-9a-f]{20}$/;

// Every call carries the current user's session token (if any).
const get = async <T>(path: string, params: Params = {}) => apiGet<T>(path, params, fetch, await sessionToken());
const post = async <T>(path: string, body: object) => apiSend<T>('POST', path, body, fetch, await sessionToken());
const put = async <T>(path: string, body: object) => apiSend<T>('PUT', path, body, fetch, await sessionToken());
const del = async (path: string) => apiSend<void>('DELETE', path, undefined, fetch, await sessionToken());

/** Whether a run was stopped from the console (GET/POST /api/traces/{id}/stop). */
export interface RunStop {
  stopped: boolean;
  stopped_at?: string;
  /** How often the SDK refreshes its settings, so how long a stop can take to arrive. */
  poll_seconds: number;
}

const RULE_ID = /^ar_[0-9a-f]{20}$/;
const SCORER_ID = /^sc_[0-9a-f]{20}$/;
const DATASET_ID = /^ds_[0-9a-f]{20}$/;
const CHANNEL_ID = /^ch_[0-9a-f]{20}$/;

const OUTCOME_ENDPOINT_ID = /^oep_[0-9a-f]{16}$/;

function checked(id: string, re: RegExp, what: string): string {
  if (!re.test(id)) throw new ApiError(400, 'bad_request', `Invalid ${what} ID`);
  return id;
}

function keyPath(id: string, action: 'rotate' | 'revoke'): string {
  if (!KEY_ID.test(id)) throw new ApiError(400, 'bad_request', 'Invalid key ID');
  return `/api/keys/${id}/${action}`;
}

export const api = {
  runs: (p: { from?: string; to?: string; status?: string; agent?: string; version?: string; environment?: string; limit?: string; cursor?: string }) =>
    get<RunsResponse>('/api/runs', p),
  trace: async (id: string) => {
    if (!isTraceId(id)) throw new ApiError(400, 'bad_request', 'Invalid trace ID');
    return get<TraceResponse>(`/api/traces/${id}`);
  },
  traceLinks: async (id: string) => {
    if (!isTraceId(id)) throw new ApiError(400, 'bad_request', 'Invalid trace ID');
    return get<{ links: TraceLink[] }>(`/api/traces/${id}/links`);
  },
  runStop: async (id: string) => {
    if (!isTraceId(id)) throw new ApiError(400, 'bad_request', 'Invalid trace ID');
    return get<RunStop>(`/api/traces/${id}/stop`);
  },
  stopRun: async (id: string) => {
    if (!isTraceId(id)) throw new ApiError(400, 'bad_request', 'Invalid trace ID');
    return post<RunStop>(`/api/traces/${id}/stop`, {});
  },
  step: async (traceId: string, stepId: string) => {
    if (!isTraceId(traceId) || !isStepId(stepId)) throw new ApiError(400, 'bad_request', 'Invalid step ID');
    return get<StepResponse>(`/api/traces/${traceId}/steps/${stepId}`);
  },
  overview: (window: string) => get<OverviewResponse>('/api/overview', { window }),
  alerts: (window: string) => get<AlertsResponse>('/api/alerts', { window }),
  insights: (window: string) => get<Insights>('/api/insights', { window }),
  me: () => get<Me>('/api/me'),
  keys: () => get<{ keys: APIKey[] }>('/api/keys'),
  createKey: (name: string) => post<CreatedKey>('/api/keys', { name }),
  rotateKey: async (id: string, graceHours: number) => post<CreatedKey>(keyPath(id, 'rotate'), { grace_hours: graceHours }),
  revokeKey: async (id: string) => post<void>(keyPath(id, 'revoke'), {}),
  config: () => get<SDKConfig>('/api/config'),
  saveConfig: (c: Omit<SDKConfig, 'updated_at' | 'poll_interval_seconds'>) => put<SDKConfig>('/api/config', c),
  limits: () => get<LimitsResponse>('/api/limits'),
  saveLimits: (version: number, limits: AgentLimit[]) => put<LimitsResponse>('/api/limits', { version, limits }),
  prices: () => get<PricesResponse>('/api/prices'),
  savePrices: (version: number, prices: PriceOverride[]) => put<PricesResponse>('/api/prices', { version, prices }),
  awsPrices: (region: string) => get<{ region: string; prices: AWSPrice[] }>('/api/prices/aws', { region }),
  models: (window: string) => get<{ window: string; models: ModelUsage[] }>('/api/models', { window }),
  tools: (window: string) => get<{ window: string; tools: ToolHealth[] }>('/api/tools', { window }),
  agentVersions: (agent: string, window: string) => get<{ window: string; agent: string; versions: VersionStats[] }>('/api/agents/versions', { agent, window }),
  features: () => get<{ features: string[] }>('/api/features'),
  // Passports and management keys (behind the passports flag).
  mgmtKeys: () => get<{ keys: MgmtKey[]; scopes: string[] }>('/api/mgmt-keys'),
  createMgmtKey: (name: string, scopes: string[]) => post<{ key: MgmtKey; secret: string }>('/api/mgmt-keys', { name, scopes }),
  revokeMgmtKey: async (id: string) => post<void>(`/api/mgmt-keys/${checked(id, MGMT_KEY_ID, 'key')}/revoke`, {}),
  passports: () => get<{ passports: PassportVersion[] }>('/api/passports'),
  agentPassport: (agent: string) => get<AgentPassportResponse>('/api/agents/passport', { agent }),
  passportDrift: (window: string) => get<DriftResponse>('/api/passport-drift', { window }),
  ledger: (agent: string, days: number, beforeSeq?: number) =>
    get<{ agent: string; from: string; to: string; entries: LedgerEntry[]; next_before_seq?: number }>('/api/agents/ledger', {
      agent,
      days: String(days),
      ...(beforeSeq ? { before_seq: String(beforeSeq) } : {}),
    }),
  ledgerVerify: (agent: string, days: number) => get<{ agent: string; result: LedgerResult }>('/api/agents/ledger/verify', { agent, days: String(days) }),
  toolUsage: (agent: string, window: string) => get<{ window: string; agent: string; versions: VersionToolUsage[] }>('/api/agents/tool-usage', { agent, window }),
  agentTopics: (agent: string, window: string) => get<TopicsResponse>('/api/agents/topics', { agent, window }),
  security: (window: string) => get<SecurityResponse>('/api/security', { window }),
  replay: async (traceId: string, stepId: string, body: { provider: string; model: string; api_key: string; messages: ReplayMessage[]; max_tokens: number }) => {
    if (!isTraceId(traceId) || !isStepId(stepId)) throw new ApiError(400, 'bad_request', 'Invalid step ID');
    return post<ReplayResponse>(`/api/traces/${traceId}/steps/${stepId}/replay`, body);
  },
  scorers: () => get<{ scorers: Scorer[] }>('/api/scorers'),
  createScorer: (s: Omit<Scorer, 'id' | 'created_at'>) => post<Scorer>('/api/scorers', s),
  updateScorer: async (id: string, s: Omit<Scorer, 'id' | 'created_at' | 'kind'>) => put<Scorer>(`/api/scorers/${checked(id, SCORER_ID, 'check')}`, s),
  deleteScorer: async (id: string) => del(`/api/scorers/${checked(id, SCORER_ID, 'check')}`),
  datasets: () => get<{ datasets: Dataset[] }>('/api/datasets'),
  createDataset: (name: string, description: string) => post<Dataset>('/api/datasets', { name, description }),
  dataset: async (id: string) => get<{ dataset: Dataset; items: DatasetItem[] }>(`/api/datasets/${checked(id, DATASET_ID, 'dataset')}`),
  deleteDataset: async (id: string) => del(`/api/datasets/${checked(id, DATASET_ID, 'dataset')}`),
  addDatasetItems: async (id: string, traceIds: string[]) =>
    post<{ added: number; found: number; requested: number }>(`/api/datasets/${checked(id, DATASET_ID, 'dataset')}/items`, { trace_ids: traceIds }),
  removeDatasetItem: async (id: string, traceId: string) => {
    if (!isTraceId(traceId)) throw new ApiError(400, 'bad_request', 'Invalid trace ID');
    return del(`/api/datasets/${checked(id, DATASET_ID, 'dataset')}/items/${traceId}`);
  },
  evaluateDataset: async (id: string) => post<EvalRun>(`/api/datasets/${checked(id, DATASET_ID, 'dataset')}/evaluate`, {}),
  datasetEvals: async (id: string) => get<{ evals: EvalRun[] }>(`/api/datasets/${checked(id, DATASET_ID, 'dataset')}/evals`),
  agentChecks: (agent: string, window: string) => get<AgentChecks>('/api/agents/checks', { agent, window }),
  economics: (window: string) => get<{ window: string; agents: Economics[] }>('/api/economics', { window }),
  grading: (window: string, agent?: string) => get<GradingResponse>('/api/agents/grading', agent ? { window, agent } : { window }),
  outcomeSettings: () => get<OutcomeSettings>('/api/outcomes/settings'),
  putOutcomeSettings: (v: { reversal_outcomes: string[]; pending_hours: number }) => put<OutcomeSettings>('/api/outcomes/settings', v),
  outcomeEndpoints: () => get<{ endpoints: OutcomeEndpoint[] }>('/api/outcomes/endpoints'),
  createOutcomeEndpoint: (name: string) =>
    post<{ endpoint: OutcomeEndpoint; secret: string; path: string; signature_header: string }>('/api/outcomes/endpoints', { name }),
  revokeOutcomeEndpoint: (id: string) => post<{ revoked: boolean }>(`/api/outcomes/endpoints/${checked(id, OUTCOME_ENDPOINT_ID, 'endpoint')}/revoke`, {}),
  outcomeEvents: (status?: OutcomeEventStatus) => get<{ events: OutcomeEvent[] }>('/api/outcomes/events', status ? { status, limit: '100' } : { limit: '100' }),
  problemSuggestions: (src: ProblemSource) => get<SuggestionsResponse>('/api/problems/suggestions', problemParams(src)),
  addProblemItems: async (id: string, src: ProblemSource) =>
    post<{ added: number; found: number; source: string }>(`/api/datasets/${checked(id, DATASET_ID, 'dataset')}/items/from-problems`, problemParams(src)),
  search: (p: { q: string; window: string; agent?: string; status?: string }) =>
    get<{ window: string; q: string; hits: SearchHit[] }>('/api/search', {
      q: p.q,
      window: p.window,
      ...(p.agent ? { agent: p.agent } : {}),
      ...(p.status ? { status: p.status } : {}),
    }),
  sessions: (window: string, agent?: string) => get<{ window: string; sessions: Session[] }>('/api/sessions', { window, ...(agent ? { agent } : {}) }),
  session: (id: string) => get<SessionDetail>(`/api/sessions/${encodeURIComponent(id)}`),
  alertRules: () => get<{ rules: AlertRule[] }>('/api/alert-rules'),
  createAlertRule: (r: { kind: RuleKind; threshold: number; window_minutes: number; min_runs: number; enabled: boolean }) =>
    post<AlertRule>('/api/alert-rules', r),
  updateAlertRule: async (id: string, r: { threshold: number; window_minutes: number; min_runs: number; enabled: boolean }) =>
    put<AlertRule>(`/api/alert-rules/${checked(id, RULE_ID, 'rule')}`, r),
  alertChannels: () => get<{ channels: AlertChannel[] }>('/api/alert-channels'),
  createAlertChannel: (c: { kind: ChannelKind; name: string; target: string }) =>
    post<{ channel: AlertChannel; signing_secret?: string }>('/api/alert-channels', c),
  setAlertChannelEnabled: async (id: string, enabled: boolean) =>
    put<void>(`/api/alert-channels/${checked(id, CHANNEL_ID, 'channel')}/state`, { enabled }),
  deleteAlertChannel: async (id: string) => del(`/api/alert-channels/${checked(id, CHANNEL_ID, 'channel')}`),
  testAlertChannel: async (id: string) =>
    post<{ ok: boolean; error?: string }>(`/api/alert-channels/${checked(id, CHANNEL_ID, 'channel')}/test`, {}),
  alertEvents: () => get<{ events: AlertEvent[] }>('/api/alert-events'),
  // Learning loop (404 unless the workspace has the learning_loop feature).
  lessons: (agent?: string) => get<LessonsResponse>('/api/lessons', agent !== undefined ? { agent } : {}),
  vault: (agent: string) => get<VaultResponse>('/api/vault', { agent }),
  vaultNote: (agent: string, path: string) => get<VaultNote>('/api/vault/note', { agent, path }),
  saveVaultNote: (agent: string, path: string, content: string, base: string) =>
    put<VaultNote>('/api/vault/note', { agent, path, content, base_sha256: base }),
  vaultAction: (agent: string, path: string, action: 'approve' | 'retire' | 'restore', sha256: string) =>
    post<{ path: string; status: string; version: number }>('/api/vault/note/action', { agent, path, action, sha256 }),
  vaultReview: (agent: string, path: string, sha256: string) => post<{ path: string }>('/api/vault/note/review', { agent, path, sha256 }),
  securityReviewers: () => get<Reviewers>('/api/security-reviewers'),
  addSecurityReviewer: (email: string) => post<{ user_id: string }>('/api/security-reviewers', { email }),
  // Learning loop, phase 2: proofs, shipped releases, opt-in and kill switches.
  skillProofs: (agent: string, path: string) => get<{ proofs: Proof[] }>('/api/skills/proofs', { agent, path }),
  skillReleases: (agent: string, path?: string) =>
    get<{ releases: Release[]; settings: ShipSettings }>('/api/skills/releases', path ? { agent, path } : { agent }),
  skillRollbacks: (days = 7) => get<{ rollbacks: Rollback[] }>('/api/skills/rollbacks', { days: String(days) }),
  putShipSettings: (agent: string, ship: boolean, killAll: boolean) => put<ShipSettings>('/api/skills/settings', { agent, ship, kill_all: killAll }),
  killSkill: (agent: string, path: string, killed: boolean) => post<ShipSettings>('/api/skills/kill', { agent, path, killed }),
};
