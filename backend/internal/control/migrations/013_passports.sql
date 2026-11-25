-- Management keys and agent passports.
--
-- Management keys (bd_mgmt_...) are for the CLI and CI: pushing passports
-- now, vault sync and skills later. They are separate from ingest keys so
-- an agent's own ingest key can never change what the agent may do. Like
-- ingest keys, only a SHA-256 of the key is stored.
CREATE TABLE control.mgmt_keys (
    id              text        PRIMARY KEY,
    tenant_id       text        NOT NULL REFERENCES control.tenants (id),
    name            text        NOT NULL,
    display_prefix  text        NOT NULL,
    key_hash        bytea       NOT NULL UNIQUE CHECK (octet_length(key_hash) = 32),
    scopes          text[]      NOT NULL CHECK (cardinality(scopes) > 0 AND scopes <@ ARRAY['passport:write', 'vault:sync', 'skills:write']),
    created_by      text        REFERENCES control.users (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz,
    last_used_at    timestamptz
);
CREATE INDEX mgmt_keys_tenant ON control.mgmt_keys (tenant_id, created_at DESC);
ALTER TABLE control.mgmt_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.mgmt_keys
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- Ingest resolves a management key through this function only.
CREATE FUNCTION control.resolve_mgmt_key(h bytea)
RETURNS TABLE (tenant_id text, key_id text, scopes text[])
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT k.tenant_id, k.id, k.scopes FROM control.mgmt_keys k
    WHERE k.key_hash = h AND k.revoked_at IS NULL
$$;

-- Passports: one row per pushed version, never changed afterwards. The
-- newest version is the agent's active passport.
CREATE TABLE control.passports (
    tenant_id   text        NOT NULL REFERENCES control.tenants (id),
    agent       text        NOT NULL CHECK (length(agent) BETWEEN 1 AND 128),
    version     integer     NOT NULL CHECK (version > 0),
    document    jsonb       NOT NULL,
    sha256      bytea       NOT NULL CHECK (octet_length(sha256) = 32),
    pushed_by   text        NOT NULL,
    pushed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, agent, version)
);
ALTER TABLE control.passports ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.passports
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

CREATE FUNCTION control.passports_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'control.passports: passport versions are append-only';
END
$$;
CREATE TRIGGER passports_append_only BEFORE UPDATE OR DELETE ON control.passports
    FOR EACH ROW EXECUTE FUNCTION control.passports_immutable();
CREATE TRIGGER passports_no_truncate BEFORE TRUNCATE ON control.passports
    FOR EACH STATEMENT EXECUTE FUNCTION control.passports_immutable();

-- Pushes a validated passport (ingest validates before calling). Pushing
-- the same document again is a no-op that returns the current version.
CREATE FUNCTION control.push_passport(t text, a text, doc jsonb, digest bytea, by_key text)
RETURNS TABLE (version integer, pushed_at timestamptz, unchanged boolean)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
#variable_conflict use_column
DECLARE
    cur_version integer;
    cur_sha bytea;
    cur_at timestamptz;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('passport/' || t || '/' || a, 0));
    SELECT p.version, p.sha256, p.pushed_at INTO cur_version, cur_sha, cur_at
    FROM control.passports p WHERE p.tenant_id = t AND p.agent = a ORDER BY p.version DESC LIMIT 1;
    IF cur_sha = digest THEN
        RETURN QUERY SELECT cur_version, cur_at, true;
        RETURN;
    END IF;
    INSERT INTO control.passports (tenant_id, agent, version, document, sha256, pushed_by)
    VALUES (t, a, COALESCE(cur_version, 0) + 1, doc, digest, by_key)
    RETURNING control.passports.version, control.passports.pushed_at INTO cur_version, cur_at;
    UPDATE control.mgmt_keys SET last_used_at = now()
    WHERE tenant_id = t AND id = substr(by_key, 5) AND by_key LIKE 'key:%';
    RETURN QUERY SELECT cur_version, cur_at, false;
END
$$;

-- Each agent's active (newest) passport. The pipeline and the alerts
-- worker read passports through this function only.
CREATE FUNCTION control.passports_for(t text)
RETURNS TABLE (agent text, version integer, document jsonb)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT DISTINCT ON (p.agent) p.agent, p.version, p.document
    FROM control.passports p WHERE p.tenant_id = t
    ORDER BY p.agent, p.version DESC
$$;

-- Tenants that have a passport and the passports feature: the alerts worker
-- checks their rate limits and budgets.
CREATE FUNCTION control.passport_tenants() RETURNS SETOF text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT DISTINCT p.tenant_id FROM control.passports p
    JOIN control.tenant_features f ON f.tenant_id = p.tenant_id AND f.feature = 'passports'
    ORDER BY 1
$$;

REVOKE ALL ON FUNCTION control.resolve_mgmt_key(bytea), control.push_passport(text, text, jsonb, bytea, text),
    control.passports_for(text), control.passport_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.passport_tenants() TO {{alerts}};
GRANT EXECUTE ON FUNCTION control.resolve_mgmt_key(bytea), control.push_passport(text, text, jsonb, bytea, text),
    control.passports_for(text) TO {{ingest}};
GRANT EXECUTE ON FUNCTION control.passports_for(text) TO {{alerts}};

-- The API manages management keys (owners and admins) and reads passports.
GRANT SELECT, INSERT ON control.mgmt_keys TO {{api}};
GRANT UPDATE (revoked_at) ON control.mgmt_keys TO {{api}};
GRANT SELECT ON control.passports TO {{api}};
