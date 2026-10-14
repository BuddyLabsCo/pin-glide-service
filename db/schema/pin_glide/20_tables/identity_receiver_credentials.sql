CREATE TABLE IF NOT EXISTS pin_glide.identity_receiver_credentials (
    id bigserial,
    receiver_id text NOT NULL,
    generation bigint NOT NULL,
    token_digest text NOT NULL,
    issued_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    CONSTRAINT pk_identity_receiver_credentials PRIMARY KEY (id),
    CONSTRAINT fk_identity_receiver_registrations_id FOREIGN KEY (receiver_id)
    REFERENCES pin_glide.identity_receiver_registrations (id),
    CONSTRAINT chk_receiver_credentials_count_generations_from_one CHECK (
        generation > 0
    )
);

CREATE UNIQUE INDEX
IF NOT EXISTS uidx_identity_receiver_credentials_token_digest
ON pin_glide.identity_receiver_credentials (token_digest);

CREATE UNIQUE INDEX
IF NOT EXISTS uidx_identity_receiver_credentials_receiver_id_generation
ON pin_glide.identity_receiver_credentials (receiver_id, generation);

COMMENT ON TABLE pin_glide.identity_receiver_credentials
IS 'Configuration Data: long-lived Receiver credential digests.';
COMMENT ON COLUMN pin_glide.identity_receiver_credentials.id
IS 'Surrogate key of one issued Receiver credential record.';
COMMENT ON COLUMN pin_glide.identity_receiver_credentials.receiver_id
IS 'Receiver registration this credential belongs to.';
COMMENT ON COLUMN pin_glide.identity_receiver_credentials.generation
IS 'Issuance generation; only the newest one currently authorizes.';
COMMENT ON COLUMN pin_glide.identity_receiver_credentials.token_digest
IS 'SHA-256 digest of the credential; the raw token is never stored.';
COMMENT ON COLUMN pin_glide.identity_receiver_credentials.issued_at
IS 'When this generation of the Receiver credential was issued.';
COMMENT ON COLUMN pin_glide.identity_receiver_credentials.revoked_at
IS 'Superseded by a replacement or revocation; NULL while current.';
