-- Reference queries for the console's query API. Always bind tenant_id
-- server-side from the session; never accept it from the client.

-- Runs list (last 24h)
SELECT trace_id, min(first_start) AS started, sum(steps) AS steps, sum(failed_steps) AS failed,
       sum(cost_usd) AS cost_usd, any(agent_name) AS agent, any(framework) AS framework,
       max(root_duration) AS duration_ms
FROM bindle.traces
WHERE tenant_id = {tenant:String} AND first_start > now64() - INTERVAL 1 DAY
GROUP BY trace_id
ORDER BY started DESC
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
