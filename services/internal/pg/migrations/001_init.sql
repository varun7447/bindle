-- Bindle storage on PostgreSQL.
--
-- {{writer}} and {{reader}} are replaced by bindle-migrate with quoted role
-- names. The migrating role owns the tables; the app roles do not, so
-- row-level security always applies to them.

CREATE SCHEMA IF NOT EXISTS bindle;
REVOKE ALL ON SCHEMA bindle FROM PUBLIC;

-- One row per normalized agent step (span). Partitioned by day on
-- start_time (microseconds); start_ns keeps the exact nanosecond timestamp.
CREATE TABLE bindle.steps (
    tenant_id       text             NOT NULL,
    trace_id        text             NOT NULL,
    step_id         text             NOT NULL,
    parent_step_id  text             NOT NULL DEFAULT '',
    name            text             NOT NULL DEFAULT '',
    type            text             NOT NULL,
    status          text             NOT NULL,
    attempt         bigint           NOT NULL DEFAULT 0,
    framework       text             NOT NULL DEFAULT '',
    service_name    text             NOT NULL DEFAULT '',
    agent_name      text             NOT NULL DEFAULT '',
    agent_version   text             NOT NULL DEFAULT '',
    session_id      text             NOT NULL DEFAULT '',
    environment     text             NOT NULL DEFAULT '',
    provider        text             NOT NULL DEFAULT '',
    model           text             NOT NULL DEFAULT '',
    tool_name       text             NOT NULL DEFAULT '',
    input_tokens    bigint           NOT NULL DEFAULT 0,
    output_tokens   bigint           NOT NULL DEFAULT 0,
    cached_tokens   bigint           NOT NULL DEFAULT 0,
    cost_usd        double precision NOT NULL DEFAULT 0,
    cost_known      boolean          NOT NULL DEFAULT false,
    start_time      timestamptz      NOT NULL,
    start_ns        bigint           NOT NULL,
    duration_ms     double precision NOT NULL DEFAULT 0,
    error_message   text             NOT NULL DEFAULT '',
    input           text COMPRESSION lz4 NOT NULL DEFAULT '',
    output          text COMPRESSION lz4 NOT NULL DEFAULT '',
    reasoning       text COMPRESSION lz4 NOT NULL DEFAULT '',
    attributes      jsonb COMPRESSION lz4 NOT NULL DEFAULT '{}',
    links           jsonb            NOT NULL DEFAULT '[]',
    ingested_at     timestamptz      NOT NULL DEFAULT now(),
    -- Also makes ingest idempotent: an exporter retrying a batch does not
    -- duplicate steps or double-count the trace rollup.
    PRIMARY KEY (tenant_id, trace_id, step_id, start_time)
) PARTITION BY RANGE (start_time);

CREATE INDEX steps_tenant_time ON bindle.steps (tenant_id, start_time);

-- One row per trace for the runs list and overview, maintained by the
-- writer in the same statement that inserts steps.
CREATE TABLE bindle.traces (
    tenant_id           text             NOT NULL,
    trace_id            text             NOT NULL,
    first_start         timestamptz      NOT NULL,
    first_start_ns      bigint           NOT NULL,
    steps               bigint           NOT NULL DEFAULT 0,
    failed_steps        bigint           NOT NULL DEFAULT 0,
    cost_unknown_steps  bigint           NOT NULL DEFAULT 0,
    input_tokens        bigint           NOT NULL DEFAULT 0,
    output_tokens       bigint           NOT NULL DEFAULT 0,
    cost_usd            double precision NOT NULL DEFAULT 0,
    agent_name          text             NOT NULL DEFAULT '',
    agent_version       text             NOT NULL DEFAULT '',
    framework           text             NOT NULL DEFAULT '',
    environment         text             NOT NULL DEFAULT '',
    session_id          text             NOT NULL DEFAULT '',
    root_status         text             NOT NULL DEFAULT '',
    root_duration_ms    double precision NOT NULL DEFAULT 0,
    updated_at          timestamptz      NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, trace_id)
);

CREATE INDEX traces_runs ON bindle.traces (tenant_id, first_start_ns DESC, trace_id DESC);

-- Tenant isolation, second layer: app roles see and write only rows of the
-- tenant set with set_config('bindle.tenant', ..., true) in their
-- transaction. Unset tenant: current_setting returns NULL and nothing matches.
ALTER TABLE bindle.steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE bindle.traces ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON bindle.steps
    USING (tenant_id = current_setting('bindle.tenant', true))
    WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
CREATE POLICY tenant_isolation ON bindle.traces
    USING (tenant_id = current_setting('bindle.tenant', true))
    WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- Partition maintenance runs as the table owner (SECURITY DEFINER) so the
-- writer role needs no DDL rights. Inputs are dates only, and the allowed
-- range is bounded so a caller cannot create or drop arbitrary partitions.
CREATE FUNCTION bindle.ensure_steps_partition(day date) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    part text := 'steps_' || to_char(day, 'YYYYMMDD');
BEGIN
    IF day < current_date - 45 OR day > current_date + 3 THEN
        RAISE EXCEPTION 'partition day % outside allowed range', day;
    END IF;
    IF to_regclass('bindle.' || part) IS NOT NULL THEN
        RETURN;
    END IF;
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS bindle.%I PARTITION OF bindle.steps FOR VALUES FROM (%L) TO (%L)',
        part, day::text || ' 00:00:00+00', (day + 1)::text || ' 00:00:00+00');
EXCEPTION WHEN duplicate_table OR unique_violation THEN
    NULL; -- another writer created it concurrently
END;
$$;

-- Drops step partitions that end on or before cutoff and deletes trace
-- rollups that started before it. Refuses cutoffs newer than 7 days ago.
CREATE FUNCTION bindle.apply_retention(cutoff date) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    r record;
    dropped integer := 0;
BEGIN
    IF cutoff > current_date - 7 THEN
        RAISE EXCEPTION 'retention cutoff % is too recent', cutoff;
    END IF;
    FOR r IN
        SELECT c.relname
        FROM pg_inherits i
        JOIN pg_class c ON c.oid = i.inhrelid
        WHERE i.inhparent = 'bindle.steps'::regclass
          AND c.relname ~ '^steps_[0-9]{8}$'
          AND to_date(substr(c.relname, 7), 'YYYYMMDD') + 1 <= cutoff
    LOOP
        EXECUTE format('DROP TABLE IF EXISTS bindle.%I', r.relname);
        dropped := dropped + 1;
    END LOOP;
    DELETE FROM bindle.traces WHERE first_start < cutoff::timestamptz;
    RETURN dropped;
END;
$$;

REVOKE ALL ON FUNCTION bindle.ensure_steps_partition(date) FROM PUBLIC;
REVOKE ALL ON FUNCTION bindle.apply_retention(date) FROM PUBLIC;

-- Least privilege. Writer: insert steps, upsert rollups, manage partitions.
-- Reader: select only. Neither can update or delete steps, run DDL, or
-- bypass row-level security.
GRANT USAGE ON SCHEMA bindle TO {{writer}}, {{reader}};
GRANT SELECT, INSERT ON bindle.steps TO {{writer}};
GRANT SELECT, INSERT, UPDATE ON bindle.traces TO {{writer}};
GRANT EXECUTE ON FUNCTION bindle.ensure_steps_partition(date) TO {{writer}};
GRANT EXECUTE ON FUNCTION bindle.apply_retention(date) TO {{writer}};
GRANT SELECT ON bindle.steps, bindle.traces TO {{reader}};
