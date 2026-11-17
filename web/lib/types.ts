// Response shapes of bindle-api (services/internal/api). Keep in sync.

export type RunStatus = 'ok' | 'failed' | 'unknown';

export interface Run {
  trace_id: string;
  started_at: string;
  duration_ms: number;
  status: RunStatus;
  root_status: string;
  steps: number;
  failed_steps: number;
  input_tokens: number;
  output_tokens: number;
  cost_usd: number;
  cost_partial: boolean;
  cost_unknown_steps: number;
  agent: string;
  agent_version: string;
  framework: string;
  environment: string;
  session_id: string;
}

export interface RunsResponse {
  runs: Run[];
  next_cursor?: string;
}

export interface StepSummary {
  step_id: string;
  parent_step_id: string;
  name: string;
  type: string;
  status: string;
  attempt: number;
  start_time: string;
  duration_ms: number;
  provider: string;
  model: string;
  tool_name: string;
  input_tokens: number;
  output_tokens: number;
  cached_tokens: number;
  cost_usd: number;
  cost_known: boolean;
  error_message: string;
  input_bytes: number;
  output_bytes: number;
  reasoning_bytes: number;
  redacted: boolean;
}

export interface TraceResponse {
  trace: Run;
  steps: StepSummary[];
  truncated: boolean;
}

export interface StepDetail extends StepSummary {
  trace_id: string;
  framework: string;
  input: string;
  output: string;
  reasoning: string;
  attributes: Record<string, string>;
  links: string[];
  redacted_kinds: string[];
}

export interface StepResponse {
  step: StepDetail;
}

export interface SeriesPoint {
  bucket: string;
  runs: number;
  failed_runs: number;
  cost_usd: number;
}

export interface ToolFailure {
  tool_name: string;
  failures: number;
  calls: number;
}

export interface OverviewResponse {
  window: string;
  from: string;
  to: string;
  bucket_seconds: number;
  runs: number;
  failed_runs: number;
  run_failure_rate: number;
  step_failure_rate: number;
  p50_run_ms: number;
  p95_run_ms: number;
  cost_usd: number;
  cost_partial: boolean;
  cost_unknown_steps: number;
  input_tokens: number;
  output_tokens: number;
  series: SeriesPoint[];
  top_failing_tools: ToolFailure[];
}

export interface Alert {
  kind: string;
  severity: string;
  trace_id: string;
  tool_name: string;
  repeats: number;
  threshold: number;
  first_seen: string;
  last_seen: string;
}

export interface AlertsResponse {
  window: string;
  alerts: Alert[];
}

export interface Me {
  user_id: string;
  tenant_id: string;
  tenant_name: string;
  role: 'owner' | 'admin' | 'member';
  can_manage_keys: boolean;
}

export interface APIKey {
  id: string;
  name: string;
  display_prefix: string;
  created_by?: string;
  created_at: string;
  expires_at?: string;
  revoked_at?: string;
  replaced_by?: string;
  last_used_at?: string;
}

export interface CreatedKey {
  key: APIKey;
  secret: string;
  old_key_expires_in_hours?: number;
}
