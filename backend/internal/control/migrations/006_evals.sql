-- Evals: checks (deterministic scorers), datasets saved from runs, and
-- results of running the checks on a dataset. Model-graded scores are not
-- stored here: they arrive as eval steps from bindle.score() in the SDKs.
CREATE TABLE control.scorers (
    id              text             PRIMARY KEY,
    tenant_id       text             NOT NULL REFERENCES control.tenants (id),
    name            text             NOT NULL,
    kind            text             NOT NULL,
    agent           text             NOT NULL DEFAULT '',
    value           text             NOT NULL DEFAULT '',
    number          double precision NOT NULL DEFAULT 0,
    case_sensitive  boolean          NOT NULL DEFAULT false,
    enabled         boolean          NOT NULL DEFAULT true,
    created_by      text             REFERENCES control.users (id),
    created_at      timestamptz      NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE control.datasets (
    id           text        PRIMARY KEY,
    tenant_id    text        NOT NULL REFERENCES control.tenants (id),
    name         text        NOT NULL,
    description  text        NOT NULL DEFAULT '',
    created_by   text        REFERENCES control.users (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name),
    UNIQUE (id, tenant_id)
);

-- A copy of each run's input, output and facts: datasets outlive trace
-- retention. Content is already redacted (it was stored redacted).
CREATE TABLE control.dataset_items (
    dataset_id   text             NOT NULL,
    tenant_id    text             NOT NULL,
    trace_id     text             NOT NULL,
    agent        text             NOT NULL DEFAULT '',
    version      text             NOT NULL DEFAULT '',
    input        text             NOT NULL DEFAULT '',
    output       text             NOT NULL DEFAULT '',
    tools        jsonb            NOT NULL DEFAULT '[]',
    steps        bigint           NOT NULL DEFAULT 0,
    cost_usd     double precision NOT NULL DEFAULT 0,
    cost_known   boolean          NOT NULL DEFAULT false,
    duration_ms  double precision NOT NULL DEFAULT 0,
    failed       boolean          NOT NULL DEFAULT false,
    added_at     timestamptz      NOT NULL DEFAULT now(),
    PRIMARY KEY (dataset_id, trace_id),
    FOREIGN KEY (dataset_id, tenant_id) REFERENCES control.datasets (id, tenant_id) ON DELETE CASCADE
);

CREATE TABLE control.eval_runs (
    id          text        PRIMARY KEY,
    tenant_id   text        NOT NULL,
    dataset_id  text        NOT NULL,
    items       integer     NOT NULL,
    tallies     jsonb       NOT NULL DEFAULT '[]',
    failures    jsonb       NOT NULL DEFAULT '[]', -- failed results only, capped
    created_by  text        REFERENCES control.users (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (dataset_id, tenant_id) REFERENCES control.datasets (id, tenant_id) ON DELETE CASCADE
);
CREATE INDEX eval_runs_dataset ON control.eval_runs (dataset_id, created_at DESC);

ALTER TABLE control.scorers ENABLE ROW LEVEL SECURITY;
ALTER TABLE control.datasets ENABLE ROW LEVEL SECURITY;
ALTER TABLE control.dataset_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE control.eval_runs ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.scorers
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
CREATE POLICY tenant_isolation ON control.datasets
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
CREATE POLICY tenant_isolation ON control.dataset_items
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
CREATE POLICY tenant_isolation ON control.eval_runs
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

GRANT SELECT, INSERT, DELETE ON control.scorers TO {{api}};
GRANT UPDATE (name, agent, value, number, case_sensitive, enabled) ON control.scorers TO {{api}};
GRANT SELECT, INSERT, DELETE ON control.datasets, control.dataset_items TO {{api}};
GRANT UPDATE (name, description) ON control.datasets TO {{api}};
GRANT SELECT, INSERT ON control.eval_runs TO {{api}};
