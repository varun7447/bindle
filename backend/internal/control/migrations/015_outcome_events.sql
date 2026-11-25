-- Outcome-graded quality (feature "outcome_grading"): business events that
-- arrive after a run, found by a correlation key the agent recorded
-- (bindle.correlate). Only tenant-bound hashes of correlation values are
-- stored, never the values.

-- Workspace settings: which outcome names are reversals (a run with one
-- counts as wrong even if every step and eval passed), and how long an event
-- that matches no run yet keeps being retried.
CREATE TABLE control.outcome_settings (
    tenant_id         text        PRIMARY KEY REFERENCES control.tenants (id),
    reversal_outcomes text[]      NOT NULL DEFAULT '{}' CHECK (cardinality(reversal_outcomes) <= 50),
    pending_hours     integer     NOT NULL DEFAULT 24 CHECK (pending_hours BETWEEN 1 AND 168),
    updated_by        text,
    updated_at        timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE control.outcome_settings ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.outcome_settings
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
GRANT SELECT, INSERT ON control.outcome_settings TO {{api}};
GRANT UPDATE (reversal_outcomes, pending_hours, updated_by, updated_at) ON control.outcome_settings TO {{api}};

-- Ingest and the matcher read settings through this function (defaults for
-- a workspace that never saved them).
CREATE FUNCTION control.outcome_settings_for(t text)
RETURNS TABLE (reversal_outcomes text[], pending_hours integer)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT COALESCE(s.reversal_outcomes, '{}'::text[]), COALESCE(s.pending_hours, 24)
    FROM (SELECT t AS tenant_id) x LEFT JOIN control.outcome_settings s ON s.tenant_id = x.tenant_id
$$;
REVOKE ALL ON FUNCTION control.outcome_settings_for(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.outcome_settings_for(text) TO {{api}}, {{ingest}}, {{alerts}};

-- Signed webhook endpoints. The secret signs request bodies (HMAC-SHA256)
-- and is shown once, when the endpoint is created. Like alert-channel
-- secrets it is stored as text so ingest can verify signatures; the API role
-- cannot read it back.
CREATE TABLE control.outcome_endpoints (
    id          text        PRIMARY KEY CHECK (id ~ '^oep_[0-9a-f]{16}$'),
    tenant_id   text        NOT NULL REFERENCES control.tenants (id),
    name        text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    secret      text        NOT NULL,
    created_by  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz
);
CREATE INDEX outcome_endpoints_tenant ON control.outcome_endpoints (tenant_id, created_at DESC);
ALTER TABLE control.outcome_endpoints ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.outcome_endpoints
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
GRANT INSERT ON control.outcome_endpoints TO {{api}};
GRANT SELECT (id, tenant_id, name, created_by, created_at, revoked_at) ON control.outcome_endpoints TO {{api}};
GRANT UPDATE (revoked_at) ON control.outcome_endpoints TO {{api}};

-- Ingest looks an endpoint up by its ID alone (the tenant comes from the
-- endpoint, never from the request body).
CREATE FUNCTION control.outcome_endpoint_secret(endpoint text)
RETURNS TABLE (tenant_id text, secret text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT e.tenant_id, e.secret FROM control.outcome_endpoints e WHERE e.id = endpoint AND e.revoked_at IS NULL
$$;
REVOKE ALL ON FUNCTION control.outcome_endpoint_secret(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.outcome_endpoint_secret(text) TO {{ingest}};

-- Reported events. The ID is derived from the tenant and the event's content,
-- so a retried or replayed request is the same event. An event is pending
-- until it matches a run (matched) or its retry window ends (unmatched); it
-- is never dropped silently.
CREATE TABLE control.outcome_events (
    id              text             PRIMARY KEY CHECK (id ~ '^oev_[0-9a-f]{32}$'),
    tenant_id       text             NOT NULL REFERENCES control.tenants (id),
    correlations    text[]           NOT NULL CHECK (cardinality(correlations) BETWEEN 1 AND 8),
    name            text             NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    success         boolean          NOT NULL,
    value           double precision,
    comment         text             CHECK (char_length(comment) <= 500),
    source          text             NOT NULL CHECK (source IN ('api', 'webhook')),
    endpoint_id     text,
    occurred_at     timestamptz      NOT NULL,
    received_at     timestamptz      NOT NULL DEFAULT now(),
    status          text             NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'matched', 'unmatched')),
    attempts        integer          NOT NULL DEFAULT 0,
    next_attempt_at timestamptz      NOT NULL DEFAULT now(),
    expires_at      timestamptz      NOT NULL,
    matched_traces  text[]           NOT NULL DEFAULT '{}',
    matched_at      timestamptz
);
CREATE INDEX outcome_events_due ON control.outcome_events (next_attempt_at) WHERE status = 'pending';
CREATE INDEX outcome_events_recent ON control.outcome_events (tenant_id, received_at DESC);
ALTER TABLE control.outcome_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.outcome_events
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
GRANT USAGE ON SCHEMA control TO {{ingest}};
GRANT SELECT, INSERT ON control.outcome_events TO {{ingest}};
GRANT SELECT ON control.outcome_events TO {{api}};
GRANT SELECT ON control.outcome_events TO {{alerts}};
GRANT UPDATE (status, attempts, next_attempt_at, matched_traces, matched_at) ON control.outcome_events TO {{alerts}};

-- The matcher finds workspaces with events due for a matching attempt, then
-- works inside each workspace under row-level security.
CREATE FUNCTION control.outcome_due_tenants()
RETURNS SETOF text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT DISTINCT e.tenant_id FROM control.outcome_events e
    WHERE e.status = 'pending' AND e.next_attempt_at <= now() ORDER BY 1 LIMIT 1000
$$;
REVOKE ALL ON FUNCTION control.outcome_due_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.outcome_due_tenants() TO {{alerts}};

-- Events are kept for 90 days (steps for 30), then removed.
CREATE FUNCTION control.outcome_events_prune()
RETURNS bigint
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog AS $$
    WITH d AS (DELETE FROM control.outcome_events WHERE received_at < now() - interval '90 days' RETURNING 1)
    SELECT count(*) FROM d
$$;
REVOKE ALL ON FUNCTION control.outcome_events_prune() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.outcome_events_prune() TO {{alerts}};
