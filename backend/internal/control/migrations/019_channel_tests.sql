-- Test deliveries ("Send test" in the console) run in the alerts worker,
-- the only service allowed to decrypt channel targets and signing secrets.
-- The API records a request here; the worker claims it, delivers a sample
-- alert and stores the outcome as a message safe to show the user.
CREATE TABLE control.alert_channel_tests (
    tenant_id    text        NOT NULL REFERENCES control.tenants (id),
    id           text        NOT NULL CHECK (id ~ '^ct_[0-9a-f]{20}$'),
    channel_id   text        NOT NULL REFERENCES control.alert_channels (id) ON DELETE CASCADE,
    requested_by text,
    requested_at timestamptz NOT NULL DEFAULT now(),
    status       text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'ok', 'failed')),
    claimed_at   timestamptz,
    error        text        CHECK (error IS NULL OR length(error) <= 300),
    finished_at  timestamptz,
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX alert_channel_tests_open ON control.alert_channel_tests (requested_at) WHERE status IN ('pending', 'running');
ALTER TABLE control.alert_channel_tests ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON control.alert_channel_tests
    USING (tenant_id = current_setting('bindle.tenant', true)) WITH CHECK (tenant_id = current_setting('bindle.tenant', true));
GRANT SELECT, INSERT ON control.alert_channel_tests TO {{api}};

-- The worker claims up to max open tests across tenants: pending ones, and
-- running ones whose worker stopped (claimed more than 2 minutes ago). Tests
-- older than a day are removed here too.
CREATE FUNCTION control.claim_channel_tests(max int)
RETURNS TABLE (tenant_id text, id text, channel_id text)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog AS $$
#variable_conflict use_column
BEGIN
    DELETE FROM control.alert_channel_tests t WHERE t.requested_at < now() - interval '1 day';
    RETURN QUERY
    WITH due AS (
        SELECT t.tenant_id, t.id FROM control.alert_channel_tests t
        WHERE t.status = 'pending' OR (t.status = 'running' AND t.claimed_at < now() - interval '2 minutes')
        ORDER BY t.requested_at
        LIMIT GREATEST(LEAST(max, 100), 0)
        FOR UPDATE SKIP LOCKED
    )
    UPDATE control.alert_channel_tests t SET status = 'running', claimed_at = now()
    FROM due WHERE t.tenant_id = due.tenant_id AND t.id = due.id
    RETURNING t.tenant_id, t.id, t.channel_id;
END
$$;
REVOKE ALL ON FUNCTION control.claim_channel_tests(int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.claim_channel_tests(int) TO {{alerts}};

-- Records a claimed test's outcome. err is the user-facing message.
CREATE FUNCTION control.finish_channel_test(t text, test_id text, ok boolean, err text)
RETURNS void
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog AS $$
    UPDATE control.alert_channel_tests SET
        status = CASE WHEN ok THEN 'ok' ELSE 'failed' END,
        error = CASE WHEN ok THEN NULL ELSE left(COALESCE(err, 'the test failed'), 300) END,
        finished_at = now()
    WHERE tenant_id = t AND id = test_id AND status = 'running'
$$;
REVOKE ALL ON FUNCTION control.finish_channel_test(text, text, boolean, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.finish_channel_test(text, text, boolean, text) TO {{alerts}};
