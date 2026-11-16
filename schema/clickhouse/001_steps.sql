-- Bindle step storage. One row per normalized agent step (span).
-- Ordered by tenant first so every query is tenant-scoped and fast.

CREATE DATABASE IF NOT EXISTS bindle;

CREATE TABLE IF NOT EXISTS bindle.steps
(
    tenant_id       LowCardinality(String),
    trace_id        String,
    step_id         String,
    parent_step_id  String,
    name            String,
    type            LowCardinality(String),
    status          LowCardinality(String),
    attempt         Int64,

    framework       LowCardinality(String),
    service_name    LowCardinality(String),
    agent_name      LowCardinality(String),
    agent_version   LowCardinality(String),
    session_id      String,
    environment     LowCardinality(String),

    provider        LowCardinality(String),
    model           LowCardinality(String),
    tool_name       LowCardinality(String),
    input_tokens    Int64,
    output_tokens   Int64,
    cached_tokens   Int64,
    cost_usd        Float64,
    cost_known      Bool,

    start_time      DateTime64(9, 'UTC'),
    duration_ms     Float64,

    error_message   String,
    input           String CODEC(ZSTD(3)),
    output          String CODEC(ZSTD(3)),
    reasoning       String CODEC(ZSTD(3)),

    attributes      Map(String, String),
    links           Array(String),

    ingested_at     DateTime64(3, 'UTC') DEFAULT now64(3),

    INDEX idx_trace trace_id TYPE bloom_filter GRANULARITY 4,
    INDEX idx_session session_id TYPE bloom_filter GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(start_time)
ORDER BY (tenant_id, toStartOfHour(start_time), trace_id, step_id)
TTL toDateTime(start_time) + INTERVAL 30 DAY;  -- per-plan retention comes later

-- One row per trace for the runs list and overview dashboard.
CREATE TABLE IF NOT EXISTS bindle.traces
(
    tenant_id      LowCardinality(String),
    trace_id       String,
    first_start    SimpleAggregateFunction(min, DateTime64(9, 'UTC')),
    steps          SimpleAggregateFunction(sum, UInt64),
    failed_steps   SimpleAggregateFunction(sum, UInt64),
    input_tokens   SimpleAggregateFunction(sum, Int64),
    output_tokens  SimpleAggregateFunction(sum, Int64),
    cost_usd       SimpleAggregateFunction(sum, Float64),
    agent_name     SimpleAggregateFunction(any, String),
    agent_version  SimpleAggregateFunction(any, String),
    framework      SimpleAggregateFunction(any, String),
    root_duration  SimpleAggregateFunction(max, Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(first_start)
ORDER BY (tenant_id, trace_id)
TTL toDateTime(first_start) + INTERVAL 30 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS bindle.traces_mv TO bindle.traces AS
SELECT
    tenant_id,
    trace_id,
    min(start_time)                            AS first_start,
    count()                                    AS steps,
    countIf(status = 'failure')                AS failed_steps,
    sum(input_tokens)                          AS input_tokens,
    sum(output_tokens)                         AS output_tokens,
    sum(cost_usd)                              AS cost_usd,
    anyIf(agent_name, agent_name != '')        AS agent_name,
    anyIf(agent_version, agent_version != '')  AS agent_version,
    anyIf(framework, framework != 'unknown')   AS framework,
    maxIf(duration_ms, parent_step_id = '')    AS root_duration
FROM bindle.steps
GROUP BY tenant_id, trace_id;
