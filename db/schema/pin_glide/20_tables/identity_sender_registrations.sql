CREATE TABLE IF NOT EXISTS pin_glide.identity_sender_registrations (
    id text NOT NULL,
    account_id text NOT NULL,
    state pin_glide.identity_sender_registration_state NOT NULL
    DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    CONSTRAINT pk_identity_sender_registrations PRIMARY KEY (id),
    CONSTRAINT fk_identity_accounts_id FOREIGN KEY (account_id)
    REFERENCES pin_glide.identity_accounts (id),
    CONSTRAINT chk_revoked_sender_registrations_have_revoked_at CHECK (
        state = 'active' OR revoked_at IS NOT NULL
    )
);

CREATE INDEX IF NOT EXISTS idx_identity_sender_registrations_account_id
ON pin_glide.identity_sender_registrations (account_id);

COMMENT ON TABLE pin_glide.identity_sender_registrations
IS 'Configuration Data: Sender registrations and their security revocation.';
COMMENT ON COLUMN pin_glide.identity_sender_registrations.id
IS 'Sender deviceId assigned by the service; reinstallation creates a new one.';
COMMENT ON COLUMN pin_glide.identity_sender_registrations.account_id
IS 'Owning Account; a Sender belongs to exactly one Account.';
COMMENT ON COLUMN pin_glide.identity_sender_registrations.state
IS 'Active registrations accept login restoration; revoked ones never do.';
COMMENT ON COLUMN pin_glide.identity_sender_registrations.created_at
IS 'When the installation registration was first created.';
COMMENT ON COLUMN pin_glide.identity_sender_registrations.revoked_at
IS 'When owner security revocation permanently closed the registration.';
