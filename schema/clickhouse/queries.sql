-- Reference queries for the console's query API (services/internal/query).
-- Always bind tenant_id server-side from the session, never from the client.
-- Values are ClickHouse query parameters ({name:Type}), never string-spliced.

-- Runs list. Keyset pagination on (first_start, trace_id), newest first.
-- start_hour bounds let ClickHouse skip parts outside the window.
SELECT trace_id, min(first_start) AS started, sum(steps) AS steps, sum(failed_steps) AS failed,
       sum(cost_usd) AS cost_usd, sum(cost_unknown_steps) AS cost_unknown_steps,
       max(agent_name) AS agent, max(framework) AS framework, max(environment) AS environment,
       max(root_status) AS root_status, max(root_duration) AS duration_ms
FROM bindle.traces
WHERE tenant_id = {tenant:String}
  AND start_hour >= toStartOfHour({from:DateTime64(9)}) AND start_hour <= {to:DateTime64(9)}
GROUP BY trace_id
HAVING started >= {from:DateTime64(9)} AND started < {to:DateTime64(9)}
ORDER BY started DESC, trace_id DESC
LIMIT 100;

-- Trace waterfall
SELECT step_id, parent_step_id, name, type, status, attempt, start_time, duration_ms,
       model, tool_name, input_tokens, output_tokens, cost_usd, error_message
FROM bindle.steps
WHERE tenant_id = {tenant:String} AND trace_id = {trace:String}
ORDER BY start_time;

-- Overview KPIs (last 24h)
SELECT
    uniqExact(trace_id)                                  AS runs,
    countIf(status = 'failure') / count()                AS step_failure_rate,
    quantile(0.95)(if(parent_step_id = '', duration_ms, NULL)) AS p95_run_ms,
    sum(cost_usd)                                        AS cost_usd
FROM bindle.steps
WHERE tenant_id = {tenant:String} AND start_time > now64() - INTERVAL 1 DAY;

-- Loop detector: same tool + same input repeated within one trace
SELECT trace_id, tool_name, cityHash64(input) AS input_hash, count() AS repeats
FROM bindle.steps
WHERE tenant_id = {tenant:String} AND type = 'tool_call' AND start_time > now64() - INTERVAL 1 HOUR
GROUP BY trace_id, tool_name, input_hash
HAVING repeats >= 5
ORDER BY repeats DESC;
