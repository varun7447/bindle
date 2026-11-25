-- Secrets are stored encrypted (internal/secretbox: AWS KMS, or AES-GCM with
-- a local key in the docker stack), bound to their tenant, purpose and row.
--
-- alert_channels.target is a secret too: Slack incoming-webhook URLs and many
-- webhook URLs carry a token. display keeps the masked form the console shows,
-- so listing channels needs no decryption.
--
-- The plaintext columns stay, nullable, for rows written before this
-- migration: readers use the ciphertext when present and fall back to them.
-- `bindle-admin secrets encrypt` encrypts those rows and clears the plaintext.
ALTER TABLE control.alert_channels
    ADD COLUMN target_enc bytea,
    ADD COLUMN secret_enc bytea,
    ADD COLUMN display text NOT NULL DEFAULT '',
    ALTER COLUMN target DROP NOT NULL,
    ALTER COLUMN secret DROP NOT NULL,
    ALTER COLUMN secret DROP DEFAULT,
    ADD CONSTRAINT alert_channels_target_present CHECK (target IS NOT NULL OR target_enc IS NOT NULL);

ALTER TABLE control.outcome_endpoints
    ADD COLUMN secret_enc bytea,
    ALTER COLUMN secret DROP NOT NULL,
    ADD CONSTRAINT outcome_endpoints_secret_present CHECK (secret IS NOT NULL OR secret_enc IS NOT NULL);

-- Ingest still looks an endpoint up by ID alone; it now gets the ciphertext
-- (or, for an old row, the plaintext) and decrypts it itself.
DROP FUNCTION control.outcome_endpoint_secret(text);
CREATE FUNCTION control.outcome_endpoint_secret(endpoint text)
RETURNS TABLE (tenant_id text, secret text, secret_enc bytea)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT e.tenant_id, COALESCE(e.secret, ''), e.secret_enc FROM control.outcome_endpoints e
    WHERE e.id = endpoint AND e.revoked_at IS NULL
$$;
REVOKE ALL ON FUNCTION control.outcome_endpoint_secret(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control.outcome_endpoint_secret(text) TO {{ingest}};
