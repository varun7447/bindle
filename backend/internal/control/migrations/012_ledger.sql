-- Tamper-evident ledger: one append-only, hash-chained journal per tenant
-- and agent. Each entry is a compact summary of one already-redacted step
-- (never prompts or outputs). hash = SHA-256(prev_hash || canonical entry),
-- computed by the writer (internal/ledger) and re-checked on verify.
--
-- Entries are partitioned by the UTC day they were appended. Retention
-- records each closed day's final hash in ledger_segments, then drops the
-- whole day partition; single rows are never updated or deleted (triggers
-- refuse it for every role, including the owner).
--
-- App roles never touch the tables directly to write: ingest and the
-- processor append through the SECURITY DEFINER functions below; the API
-- reads with row-level security; the alerts worker runs maintenance.

CREATE TABLE control.ledger_entries (
    tenant_id       text        NOT NULL,
    agent           text        NOT NULL,
    seq             bigint      NOT NULL CHECK (seq > 0),
    day             date        NOT NULL,
    at              timestamptz NOT NULL,
    kind            text        NOT NULL CHECK (kind IN ('step', 'drift', 'skill_change', 'vault_change')),
    trace_id        text        NOT NULL DEFAULT '',
    step_id         text        NOT NULL DEFAULT '',
    entry_key       text        NOT NULL DEFAULT '',
    step_type       text        NOT NULL DEFAULT '',
    step_name       text        NOT NULL DEFAULT '',
    tool_name       text        NOT NULL DEFAULT '',
    status          text        NOT NULL DEFAULT '',
    error_class     text        NOT NULL DEFAULT '',
    input_tokens    bigint      NOT NULL DEFAULT 0,
    output_tokens   bigint      NOT NULL DEFAULT 0,
    cost_usd        numeric     NOT NULL DEFAULT 0,
    cost_known      boolean     NOT NULL DEFAULT false,
    security_flags  text[]      NOT NULL DEFAULT '{}',
    detail          text        NOT NULL DEFAULT '{}',
    prev_hash       bytea       NOT NULL CHECK (octet_length(prev_hash) = 32),
    hash            bytea       NOT NULL CHECK (octet_length(hash) = 32),
    PRIMARY KEY (tenant_id, agent, seq, day),
    -- A retried batch can't write the same entry twice on one day; the
    -- writer also checks every day before chaining (see ledger_known).
    UNIQUE (tenant_id, day, kind, trace_id, step_id, entry_key)
) PARTITION BY RANGE (day);
CREATE INDEX ledger_entries_key ON control.ledger_entries (tenant_id, trace_id, step_id, kind, entry_key);
CREATE INDEX ledger_entries_agent_at ON control.ledger_entries (tenant_id, agent, at DESC);

CREATE TABLE control.ledger_heads (
    tenant_id   text   NOT NULL,
    agent       text   NOT NULL,
    seq         bigint NOT NULL,
    hash        bytea  NOT NULL CHECK (octet_length(hash) = 32),
    PRIMARY KEY (tenant_id, agent)
);

CREATE TABLE control.ledger_segments (
    tenant_id   text        NOT NULL,
    agent       text        NOT NULL,
    day         date        NOT NULL,
    first_seq   bigint      NOT NULL,
    last_seq    bigint      NOT NULL,
    entries     bigint      NOT NULL,
    final_hash  bytea       NOT NULL CHECK (octet_length(final_hash) = 32),
    closed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, agent, day)
);
CREATE INDEX ledger_segments_last ON control.ledger_segments (tenant_id, agent, last_seq);

-- Days whose segments have been recorded.
CREATE TABLE control.ledger_closed_days (
    day         date        PRIMARY KEY,
    closed_at   timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE control.ledger_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE control.ledger_heads ENABLE ROW LEVEL SECURITY;
ALTER TABLE control.ledger_segments ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.ledger_entries
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
CREATE POLICY tenant_isolation ON control.ledger_heads
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
CREATE POLICY tenant_isolation ON control.ledger_segments
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));

-- Append-only: nobody updates, deletes or truncates entries or segments.
CREATE FUNCTION control.ledger_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'control.%: ledger records are append-only', TG_TABLE_NAME;
END
$$;
CREATE TRIGGER ledger_entries_append_only BEFORE UPDATE OR DELETE ON control.ledger_entries
    FOR EACH ROW EXECUTE FUNCTION control.ledger_immutable();
CREATE TRIGGER ledger_entries_no_truncate BEFORE TRUNCATE ON control.ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION control.ledger_immutable();
CREATE TRIGGER ledger_segments_append_only BEFORE UPDATE OR DELETE ON control.ledger_segments
    FOR EACH ROW EXECUTE FUNCTION control.ledger_immutable();
CREATE TRIGGER ledger_segments_no_truncate BEFORE TRUNCATE ON control.ledger_segments
    FOR EACH STATEMENT EXECUTE FUNCTION control.ledger_immutable();

-- Creates the partition for one day if it doesn't exist. Row triggers are
-- inherited from the parent; TRUNCATE triggers are not, so each partition
-- gets its own.
CREATE FUNCTION control.ledger_ensure_partition(d date) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    part text := 'ledger_entries_' || to_char(d, 'YYYYMMDD');
BEGIN
    IF to_regclass('control.' || part) IS NOT NULL THEN
        RETURN;
    END IF;
    PERFORM pg_advisory_xact_lock(7447_0012);
    IF to_regclass('control.' || part) IS NOT NULL THEN
        RETURN;
    END IF;
    EXECUTE format('CREATE TABLE control.%I PARTITION OF control.ledger_entries FOR VALUES FROM (%L) TO (%L)', part, d, d + 1);
    EXECUTE format('CREATE TRIGGER ledger_partition_no_truncate BEFORE TRUNCATE ON control.%I FOR EACH STATEMENT EXECUTE FUNCTION control.ledger_immutable()', part);
END
$$;

-- Writers: lock the agent's chain head (creating it at the genesis) and
-- learn today's UTC day, which is part of every entry appended in this
-- transaction. The row lock serializes concurrent writers for one agent.
CREATE FUNCTION control.ledger_lock_head(t text, a text)
RETURNS TABLE (seq bigint, hash bytea, day date)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
#variable_conflict use_column
BEGIN
    INSERT INTO control.ledger_heads (tenant_id, agent, seq, hash)
    VALUES (t, a, 0, '\x0000000000000000000000000000000000000000000000000000000000000000'::bytea)
    ON CONFLICT (tenant_id, agent) DO NOTHING;
    RETURN QUERY SELECT h.seq, h.hash, (now() AT TIME ZONE 'UTC')::date
        FROM control.ledger_heads h WHERE h.tenant_id = t AND h.agent = a FOR UPDATE;
END
$$;

-- Writers: which of these entry keys (kind, trace, step, entry key) are
-- already in the ledger, on any retained day. Called after ledger_lock_head.
CREATE FUNCTION control.ledger_known(t text, kinds text[], traces text[], steps text[], ekeys text[])
RETURNS TABLE (kind text, trace_id text, step_id text, entry_key text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT e.kind, e.trace_id, e.step_id, e.entry_key
    FROM control.ledger_entries e
    JOIN unnest(kinds, traces, steps, ekeys) AS k(kind, trace_id, step_id, entry_key)
      ON e.kind = k.kind AND e.trace_id = k.trace_id AND e.step_id = k.step_id AND e.entry_key = k.entry_key
    WHERE e.tenant_id = t
$$;

-- Writers: append chained entries and move the head. expect_seq must be the
-- head returned by ledger_lock_head in this transaction, and every entry's
-- day must be today, so a writer can't fork the chain or backdate a day.
CREATE FUNCTION control.ledger_append(t text, a text, expect_seq bigint, rows jsonb) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    today date := (now() AT TIME ZONE 'UTC')::date;
    head_seq bigint;
    last_seq bigint;
    last_hash bytea;
    n bigint;
BEGIN
    SELECT h.seq INTO head_seq FROM control.ledger_heads h WHERE h.tenant_id = t AND h.agent = a FOR UPDATE;
    IF head_seq IS NULL OR head_seq <> expect_seq THEN
        RAISE EXCEPTION 'ledger head moved (expected %, found %)', expect_seq, head_seq;
    END IF;
    PERFORM control.ledger_ensure_partition(today);
    INSERT INTO control.ledger_entries (tenant_id, agent, seq, day, at, kind, trace_id, step_id, entry_key, step_type,
        step_name, tool_name, status, error_class, input_tokens, output_tokens, cost_usd, cost_known, security_flags,
        detail, prev_hash, hash)
    SELECT t, a, r.seq, r.day, r.at, r.kind, r.trace_id, r.step_id, r.entry_key, r.step_type, r.step_name, r.tool_name,
        r.status, r.error_class, r.input_tokens, r.output_tokens, r.cost_usd::numeric, r.cost_known, r.security_flags,
        r.detail, decode(r.prev_hash, 'hex'), decode(r.hash, 'hex')
    FROM jsonb_to_recordset(rows) AS r(seq bigint, day date, at timestamptz, kind text, trace_id text, step_id text,
        entry_key text, step_type text, step_name text, tool_name text, status text, error_class text,
        input_tokens bigint, output_tokens bigint, cost_usd text, cost_known boolean, security_flags text[],
        detail text, prev_hash text, hash text);
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n = 0 THEN
        RETURN head_seq;
    END IF;
    IF EXISTS (SELECT 1 FROM jsonb_to_recordset(rows) AS r(day date) WHERE r.day <> today) THEN
        RAISE EXCEPTION 'ledger entries must be appended on their own day';
    END IF;
    SELECT max(r.seq) INTO last_seq FROM jsonb_to_recordset(rows) AS r(seq bigint);
    IF last_seq <> head_seq + n OR (SELECT min(r.seq) FROM jsonb_to_recordset(rows) AS r(seq bigint)) <> head_seq + 1 THEN
        RAISE EXCEPTION 'ledger entries must continue the chain from %', head_seq;
    END IF;
    SELECT decode(r.hash, 'hex') INTO last_hash FROM jsonb_to_recordset(rows) AS r(seq bigint, hash text) WHERE r.seq = last_seq;
    UPDATE control.ledger_heads SET seq = last_seq, hash = last_hash WHERE tenant_id = t AND agent = a;
    RETURN last_seq;
END
$$;

-- Maintenance (alerts worker, hourly): create partitions ahead, record the
-- segment of every finished day, then drop day partitions older than the
-- retention, but only after their segments are recorded.
CREATE FUNCTION control.ledger_maintain(retention_days int, ahead_days int) RETURNS TABLE (closed int, dropped int)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    today date := (now() AT TIME ZONE 'UTC')::date;
    -- A day is finished an hour after it ends (no transaction is that long).
    finished date := ((now() - interval '1 hour') AT TIME ZONE 'UTC')::date;
    d date;
    part record;
    c int := 0;
    x int := 0;
BEGIN
    IF retention_days < 1 OR ahead_days < 0 OR ahead_days > 31 THEN
        RAISE EXCEPTION 'ledger_maintain: retention_days must be >= 1 and ahead_days between 0 and 31';
    END IF;
    FOR i IN 0..ahead_days LOOP
        PERFORM control.ledger_ensure_partition(today + i);
    END LOOP;
    FOR part IN
        SELECT to_date(substring(c2.relname FROM 16), 'YYYYMMDD') AS pday, c2.relname
        FROM pg_inherits i JOIN pg_class c2 ON c2.oid = i.inhrelid
        WHERE i.inhparent = 'control.ledger_entries'::regclass
        ORDER BY 1
    LOOP
        d := part.pday;
        IF d < finished AND NOT EXISTS (SELECT 1 FROM control.ledger_closed_days cd WHERE cd.day = d) THEN
            INSERT INTO control.ledger_segments (tenant_id, agent, day, first_seq, last_seq, entries, final_hash)
            SELECT g.tenant_id, g.agent, d, g.first_seq, g.last_seq, g.n,
                   (SELECT e.hash FROM control.ledger_entries e
                     WHERE e.tenant_id = g.tenant_id AND e.agent = g.agent AND e.day = d AND e.seq = g.last_seq)
            FROM (SELECT e.tenant_id, e.agent, min(e.seq) AS first_seq, max(e.seq) AS last_seq, count(*) AS n
                  FROM control.ledger_entries e WHERE e.day = d GROUP BY e.tenant_id, e.agent) g
            ON CONFLICT (tenant_id, agent, day) DO NOTHING;
            INSERT INTO control.ledger_closed_days (day) VALUES (d) ON CONFLICT DO NOTHING;
            c := c + 1;
        END IF;
        IF d < today - retention_days AND EXISTS (SELECT 1 FROM control.ledger_closed_days cd WHERE cd.day = d) THEN
            EXECUTE format('DROP TABLE control.%I', part.relname);
            x := x + 1;
        END IF;
    END LOOP;
    RETURN QUERY SELECT c, x;
END
$$;

-- Partitions for today and the next few days, so the first appends don't
-- have to create one.
SELECT control.ledger_ensure_partition((now() AT TIME ZONE 'UTC')::date + i) FROM generate_series(0, 3) AS i;

REVOKE ALL ON FUNCTION control.ledger_ensure_partition(date), control.ledger_lock_head(text, text),
    control.ledger_known(text, text[], text[], text[], text[]), control.ledger_append(text, text, bigint, jsonb),
    control.ledger_maintain(int, int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.ledger_lock_head(text, text), control.ledger_known(text, text[], text[], text[], text[]),
    control.ledger_append(text, text, bigint, jsonb) TO {{ingest}}, {{processor}};
GRANT EXECUTE ON FUNCTION control.ledger_maintain(int, int) TO {{alerts}};
GRANT SELECT ON control.ledger_entries, control.ledger_heads, control.ledger_segments TO {{api}};
