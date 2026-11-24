// Response shapes of bindle-api (backend/internal/api). Keep in sync.

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
  cached_tokens?: number;
  llm_calls?: number;
  tool_calls?: number;
  peak_input_tokens?: number;
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
  /** Why an LLM call stopped ("stop", "length", "max_tokens"…), when the framework reports it. */
  finish_reason?: string;
  /** Security signals on the step (prompt_injection, secret_exposed, possible_exfiltration). */
  security_flags?: string[];
  /** Learned skills loaded in the step ("<path>@<release>"). */
  skills_used?: string[];
  /** Why each flag was raised: "flag:reason". */
  security_reasons?: string[];
  /** Redaction labels found in the step (EMAIL, CARD, ...): labels only. */
  data_classes?: string[];
  /** How the step went outside its agent's passport: "kind:detail". Observe-only. */
  passport_drift?: string[];
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
  truncated_fields?: string[];
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

export interface AgentSummary {
  agent: string;
  runs: number;
  failed_runs: number;
  cost_usd: number;
  cost_partial: boolean;
  p95_run_ms: number;
  last_run_start: string;
  median_steps?: number;
  median_cost_usd?: number;
}

export interface ErrorPattern {
  pattern: string;
  failures: number;
  sample_trace_id: string;
  sample_step_id: string;
}

export interface Outlier {
  run: Run;
  reason: 'steps' | 'cost';
  value: number;
  typical: number;
}

export interface Issue {
  step_name: string;
  step_type: string;
  failures: number;
  runs: number;
  last_seen: string;
  sample_error: string;
  sample_trace_id: string;
  sample_step_id: string;
  patterns?: ErrorPattern[];
}

export interface Insights {
  agents: AgentSummary[];
  issues: Issue[];
  outliers?: Outlier[];
  unhappy_sessions?: { session_id: string; feedback: { positive: number; negative: number; count: number } }[];
  previous: { runs: number; failed_runs: number; cost_usd: number; p95_run_ms: number };
}
