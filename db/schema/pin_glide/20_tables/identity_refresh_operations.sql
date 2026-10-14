CREATE TABLE IF NOT EXISTS pin_glide.identity_refresh_operations (
    sender_id text NOT NULL,
    operation_id text NOT NULL,
    attempt smallint NOT NULL,
    parent_digest text NOT NULL,
    expires_at timestamptz NOT NULL,
    committed_at timestamptz NOT NULL,
    CONSTRAINT pk_identity_refresh_operations PRIMARY KEY (
        sender_id,
        operation_id
    ),
    CONSTRAINT fk_identity_sender_registrations_id FOREIGN KEY (sender_id)
    REFERENCES pin_glide.identity_sender_registrations (id),
    CONSTRAINT chk_identity_refresh_operations_attempt CHECK (attempt IN (0, 1))
);

CREATE INDEX IF NOT EXISTS idx_identity_refresh_operations_parent_digest
ON pin_glide.identity_refresh_operations (parent_digest);

COMMENT ON TABLE pin_glide.identity_refresh_operations
IS 'Transaction Data: refresh attempt ordering, clock on expires_at.';
COMMENT ON COLUMN pin_glide.identity_refresh_operations.sender_id
IS 'Sender registration the recorded refresh operation belongs to.';
COMMENT ON COLUMN pin_glide.identity_refresh_operations.operation_id
IS 'Client-generated operation identifier of one Sender renewal.';
COMMENT ON COLUMN pin_glide.identity_refresh_operations.attempt
IS 'Highest accepted attempt of this operation; 0 original, 1 retransmission.';
COMMENT ON COLUMN pin_glide.identity_refresh_operations.parent_digest
IS 'Digest of the refresh token this operation consumed.';
COMMENT ON COLUMN pin_glide.identity_refresh_operations.expires_at
IS 'Expiry of the consumed token; the ordering guard ends with it.';
COMMENT ON COLUMN pin_glide.identity_refresh_operations.committed_at
IS 'When the highest accepted attempt of this operation committed.';
