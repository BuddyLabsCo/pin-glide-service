CREATE TABLE IF NOT EXISTS pin_glide.identity_credentials (
    id bigserial,
    sender_id text NOT NULL,
    family_id text NOT NULL,
    scope pin_glide.identity_credential_scope NOT NULL,
    token_digest text NOT NULL,
    issued_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    CONSTRAINT pk_identity_credentials PRIMARY KEY (id),
    CONSTRAINT fk_identity_sender_registrations_id FOREIGN KEY (sender_id)
    REFERENCES pin_glide.identity_sender_registrations (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS uidx_identity_credentials_token_digest
ON pin_glide.identity_credentials (token_digest);

CREATE INDEX IF NOT EXISTS idx_identity_credentials_sender_id
ON pin_glide.identity_credentials (sender_id);

COMMENT ON TABLE pin_glide.identity_credentials
IS 'Transaction Data: credential digests, seven-day clock on expires_at.';
COMMENT ON COLUMN pin_glide.identity_credentials.id
IS 'Surrogate key of one issued credential record.';
COMMENT ON COLUMN pin_glide.identity_credentials.sender_id
IS 'Sender registration this credential belongs to.';
COMMENT ON COLUMN pin_glide.identity_credentials.family_id
IS 'Login family whose credentials are issued and revoked together.';
COMMENT ON COLUMN pin_glide.identity_credentials.scope
IS 'Authority this credential carries; valid only for that scope.';
COMMENT ON COLUMN pin_glide.identity_credentials.token_digest
IS 'SHA-256 digest of the credential; the raw token is never stored.';
COMMENT ON COLUMN pin_glide.identity_credentials.issued_at
IS 'When the credential was issued.';
COMMENT ON COLUMN pin_glide.identity_credentials.expires_at
IS 'When the credential stops being valid; the retention clock of this table.';
COMMENT ON COLUMN pin_glide.identity_credentials.revoked_at
IS 'When this credential or its family was revoked; NULL while valid.';
