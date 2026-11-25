-- Learning loop, phase 1: lessons, drafted skills and the knowledge vault.
-- Nothing here reaches agents. Everything is behind the learning_loop
-- feature, checked by the services.

-- Per-workspace settings.
CREATE TABLE control.learning_settings (
    tenant_id   text        PRIMARY KEY REFERENCES control.tenants (id),
    min_runs    integer     NOT NULL DEFAULT 5 CHECK (min_runs BETWEEN 2 AND 1000),
    updated_by  text,
    updated_at  timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE control.learning_settings ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.learning_settings
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- Lessons, recomputed hourly by the alerts worker from failure groups.
-- pack is the lesson pack: redacted normalized data only.
CREATE TABLE control.lessons (
    tenant_id     text        NOT NULL REFERENCES control.tenants (id),
    id            text        NOT NULL CHECK (id ~ '^les_[0-9a-f]{16}$'),
    agent         text        NOT NULL,
    step_name     text        NOT NULL,
    step_type     text        NOT NULL,
    tool_name     text        NOT NULL DEFAULT '',
    error_pattern text        NOT NULL,
    cause         text        NOT NULL,
    title         text        NOT NULL,
    runs          integer     NOT NULL CHECK (runs > 0),
    failures      integer     NOT NULL CHECK (failures > 0),
    first_seen    timestamptz NOT NULL,
    last_seen     timestamptz NOT NULL,
    evidence      text[]      NOT NULL,
    pack          jsonb       NOT NULL,
    computed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX lessons_agent ON control.lessons (tenant_id, agent, runs DESC);
ALTER TABLE control.lessons ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.lessons
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- Tool names each agent has called recently (from the alerts worker), so a
-- skill's mentions of tools can be checked against the passport.
CREATE TABLE control.agent_tools (
    tenant_id   text        NOT NULL REFERENCES control.tenants (id),
    agent       text        NOT NULL,
    tools       text[]      NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, agent)
);
ALTER TABLE control.agent_tools ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.agent_tools
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- Workspaces with the learning loop on, with their threshold and whether
-- prompts and outputs are captured (lesson packs carry content only then).
CREATE FUNCTION control.learning_tenants()
RETURNS TABLE (tenant_id text, min_runs integer, capture boolean)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT f.tenant_id, COALESCE(s.min_runs, 5), COALESCE(c.capture_content, true)
    FROM control.tenant_features f
    LEFT JOIN control.learning_settings s ON s.tenant_id = f.tenant_id
    LEFT JOIN control.sdk_config c ON c.tenant_id = f.tenant_id
    WHERE f.feature = 'learning_loop'
    ORDER BY 1
$$;

-- Replaces a workspace's lessons and observed tools with a fresh
-- computation. rows: [{id, agent, step_name, ...}], tools: {agent: [names]}.
CREATE FUNCTION control.lessons_replace(t text, rows jsonb, tools jsonb) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    n integer;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('lessons/' || t, 0));
    DELETE FROM control.lessons l WHERE l.tenant_id = t
        AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(rows) r WHERE r->>'id' = l.id);
    INSERT INTO control.lessons (tenant_id, id, agent, step_name, step_type, tool_name, error_pattern, cause, title,
                                 runs, failures, first_seen, last_seen, evidence, pack, computed_at)
    SELECT t, r.id, r.agent, r.step_name, r.step_type, COALESCE(r.tool_name, ''), r.error_pattern, r.cause, r.title,
           r.runs, r.failures, r.first_seen, r.last_seen, COALESCE(r.evidence, '{}'), r.pack, now()
    FROM jsonb_to_recordset(rows) AS r(id text, agent text, step_name text, step_type text, tool_name text, error_pattern text,
                                       cause text, title text, runs integer, failures integer, first_seen timestamptz,
                                       last_seen timestamptz, evidence text[], pack jsonb)
    ON CONFLICT (tenant_id, id) DO UPDATE SET agent = EXCLUDED.agent, step_name = EXCLUDED.step_name,
        step_type = EXCLUDED.step_type, tool_name = EXCLUDED.tool_name, error_pattern = EXCLUDED.error_pattern,
        cause = EXCLUDED.cause, title = EXCLUDED.title, runs = EXCLUDED.runs, failures = EXCLUDED.failures,
        first_seen = EXCLUDED.first_seen, last_seen = EXCLUDED.last_seen, evidence = EXCLUDED.evidence,
        pack = EXCLUDED.pack, computed_at = EXCLUDED.computed_at;
    GET DIAGNOSTICS n = ROW_COUNT;
    DELETE FROM control.agent_tools WHERE tenant_id = t;
    INSERT INTO control.agent_tools (tenant_id, agent, tools)
    SELECT t, k, ARRAY(SELECT jsonb_array_elements_text(v)) FROM jsonb_each(COALESCE(tools, '{}'::jsonb)) AS e(k, v);
    RETURN n;
END
$$;

-- Ingest serves lessons and packs to the CLI (management key) through these.
CREATE FUNCTION control.lessons_for(t text, a text)
RETURNS TABLE (id text, agent text, step_name text, tool_name text, error_pattern text, cause text, title text,
               runs integer, failures integer, last_seen timestamptz, evidence text[])
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT l.id, l.agent, l.step_name, l.tool_name, l.error_pattern, l.cause, l.title, l.runs, l.failures, l.last_seen, l.evidence
    FROM control.lessons l WHERE l.tenant_id = t AND (a IS NULL OR l.agent = a)
    ORDER BY l.runs DESC, l.id LIMIT 200
$$;
CREATE FUNCTION control.lesson_pack(t text, lesson text) RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT l.pack FROM control.lessons l WHERE l.tenant_id = t AND l.id = lesson
$$;
CREATE FUNCTION control.agent_tools_for(t text, a text) RETURNS text[]
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT COALESCE((SELECT x.tools FROM control.agent_tools x WHERE x.tenant_id = t AND x.agent = a), '{}')
$$;

-- The knowledge vault: Markdown notes per agent. notes is the current
-- state, versions the full history (append-only), links the [[link]] index
-- for backlinks. Skills (kind skill) carry a status; other notes don't.
CREATE TABLE control.vault_notes (
    tenant_id       text        NOT NULL REFERENCES control.tenants (id),
    agent           text        NOT NULL CHECK (length(agent) BETWEEN 1 AND 128),
    path            text        NOT NULL CHECK (length(path) BETWEEN 4 AND 200),
    kind            text        NOT NULL CHECK (kind IN ('skill', 'lesson', 'incident', 'note')),
    status          text        CHECK (status IN ('draft', 'proven', 'approved', 'live', 'retired')),
    version         integer     NOT NULL CHECK (version > 0),
    sha256          text        NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    body            text        NOT NULL,
    scan            jsonb       NOT NULL DEFAULT '{}',
    passport_check  jsonb       NOT NULL DEFAULT '{}',
    reviewed_sha    text,       -- a security reviewer cleared the scan for this text
    reviewed_by     text,
    updated_by      text        NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, agent, path),
    CHECK ((kind = 'skill') = (status IS NOT NULL))
);
ALTER TABLE control.vault_notes ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.vault_notes
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

CREATE TABLE control.vault_versions (
    tenant_id   text        NOT NULL REFERENCES control.tenants (id),
    agent       text        NOT NULL,
    path        text        NOT NULL,
    version     integer     NOT NULL CHECK (version > 0),
    kind        text        NOT NULL,
    status      text,
    action      text        NOT NULL,   -- create, edit, approve, retire, restore, prove, review
    sha256      text        NOT NULL,
    body        text        NOT NULL,
    edited_by   text        NOT NULL,
    edited_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, agent, path, version)
);
ALTER TABLE control.vault_versions ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.vault_versions
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
CREATE FUNCTION control.vault_versions_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'control.vault_versions: note history is append-only';
END
$$;
CREATE TRIGGER vault_versions_append_only BEFORE UPDATE OR DELETE ON control.vault_versions
    FOR EACH ROW EXECUTE FUNCTION control.vault_versions_immutable();
CREATE TRIGGER vault_versions_no_truncate BEFORE TRUNCATE ON control.vault_versions
    FOR EACH STATEMENT EXECUTE FUNCTION control.vault_versions_immutable();

CREATE TABLE control.vault_links (
    tenant_id   text NOT NULL REFERENCES control.tenants (id),
    agent       text NOT NULL,
    path        text NOT NULL,
    target      text NOT NULL,
    PRIMARY KEY (tenant_id, agent, path, target)
);
CREATE INDEX vault_links_target ON control.vault_links (tenant_id, agent, target);
ALTER TABLE control.vault_links ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.vault_links
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- Saves a note. base_sha is the text the writer started from ('' for a new
-- note); a different current text is a conflict and nothing is saved.
-- Saving a skill makes it a draft again: an edit voids its proof and
-- approval, and the security review applies to the reviewed text only.
CREATE FUNCTION control.vault_put(t text, a text, p text, k text, new_body text, new_sha text, base_sha text,
                                  new_scan jsonb, new_check jsonb, new_links text[], by_whom text)
RETURNS TABLE (version integer, conflict boolean, unchanged boolean, current_sha text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    cur control.vault_notes%ROWTYPE;
    next_version integer;
    st text;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('vault/' || t || '/' || a || '/' || p, 0));
    SELECT * INTO cur FROM control.vault_notes n WHERE n.tenant_id = t AND n.agent = a AND n.path = p;
    IF FOUND AND cur.sha256 = new_sha THEN
        RETURN QUERY SELECT cur.version, false, true, cur.sha256;
        RETURN;
    END IF;
    IF (FOUND AND cur.sha256 <> base_sha) OR (NOT FOUND AND base_sha <> '') THEN
        RETURN QUERY SELECT COALESCE(cur.version, 0), true, false, COALESCE(cur.sha256, '');
        RETURN;
    END IF;
    IF FOUND AND cur.kind <> k THEN
        RAISE EXCEPTION 'vault_put: a note cannot change kind';
    END IF;
    next_version := COALESCE(cur.version, 0) + 1;
    st := CASE WHEN k = 'skill' THEN 'draft' END;
    INSERT INTO control.vault_notes (tenant_id, agent, path, kind, status, version, sha256, body, scan, passport_check,
                                     reviewed_sha, reviewed_by, updated_by, updated_at)
    VALUES (t, a, p, k, st, next_version, new_sha, new_body, new_scan, new_check, NULL, NULL, by_whom, now())
    ON CONFLICT (tenant_id, agent, path) DO UPDATE SET status = EXCLUDED.status, version = EXCLUDED.version,
        sha256 = EXCLUDED.sha256, body = EXCLUDED.body, scan = EXCLUDED.scan, passport_check = EXCLUDED.passport_check,
        reviewed_sha = NULL, reviewed_by = NULL, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at;
    INSERT INTO control.vault_versions (tenant_id, agent, path, version, kind, status, action, sha256, body, edited_by)
    VALUES (t, a, p, next_version, k, st, CASE WHEN cur.version IS NULL THEN 'create' ELSE 'edit' END, new_sha, new_body, by_whom);
    DELETE FROM control.vault_links l WHERE l.tenant_id = t AND l.agent = a AND l.path = p;
    INSERT INTO control.vault_links (tenant_id, agent, path, target)
    SELECT DISTINCT t, a, p, x FROM unnest(COALESCE(new_links, '{}')) AS x;
    RETURN QUERY SELECT next_version, false, false, new_sha;
END
$$;

-- Moves a skill to another status. The caller has checked who may do it;
-- this enforces the transition itself, against the exact text (sha) the
-- caller saw, so a concurrent edit can't be approved by accident.
CREATE FUNCTION control.vault_set_status(t text, a text, p text, from_status text, to_status text, at_sha text,
                                         act text, by_whom text)
RETURNS TABLE (version integer, ok boolean)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    cur control.vault_notes%ROWTYPE;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('vault/' || t || '/' || a || '/' || p, 0));
    SELECT * INTO cur FROM control.vault_notes n WHERE n.tenant_id = t AND n.agent = a AND n.path = p FOR UPDATE;
    IF NOT FOUND OR cur.kind <> 'skill' OR cur.status <> from_status OR cur.sha256 <> at_sha THEN
        RETURN QUERY SELECT COALESCE(cur.version, 0), false;
        RETURN;
    END IF;
    IF NOT ((from_status = 'draft' AND to_status = 'proven') OR (from_status = 'proven' AND to_status = 'approved')
            OR (from_status = 'approved' AND to_status = 'live') OR (from_status = 'retired' AND to_status = 'draft')
            OR (from_status <> 'retired' AND to_status = 'retired')) THEN
        RAISE EXCEPTION 'vault_set_status: % -> % is not allowed', from_status, to_status;
    END IF;
    UPDATE control.vault_notes n SET status = to_status, version = cur.version + 1, updated_by = by_whom, updated_at = now()
    WHERE n.tenant_id = t AND n.agent = a AND n.path = p;
    INSERT INTO control.vault_versions (tenant_id, agent, path, version, kind, status, action, sha256, body, edited_by)
    VALUES (t, a, p, cur.version + 1, cur.kind, to_status, act, cur.sha256, cur.body, by_whom);
    RETURN QUERY SELECT cur.version + 1, true;
END
$$;

-- A security reviewer clears a flagged skill's scan for its current text.
CREATE FUNCTION control.vault_review(t text, a text, p text, at_sha text, by_whom text) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    n integer;
BEGIN
    UPDATE control.vault_notes v SET reviewed_sha = at_sha, reviewed_by = by_whom
    WHERE v.tenant_id = t AND v.agent = a AND v.path = p AND v.sha256 = at_sha;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n = 1;
END
$$;

-- Vault sync (CLI, management key): every note of an agent.
CREATE FUNCTION control.vault_list(t text, a text)
RETURNS TABLE (path text, kind text, status text, version integer, sha256 text, body text, updated_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT n.path, n.kind, n.status, n.version, n.sha256, n.body, n.updated_at
    FROM control.vault_notes n WHERE n.tenant_id = t AND n.agent = a ORDER BY n.path
$$;

-- Security reviewers: people who may clear a flagged skill's scan.
CREATE TABLE control.security_reviewers (
    tenant_id   text        NOT NULL REFERENCES control.tenants (id),
    user_id     text        NOT NULL REFERENCES control.users (id),
    added_by    text,
    added_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);
ALTER TABLE control.security_reviewers ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.security_reviewers
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

REVOKE ALL ON FUNCTION control.learning_tenants(), control.lessons_replace(text, jsonb, jsonb),
    control.lessons_for(text, text), control.lesson_pack(text, text), control.agent_tools_for(text, text),
    control.vault_put(text, text, text, text, text, text, text, jsonb, jsonb, text[], text),
    control.vault_set_status(text, text, text, text, text, text, text, text),
    control.vault_review(text, text, text, text, text), control.vault_list(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.learning_tenants(), control.lessons_replace(text, jsonb, jsonb) TO {{alerts}};
GRANT EXECUTE ON FUNCTION control.lessons_for(text, text), control.lesson_pack(text, text), control.agent_tools_for(text, text),
    control.vault_put(text, text, text, text, text, text, text, jsonb, jsonb, text[], text),
    control.vault_list(text, text) TO {{ingest}};
GRANT EXECUTE ON FUNCTION control.agent_tools_for(text, text),
    control.vault_put(text, text, text, text, text, text, text, jsonb, jsonb, text[], text),
    control.vault_set_status(text, text, text, text, text, text, text, text),
    control.vault_review(text, text, text, text, text) TO {{api}};
GRANT SELECT ON control.lessons, control.agent_tools, control.vault_notes, control.vault_versions, control.vault_links TO {{api}};
GRANT SELECT, INSERT ON control.learning_settings TO {{api}};
GRANT UPDATE (min_runs, updated_by, updated_at) ON control.learning_settings TO {{api}};
GRANT SELECT, INSERT, DELETE ON control.security_reviewers TO {{api}};
-- Console edits of the vault are written to the agent's ledger too.
GRANT EXECUTE ON FUNCTION control.ledger_lock_head(text, text), control.ledger_known(text, text[], text[], text[], text[]),
    control.ledger_append(text, text, bigint, jsonb) TO {{api}};
