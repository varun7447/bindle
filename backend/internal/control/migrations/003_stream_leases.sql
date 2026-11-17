-- Kinesis shard leases and checkpoints for bindle-processor instances.
CREATE TABLE control.stream_leases (
    stream         text        NOT NULL,
    shard_id       text        NOT NULL,
    owner          text        NOT NULL,
    lease_expires  timestamptz NOT NULL,
    checkpoint     text        NOT NULL DEFAULT '',
    finished       boolean     NOT NULL DEFAULT false,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (stream, shard_id)
);

-- The processor role touches only this table.
GRANT USAGE ON SCHEMA control TO {{processor}};
GRANT SELECT, INSERT, UPDATE ON control.stream_leases TO {{processor}};
