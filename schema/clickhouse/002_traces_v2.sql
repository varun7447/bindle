-- traces v2: adds environment, session_id, root_status and cost_unknown_steps
-- for the query API, and puts time in the sort key so time-range scans on the
-- runs list skip old parts. The table and its MV are rebuilt and backfilled
-- from bindle.steps (pre-production: no traffic to preserve during the swap).
--
-- cost_unknown_steps counts model-using steps whose price is unknown, so the
-- console can show a run's cost as partial instead of a wrong total.
-- root_status is the root step's status; runs with no root yet read ''.
-- String rollups use max, not any: when a trace arrives in several insert
-- batches, a batch without the value contributes '' and any() could keep it.

DROP VIEW IF EXISTS bindle.traces_mv;
DROP TABLE IF EXISTS bindle.traces;

CREATE TABLE bindle.traces
(
    tenant_id           LowCardinality(String),
    trace_id            String,
    -- Hour bucket of the trace's first step: fixed per trace, so it is safe in
    -- the sort key. Late steps from an earlier hour are rare and land in a
    -- separate row that the queries merge with GROUP BY trace_id.
    start_hour          DateTime('UTC'),
    first_start         SimpleAggregateFunction(min, DateTime64(9, 'UTC')),
    steps               SimpleAggregateFunction(sum, UInt64),
    failed_steps        SimpleAggregateFunction(sum, UInt64),
    cost_unknown_steps  SimpleAggregateFunction(sum, UInt64),
    input_tokens        SimpleAggregateFunction(sum, Int64),
    output_tokens       SimpleAggregateFunction(sum, Int64),
    cost_usd            SimpleAggregateFunction(sum, Float64),
    agent_name          SimpleAggregateFunction(max, String),
    agent_version       SimpleAggregateFunction(max, String),
    framework           SimpleAggregateFunction(max, String),
    environment         SimpleAggregateFunction(max, String),
    session_id          SimpleAggregateFunction(max, String),
    root_status         SimpleAggregateFunction(max, String),
    root_duration       SimpleAggregateFunction(max, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(start_hour)
ORDER BY (tenant_id, start_hour, trace_id)
TTL start_hour + INTERVAL 30 DAY;

CREATE MATERIALIZED VIEW bindle.traces_mv TO bindle.traces AS
SELECT
    tenant_id,
    trace_id,
    toStartOfHour(min(start_time))                       AS start_hour,
    min(start_time)                                      AS first_start,
    count()                                              AS steps,
    countIf(status = 'failure')                          AS failed_steps,
    countIf(NOT cost_known AND (model != '' OR type = 'llm_call')) AS cost_unknown_steps,
    sum(input_tokens)                                    AS input_tokens,
    sum(output_tokens)                                   AS output_tokens,
    sum(cost_usd)                                        AS cost_usd,
    anyIf(agent_name, agent_name != '')                  AS agent_name,
    anyIf(agent_version, agent_version != '')            AS agent_version,
    anyIf(framework, framework != 'unknown')             AS framework,
    anyIf(environment, environment != '')                AS environment,
    anyIf(session_id, session_id != '')                  AS session_id,
    anyIf(status, parent_step_id = '')                   AS root_status,
    maxIf(duration_ms, parent_step_id = '')              AS root_duration
FROM bindle.steps
GROUP BY tenant_id, trace_id;

-- Backfill from existing steps.
INSERT INTO bindle.traces
SELECT
    tenant_id,
    trace_id,
    toStartOfHour(min(start_time)),
    min(start_time),
    count(),
    countIf(status = 'failure'),
    countIf(NOT cost_known AND (model != '' OR type = 'llm_call')),
    sum(input_tokens),
    sum(output_tokens),
    sum(cost_usd),
    anyIf(agent_name, agent_name != ''),
    anyIf(agent_version, agent_version != ''),
    anyIf(framework, framework != 'unknown'),
    anyIf(environment, environment != ''),
    anyIf(session_id, session_id != ''),
    anyIf(status, parent_step_id = ''),
    maxIf(duration_ms, parent_step_id = '')
FROM bindle.steps
GROUP BY tenant_id, trace_id;
