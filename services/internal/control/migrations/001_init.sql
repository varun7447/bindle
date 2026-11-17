-- Bindle control plane: tenants, users, memberships, ingest API keys.
--
-- {{api}} and {{ingest}} are replaced by bindle-migrate with quoted role
-- names. The migrating role owns everything; the app roles do not, so
-- row-level security always applies to them.

CREATE SCHEMA IF NOT EXISTS control;
REVOKE ALL ON SCHEMA control FROM PUBLIC;

CREATE TABLE control.tenants (
    id          text        PRIMARY KEY,
    name        text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- One row per person, keyed by their Cognito user ID (the token's sub).
CREATE TABLE control.users (
    id          text        PRIMARY KEY,
    cognito_sub text        NOT NULL UNIQUE,
    email       text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE control.memberships (
    tenant_id   text        NOT NULL REFERENCES control.tenants (id),
    user_id     text        NOT NULL REFERENCES control.users (id),
    role        text        NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);
-- v0: each user belongs to exactly one tenant, so the session alone
-- determines the tenant. Drop this index to allow multiple workspaces.
CREATE UNIQUE INDEX memberships_one_tenant_per_user ON control.memberships (user_id);

-- Ingest keys. Only the SHA-256 of the full key is stored; the key itself
-- is shown once at creation. display_prefix ("bd_live_ab12cd34") lets
-- people tell keys apart without revealing them.
CREATE TABLE control.api_keys (
    id              text        PRIMARY KEY,
    tenant_id       text        NOT NULL REFERENCES control.tenants (id),
    name            text        NOT NULL,
    display_prefix  text        NOT NULL,
    key_hash        bytea       NOT NULL UNIQUE CHECK (octet_length(key_hash) = 32),
    created_by      text        REFERENCES control.users (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz,
    revoked_at      timestamptz,
    replaced_by     text        REFERENCES control.api_keys (id),
    last_used_at    timestamptz
);
CREATE INDEX api_keys_tenant ON control.api_keys (tenant_id, created_at DESC);

-- Keys are tenant data: the API role sees only the tenant set in its
-- transaction with set_config('bindle.tenant', ..., true).
ALTER TABLE control.api_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.api_keys
    USING (tenant_id = current_setting('bindle.tenant', true))
    WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- Ingest resolves a key hash to its tenant through this function only; the
-- ingest role has no table access at all.
CREATE FUNCTION control.resolve_key(h bytea)
RETURNS TABLE (tenant_id text, key_id text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT k.tenant_id, k.id
    FROM control.api_keys k
    WHERE k.key_hash = h
      AND k.revoked_at IS NULL
      AND (k.expires_at IS NULL OR k.expires_at > now())
$$;

-- Records key usage, at most every 5 minutes per key.
CREATE FUNCTION control.touch_keys(ids text[]) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $$
    UPDATE control.api_keys SET last_used_at = now()
    WHERE id = ANY (ids)
      AND (last_used_at IS NULL OR last_used_at < now() - interval '5 minutes')
$$;

REVOKE ALL ON FUNCTION control.resolve_key(bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION control.touch_keys(text[]) FROM PUBLIC;

-- Least privilege. Nobody can delete: revoking is an update.
GRANT USAGE ON SCHEMA control TO {{api}}, {{ingest}};
GRANT SELECT, INSERT ON control.tenants, control.users, control.memberships TO {{api}};
GRANT SELECT, INSERT ON control.api_keys TO {{api}};
GRANT UPDATE (name, expires_at, revoked_at, replaced_by) ON control.api_keys TO {{api}};
GRANT EXECUTE ON FUNCTION control.resolve_key(bytea), control.touch_keys(text[]) TO {{ingest}};
