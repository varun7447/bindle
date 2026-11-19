-- Runs stopped from the console. The SDK sees recent stops in its polled
-- config (stopped_runs) and bindle.check_limits() raises for that run.
-- Rows older than a day are removed when the next stop is recorded.
CREATE TABLE control.run_stops (
    tenant_id   text        NOT NULL REFERENCES control.tenants (id),
    trace_id    text        NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$'),
    stopped_by  text,
    stopped_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, trace_id)
);
CREATE INDEX run_stops_recent ON control.run_stops (tenant_id, stopped_at DESC);
ALTER TABLE control.run_stops ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.run_stops
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- Ingest reads config through this function only. It now also returns the
-- runs stopped in the last hour (at most 200), and a row of defaults for a
-- tenant that never saved settings, so stops reach every tenant.
DROP FUNCTION control.sdk_config_for(text);
CREATE FUNCTION control.sdk_config_for(t text)
RETURNS TABLE (version bigint, sample_rate double precision, capture_content boolean, redact_patterns jsonb,
               limits jsonb, over_budget text[], prices jsonb, updated_at timestamptz, stopped_runs text[])
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT COALESCE(c.version, 0), COALESCE(c.sample_rate, 1), COALESCE(c.capture_content, true),
           COALESCE(c.redact_patterns, '[]'::jsonb), COALESCE(c.limits, '[]'::jsonb),
           ARRAY(SELECT b.agent FROM control.budget_state b
                 WHERE b.tenant_id = t AND b.day = (now() AT TIME ZONE 'UTC')::date ORDER BY b.agent),
           COALESCE(c.prices, '[]'::jsonb), COALESCE(c.updated_at, 'epoch'::timestamptz),
           ARRAY(SELECT s.trace_id FROM control.run_stops s
                 WHERE s.tenant_id = t AND s.stopped_at > now() - interval '1 hour'
                 ORDER BY s.stopped_at DESC, s.trace_id LIMIT 200)
    FROM (SELECT t AS tenant_id) x LEFT JOIN control.sdk_config c ON c.tenant_id = x.tenant_id
$$;
REVOKE ALL ON FUNCTION control.sdk_config_for(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.sdk_config_for(text) TO {{ingest}};

GRANT SELECT, INSERT, UPDATE, DELETE ON control.run_stops TO {{api}};
