-- Agent limits (kill switch, per-run caps, daily budgets) and model price
-- overrides, stored with the tenant's SDK config and edited separately.
ALTER TABLE control.sdk_config
    ADD COLUMN limits jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(limits) = 'array'),
    -- The workspace's model price overrides (used by ingest, not the SDK).
    ADD COLUMN prices jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(prices) = 'array');

-- One row per agent and UTC day on which its daily budget was exceeded.
-- Written once by the alerts worker (the insert decides who notifies).
CREATE TABLE control.budget_state (
    tenant_id    text             NOT NULL REFERENCES control.tenants (id),
    agent        text             NOT NULL,
    day          date             NOT NULL,
    spent_usd    double precision NOT NULL,
    budget_usd   double precision NOT NULL,
    exceeded_at  timestamptz      NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, agent, day)
);
ALTER TABLE control.budget_state ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.budget_state
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- Ingest reads config, limits and today's over-budget agents through this
-- function only. (Return type changed, so drop and recreate.)
DROP FUNCTION control.sdk_config_for(text);
CREATE FUNCTION control.sdk_config_for(t text)
RETURNS TABLE (version bigint, sample_rate double precision, capture_content boolean, redact_patterns jsonb,
               limits jsonb, over_budget text[], prices jsonb, updated_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT c.version, c.sample_rate, c.capture_content, c.redact_patterns, c.limits,
           ARRAY(SELECT b.agent FROM control.budget_state b
                 WHERE b.tenant_id = t AND b.day = (now() AT TIME ZONE 'UTC')::date ORDER BY b.agent),
           c.prices, c.updated_at
    FROM control.sdk_config c WHERE c.tenant_id = t
$$;
REVOKE ALL ON FUNCTION control.sdk_config_for(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.sdk_config_for(text) TO {{ingest}};

-- The alerts worker finds tenants with a daily budget through this
-- function, then works inside each tenant under row-level security.
CREATE FUNCTION control.budget_tenants() RETURNS TABLE (tenant_id text, limits jsonb)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT c.tenant_id, c.limits FROM control.sdk_config c
    WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(c.limits) l WHERE (l ->> 'daily_budget_usd')::double precision > 0)
$$;
REVOKE ALL ON FUNCTION control.budget_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.budget_tenants() TO {{alerts}};

GRANT UPDATE (limits, prices) ON control.sdk_config TO {{api}};
GRANT SELECT ON control.budget_state TO {{api}};
GRANT SELECT, INSERT ON control.budget_state TO {{alerts}};
