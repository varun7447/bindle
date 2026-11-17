// Server-side client for bindle-api. Only import from Server Components and
// route handlers: the console calls the API from its own server, so the API
// needs no CORS and credentials never reach the browser.
//
// The console holds no credentials of its own and never chooses a tenant.
// bindle-api resolves the tenant from the caller's session; until console
// auth exists (step 3) it rejects every request unless its local dev bypass
// is on. Step 3 forwards the signed-in user's session here.

import type {
  AlertsResponse,
  OverviewResponse,
  RunsResponse,
  StepResponse,
  TraceResponse,
} from './types';
import { isStepId, isTraceId } from './ids';

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

export async function apiGet<T>(path: string, params: Params = {}, fetchImpl: typeof fetch = fetch): Promise<T> {
  const url = buildUrl(apiBase(), path, params);
  let res: Response;
  try {
    res = await fetchImpl(url, { cache: 'no-store', headers: { accept: 'application/json' } });
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
  return (await res.json()) as T;
}

export const api = {
  runs: (p: { from?: string; to?: string; status?: string; agent?: string; environment?: string; limit?: string; cursor?: string }) =>
    apiGet<RunsResponse>('/api/runs', p),
  trace: async (id: string) => {
    if (!isTraceId(id)) throw new ApiError(400, 'bad_request', 'Invalid trace ID');
    return apiGet<TraceResponse>(`/api/traces/${id}`);
  },
  step: async (traceId: string, stepId: string) => {
    if (!isTraceId(traceId) || !isStepId(stepId)) throw new ApiError(400, 'bad_request', 'Invalid step ID');
    return apiGet<StepResponse>(`/api/traces/${traceId}/steps/${stepId}`);
  },
  overview: (window: string) => apiGet<OverviewResponse>('/api/overview', { window }),
  alerts: (window: string) => apiGet<AlertsResponse>('/api/alerts', { window }),
};
