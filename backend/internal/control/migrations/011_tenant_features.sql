-- Per-workspace feature flags. Only the operator sets them, with
-- bindle-admin connected as the schema owner; no app role can write here.
-- A feature is on while its row exists.
CREATE TABLE control.tenant_features (
    tenant_id   text        NOT NULL REFERENCES control.tenants (id),
    feature     text        NOT NULL CHECK (feature IN ('ledger', 'passports', 'outcome_grading', 'learning_loop')),
    enabled_by  text        NOT NULL,
    enabled_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, feature)
);
ALTER TABLE control.tenant_features ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.tenant_features
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
GRANT SELECT ON control.tenant_features TO {{api}};

-- The pipeline (ingest, processor) and workers read flags through this
-- function only.
CREATE FUNCTION control.features_for(t text) RETURNS text[]
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT COALESCE(array_agg(f.feature ORDER BY f.feature), '{}'::text[])
    FROM control.tenant_features f WHERE f.tenant_id = t
$$;
REVOKE ALL ON FUNCTION control.features_for(text) FROM PUBLIC;
GRANT USAGE ON SCHEMA control TO {{processor}}, {{alerts}};
GRANT EXECUTE ON FUNCTION control.features_for(text) TO {{api}}, {{ingest}}, {{processor}}, {{alerts}};
