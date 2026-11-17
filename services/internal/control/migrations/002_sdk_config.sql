-- Remote SDK configuration, one row per tenant (absent = defaults).
CREATE TABLE control.sdk_config (
    tenant_id        text             PRIMARY KEY REFERENCES control.tenants (id),
    version          bigint           NOT NULL CHECK (version > 0),
    sample_rate      double precision NOT NULL CHECK (sample_rate >= 0 AND sample_rate <= 1),
    capture_content  boolean          NOT NULL,
    redact_patterns  jsonb            NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(redact_patterns) = 'array'),
    updated_by       text             REFERENCES control.users (id),
    updated_at       timestamptz      NOT NULL DEFAULT now()
);

ALTER TABLE control.sdk_config ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.sdk_config
    USING (tenant_id = current_setting('bindle.tenant', true))
    WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- Ingest reads a tenant's config through this function only.
CREATE FUNCTION control.sdk_config_for(t text)
RETURNS TABLE (version bigint, sample_rate double precision, capture_content boolean, redact_patterns jsonb, updated_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT c.version, c.sample_rate, c.capture_content, c.redact_patterns, c.updated_at
    FROM control.sdk_config c WHERE c.tenant_id = t
$$;
REVOKE ALL ON FUNCTION control.sdk_config_for(text) FROM PUBLIC;

GRANT SELECT, INSERT ON control.sdk_config TO {{api}};
GRANT UPDATE (version, sample_rate, capture_content, redact_patterns, updated_by, updated_at) ON control.sdk_config TO {{api}};
GRANT EXECUTE ON FUNCTION control.sdk_config_for(text) TO {{ingest}};
