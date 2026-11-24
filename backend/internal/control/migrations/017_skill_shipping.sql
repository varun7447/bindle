-- Learning loop, phase 2: prove skills by replaying the failures they were
-- written for, ship approved skills to agents that opted in, measure each
-- shipped version and roll a worse one back. Behind the learning_loop
-- feature, checked by the services.

-- A skill must fix this share (percent) of the replayed failures.
ALTER TABLE control.learning_settings
    ADD COLUMN proof_threshold integer NOT NULL DEFAULT 80 CHECK (proof_threshold BETWEEN 1 AND 100);

CREATE FUNCTION control.proof_threshold_for(t text) RETURNS integer
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT COALESCE((SELECT s.proof_threshold FROM control.learning_settings s WHERE s.tenant_id = t), 80)
$$;

-- Recorded runs (cassettes) of each lesson's evidence, built by the alerts
-- worker from stored (already redacted) steps, for bindle skills prove.
-- They go when their lesson goes.
CREATE TABLE control.lesson_cassettes (
    tenant_id   text        NOT NULL,
    lesson_id   text        NOT NULL,
    trace_id    text        NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$'),
    cassette    jsonb       NOT NULL,
    computed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, lesson_id, trace_id),
    FOREIGN KEY (tenant_id, lesson_id) REFERENCES control.lessons (tenant_id, id) ON DELETE CASCADE
);
ALTER TABLE control.lesson_cassettes ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.lesson_cassettes
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- Evidence runs (the first n of each lesson) without a cassette yet.
CREATE FUNCTION control.lesson_cassettes_missing(t text, n integer)
RETURNS TABLE (lesson_id text, trace_id text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT l.id, e.trace_id
    FROM control.lessons l, LATERAL unnest(l.evidence[1:n]) AS e(trace_id)
    WHERE l.tenant_id = t
      AND NOT EXISTS (SELECT 1 FROM control.lesson_cassettes c
                      WHERE c.tenant_id = t AND c.lesson_id = l.id AND c.trace_id = e.trace_id)
    ORDER BY l.id, e.trace_id
$$;
CREATE FUNCTION control.lesson_cassette_put(t text, lesson text, trace text, c jsonb) RETURNS boolean
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $$
    INSERT INTO control.lesson_cassettes (tenant_id, lesson_id, trace_id, cassette)
    SELECT t, lesson, trace, c WHERE EXISTS (SELECT 1 FROM control.lessons l WHERE l.tenant_id = t AND l.id = lesson)
    ON CONFLICT (tenant_id, lesson_id, trace_id) DO UPDATE SET cassette = EXCLUDED.cassette, computed_at = now()
    RETURNING true
$$;
CREATE FUNCTION control.lesson_cassettes_for(t text, lesson text) RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT COALESCE(jsonb_agg(c.cassette ORDER BY c.trace_id), '[]'::jsonb)
    FROM control.lesson_cassettes c WHERE c.tenant_id = t AND c.lesson_id = lesson
$$;
-- The lesson each skill was written for, for measuring recurrence.
CREATE FUNCTION control.lessons_brief(t text)
RETURNS TABLE (id text, agent text, step_name text, tool_name text, error_pattern text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT l.id, l.agent, l.step_name, l.tool_name, l.error_pattern FROM control.lessons l WHERE l.tenant_id = t
$$;

-- Proof reports, append-only. verdict is the server's judgement against
-- the workspace's threshold at upload time.
CREATE TABLE control.skill_proofs (
    tenant_id   text        NOT NULL REFERENCES control.tenants (id),
    id          bigserial,
    agent       text        NOT NULL,
    path        text        NOT NULL,
    sha256      text        NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    report      jsonb       NOT NULL,
    verdict     jsonb       NOT NULL,
    passed      boolean     NOT NULL,
    uploaded_by text        NOT NULL,
    uploaded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX skill_proofs_note ON control.skill_proofs (tenant_id, agent, path, uploaded_at DESC);
ALTER TABLE control.skill_proofs ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.skill_proofs
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
CREATE FUNCTION control.skill_proofs_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'control.skill_proofs: proof reports are append-only';
END
$$;
CREATE TRIGGER skill_proofs_append_only BEFORE UPDATE OR DELETE ON control.skill_proofs
    FOR EACH ROW EXECUTE FUNCTION control.skill_proofs_immutable();

-- Releases: each approval ships the exact approved text as the next
-- release of that skill. Agents get the newest active release of each
-- skill. A rollback marks a release rolled_back (the one before it is
-- served again); retiring the skill withdraws all of them.
CREATE TABLE control.skill_releases (
    tenant_id    text        NOT NULL REFERENCES control.tenants (id),
    agent        text        NOT NULL,
    path         text        NOT NULL,
    release      integer     NOT NULL CHECK (release > 0),
    sha256       text        NOT NULL,
    body         text        NOT NULL,
    released_by  text        NOT NULL,
    released_at  timestamptz NOT NULL DEFAULT now(),
    state        text        NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'rolled_back', 'withdrawn')),
    state_reason text,
    state_at     timestamptz,
    PRIMARY KEY (tenant_id, agent, path, release)
);
ALTER TABLE control.skill_releases ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.skill_releases
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- Moves a skill to another status (replaces 016's). New here: approving
-- needs a passing proof for this exact text and ships it as a release;
-- retiring withdraws every release of the skill.
CREATE OR REPLACE FUNCTION control.vault_set_status(t text, a text, p text, from_status text, to_status text, at_sha text,
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
    IF to_status IN ('proven', 'approved') AND NOT EXISTS (
            SELECT 1 FROM control.skill_proofs s
            WHERE s.tenant_id = t AND s.agent = a AND s.path = p AND s.sha256 = at_sha AND s.passed) THEN
        RAISE EXCEPTION 'vault_set_status: % needs a passing proof for this text', to_status;
    END IF;
    UPDATE control.vault_notes n SET status = to_status, version = cur.version + 1, updated_by = by_whom, updated_at = now()
    WHERE n.tenant_id = t AND n.agent = a AND n.path = p;
    INSERT INTO control.vault_versions (tenant_id, agent, path, version, kind, status, action, sha256, body, edited_by)
    VALUES (t, a, p, cur.version + 1, cur.kind, to_status, act, cur.sha256, cur.body, by_whom);
    IF to_status = 'approved' THEN
        INSERT INTO control.skill_releases (tenant_id, agent, path, release, sha256, body, released_by)
        SELECT t, a, p, COALESCE(max(r.release), 0) + 1, cur.sha256, cur.body, by_whom
        FROM control.skill_releases r WHERE r.tenant_id = t AND r.agent = a AND r.path = p;
    ELSIF to_status = 'retired' THEN
        UPDATE control.skill_releases r SET state = 'withdrawn', state_reason = 'retired by ' || by_whom, state_at = now()
        WHERE r.tenant_id = t AND r.agent = a AND r.path = p AND r.state = 'active';
    END IF;
    RETURN QUERY SELECT cur.version + 1, true;
END
$$;

-- Records a proof report; a passing one moves a draft at that exact text
-- to proven. Returns whether it did.
CREATE FUNCTION control.skill_proof_add(t text, a text, p text, at_sha text, rep jsonb, verd jsonb, did_pass boolean, by_whom text)
RETURNS TABLE (proven boolean, version integer, status text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    cur control.vault_notes%ROWTYPE;
    r record;
BEGIN
    SELECT * INTO cur FROM control.vault_notes n WHERE n.tenant_id = t AND n.agent = a AND n.path = p;
    IF NOT FOUND OR cur.kind <> 'skill' THEN
        RAISE EXCEPTION 'skill_proof_add: no such skill' USING ERRCODE = 'no_data_found';
    END IF;
    IF cur.sha256 <> at_sha THEN
        RAISE EXCEPTION 'skill_proof_add: the skill changed since this text was proven' USING ERRCODE = 'serialization_failure';
    END IF;
    INSERT INTO control.skill_proofs (tenant_id, agent, path, sha256, report, verdict, passed, uploaded_by)
    VALUES (t, a, p, at_sha, rep, verd, did_pass, by_whom);
    IF did_pass AND cur.status = 'draft' THEN
        SELECT * INTO r FROM control.vault_set_status(t, a, p, 'draft', 'proven', at_sha, 'prove', by_whom);
        RETURN QUERY SELECT r.ok, r.version, CASE WHEN r.ok THEN 'proven' ELSE cur.status END;
        RETURN;
    END IF;
    RETURN QUERY SELECT false, cur.version, cur.status;
END
$$;

-- Per agent: whether approved skills ship to it (opt-in, off by default),
-- and the kill switch for all of its learned skills.
CREATE TABLE control.agent_skill_settings (
    tenant_id   text        NOT NULL REFERENCES control.tenants (id),
    agent       text        NOT NULL CHECK (length(agent) BETWEEN 1 AND 128),
    ship        boolean     NOT NULL DEFAULT false,
    kill_all    boolean     NOT NULL DEFAULT false,
    updated_by  text,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, agent)
);
ALTER TABLE control.agent_skill_settings ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.agent_skill_settings
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- The kill switch for one skill.
CREATE TABLE control.skill_kills (
    tenant_id   text        NOT NULL REFERENCES control.tenants (id),
    agent       text        NOT NULL,
    path        text        NOT NULL,
    killed_by   text,
    killed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, agent, path)
);
ALTER TABLE control.skill_kills ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.skill_kills
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- What GET /v1/skills serves an agent: the newest active release of each
-- skill, only if the agent opted in and nothing is killed.
CREATE FUNCTION control.skills_for(t text, a text)
RETURNS TABLE (path text, release integer, sha256 text, body text, released_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT DISTINCT ON (r.path) r.path, r.release, r.sha256, r.body, r.released_at
    FROM control.skill_releases r
    JOIN control.agent_skill_settings s ON s.tenant_id = r.tenant_id AND s.agent = r.agent AND s.ship AND NOT s.kill_all
    WHERE r.tenant_id = t AND r.agent = a AND r.state = 'active'
      AND NOT EXISTS (SELECT 1 FROM control.skill_kills k WHERE k.tenant_id = t AND k.agent = a AND k.path = r.path)
    ORDER BY r.path, r.release DESC
$$;

-- Releases being measured: the one each opted-in agent gets, per skill.
CREATE FUNCTION control.shipping_releases()
RETURNS TABLE (tenant_id text, agent text, path text, release integer, sha256 text, body text, released_at timestamptz,
               note_status text, note_sha text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT x.tenant_id, x.agent, x.path, x.release, x.sha256, x.body, x.released_at, n.status, n.sha256
    FROM (SELECT DISTINCT ON (r.tenant_id, r.agent, r.path) r.*
          FROM control.skill_releases r
          JOIN control.tenant_features f ON f.tenant_id = r.tenant_id AND f.feature = 'learning_loop'
          JOIN control.agent_skill_settings s ON s.tenant_id = r.tenant_id AND s.agent = r.agent AND s.ship
          WHERE r.state = 'active'
          ORDER BY r.tenant_id, r.agent, r.path, r.release DESC) x
    LEFT JOIN control.vault_notes n ON n.tenant_id = x.tenant_id AND n.agent = x.agent AND n.path = x.path
    ORDER BY 1, 2, 3
$$;

CREATE TABLE control.skill_measurements (
    tenant_id   text        NOT NULL REFERENCES control.tenants (id),
    agent       text        NOT NULL,
    path        text        NOT NULL,
    release     integer     NOT NULL,
    measurement jsonb       NOT NULL,
    decision    text        NOT NULL,
    computed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, agent, path, release)
);
ALTER TABLE control.skill_measurements ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.skill_measurements
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
CREATE FUNCTION control.skill_measure_put(t text, a text, p text, rel integer, m jsonb, d text) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $$
    INSERT INTO control.skill_measurements (tenant_id, agent, path, release, measurement, decision)
    VALUES (t, a, p, rel, m, d)
    ON CONFLICT (tenant_id, agent, path, release) DO UPDATE SET measurement = EXCLUDED.measurement,
        decision = EXCLUDED.decision, computed_at = now()
$$;
-- Rolls a release back (only an active one, once).
CREATE FUNCTION control.skill_rollback(t text, a text, p text, rel integer, why text) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    n integer;
BEGIN
    UPDATE control.skill_releases r SET state = 'rolled_back', state_reason = why, state_at = now()
    WHERE r.tenant_id = t AND r.agent = a AND r.path = p AND r.release = rel AND r.state = 'active';
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n = 1;
END
$$;
-- Marks an approved skill live once agents are loading its release.
CREATE FUNCTION control.skill_go_live(t text, a text, p text, at_sha text) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    r record;
BEGIN
    SELECT * INTO r FROM control.vault_set_status(t, a, p, 'approved', 'live', at_sha, 'live', 'bindle');
    RETURN CASE WHEN r.ok THEN r.version ELSE 0 END;
END
$$;

-- Ingest reads config through this function only. It now also returns the
-- kill switches for learned skills ("<agent>/<path>", or "<agent>/*" for
-- every skill of an agent that opted out or was killed), at most 500.
DROP FUNCTION control.sdk_config_for(text);
CREATE FUNCTION control.sdk_config_for(t text)
RETURNS TABLE (version bigint, sample_rate double precision, capture_content boolean, redact_patterns jsonb,
               limits jsonb, over_budget text[], prices jsonb, updated_at timestamptz, stopped_runs text[],
               killed_skills text[])
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT COALESCE(c.version, 0), COALESCE(c.sample_rate, 1), COALESCE(c.capture_content, true),
           COALESCE(c.redact_patterns, '[]'::jsonb), COALESCE(c.limits, '[]'::jsonb),
           ARRAY(SELECT b.agent FROM control.budget_state b
                 WHERE b.tenant_id = t AND b.day = (now() AT TIME ZONE 'UTC')::date ORDER BY b.agent),
           COALESCE(c.prices, '[]'::jsonb), COALESCE(c.updated_at, 'epoch'::timestamptz),
           ARRAY(SELECT s.trace_id FROM control.run_stops s
                 WHERE s.tenant_id = t AND s.stopped_at > now() - interval '1 hour'
                 ORDER BY s.stopped_at DESC, s.trace_id LIMIT 200),
           ARRAY(SELECT k FROM (
                     SELECT a.agent || '/*' AS k FROM control.agent_skill_settings a
                     WHERE a.tenant_id = t AND (a.kill_all OR NOT a.ship)
                     UNION
                     SELECT k.agent || '/' || k.path FROM control.skill_kills k WHERE k.tenant_id = t
                 ) x ORDER BY k LIMIT 500)
    FROM (SELECT t AS tenant_id) x LEFT JOIN control.sdk_config c ON c.tenant_id = x.tenant_id
$$;
REVOKE ALL ON FUNCTION control.sdk_config_for(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.sdk_config_for(text) TO {{ingest}};

REVOKE ALL ON FUNCTION control.proof_threshold_for(text), control.lesson_cassettes_missing(text, integer),
    control.lesson_cassette_put(text, text, text, jsonb), control.lesson_cassettes_for(text, text),
    control.lessons_brief(text), control.skill_proof_add(text, text, text, text, jsonb, jsonb, boolean, text),
    control.skills_for(text, text), control.shipping_releases(),
    control.skill_measure_put(text, text, text, integer, jsonb, text), control.skill_rollback(text, text, text, integer, text),
    control.skill_go_live(text, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.lesson_cassettes_missing(text, integer), control.lesson_cassette_put(text, text, text, jsonb),
    control.lessons_brief(text), control.shipping_releases(), control.skill_measure_put(text, text, text, integer, jsonb, text),
    control.skill_rollback(text, text, text, integer, text), control.skill_go_live(text, text, text, text) TO {{alerts}};
GRANT EXECUTE ON FUNCTION control.proof_threshold_for(text), control.lesson_cassettes_for(text, text),
    control.skill_proof_add(text, text, text, text, jsonb, jsonb, boolean, text), control.skills_for(text, text) TO {{ingest}};
GRANT SELECT ON control.skill_proofs, control.skill_releases, control.skill_measurements,
    control.agent_skill_settings, control.skill_kills TO {{api}};
GRANT INSERT ON control.agent_skill_settings TO {{api}};
GRANT UPDATE (ship, kill_all, updated_by, updated_at) ON control.agent_skill_settings TO {{api}};
GRANT INSERT, DELETE ON control.skill_kills TO {{api}};
GRANT UPDATE (proof_threshold) ON control.learning_settings TO {{api}};
