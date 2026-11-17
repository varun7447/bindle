-- Alerting: rules, delivery channels and fired events, per tenant.
CREATE TABLE control.alert_rules (
    id              text             PRIMARY KEY,
    tenant_id       text             NOT NULL REFERENCES control.tenants (id),
    kind            text             NOT NULL CHECK (kind IN ('error_rate', 'runaway_loop', 'run_cost')),
    threshold       double precision NOT NULL CHECK (threshold > 0),
    window_minutes  integer          NOT NULL CHECK (window_minutes BETWEEN 5 AND 1440),
    min_runs        integer          NOT NULL DEFAULT 0 CHECK (min_runs >= 0),
    enabled         boolean          NOT NULL DEFAULT true,
    created_by      text             REFERENCES control.users (id),
    created_at      timestamptz      NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_id)
);
CREATE INDEX alert_rules_tenant ON control.alert_rules (tenant_id);

-- target: a Slack incoming-webhook URL, an https webhook URL, or an email
-- address. secret signs webhook bodies (HMAC-SHA256).
CREATE TABLE control.alert_channels (
    id          text        PRIMARY KEY,
    tenant_id   text        NOT NULL REFERENCES control.tenants (id),
    kind        text        NOT NULL CHECK (kind IN ('slack', 'webhook', 'email')),
    name        text        NOT NULL,
    target      text        NOT NULL,
    secret      text        NOT NULL DEFAULT '',
    enabled     boolean     NOT NULL DEFAULT true,
    created_by  text        REFERENCES control.users (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX alert_channels_tenant ON control.alert_channels (tenant_id);

-- One row per fired alert. The unique fingerprint makes evaluation
-- idempotent: concurrent or repeated evaluators fire (and deliver) once.
CREATE TABLE control.alert_events (
    id           text        PRIMARY KEY,
    tenant_id    text        NOT NULL REFERENCES control.tenants (id),
    rule_id      text        NOT NULL,
    kind         text        NOT NULL,
    fingerprint  text        NOT NULL,
    trace_id     text        NOT NULL DEFAULT '',
    summary      text        NOT NULL,
    details      jsonb       NOT NULL DEFAULT '{}',
    delivery     jsonb       NOT NULL DEFAULT '{}',
    fired_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (rule_id, fingerprint),
    -- An event's rule must belong to the event's tenant.
    FOREIGN KEY (rule_id, tenant_id) REFERENCES control.alert_rules (id, tenant_id)
);
CREATE INDEX alert_events_tenant ON control.alert_events (tenant_id, fired_at DESC);

ALTER TABLE control.alert_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE control.alert_channels ENABLE ROW LEVEL SECURITY;
ALTER TABLE control.alert_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.alert_rules
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
CREATE POLICY tenant_isolation ON control.alert_channels
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
CREATE POLICY tenant_isolation ON control.alert_events
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- The evaluator finds tenants with enabled rules through this function,
-- then works inside each tenant under row-level security.
CREATE FUNCTION control.alerting_tenants() RETURNS SETOF text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT DISTINCT tenant_id FROM control.alert_rules WHERE enabled
$$;
REVOKE ALL ON FUNCTION control.alerting_tenants() FROM PUBLIC;

GRANT SELECT, INSERT ON control.alert_rules, control.alert_channels TO {{api}};
GRANT UPDATE (threshold, window_minutes, min_runs, enabled) ON control.alert_rules TO {{api}};
GRANT UPDATE (name, enabled) ON control.alert_channels TO {{api}};
-- Channels hold secrets (webhook URLs), so they can be deleted outright;
-- events refer to them only by id inside the delivery record.
GRANT DELETE ON control.alert_channels TO {{api}};
GRANT SELECT ON control.alert_events TO {{api}};

GRANT USAGE ON SCHEMA control TO {{alerts}};
GRANT EXECUTE ON FUNCTION control.alerting_tenants() TO {{alerts}};
GRANT SELECT ON control.alert_rules, control.alert_channels TO {{alerts}};
GRANT SELECT, INSERT ON control.alert_events TO {{alerts}};
GRANT UPDATE (delivery) ON control.alert_events TO {{alerts}};
