CREATE TABLE IF NOT EXISTS pin_glide.identity_receiver_registrations (
    id text NOT NULL,
    account_id text,
    state pin_glide.identity_receiver_registration_state NOT NULL
    DEFAULT 'pending',
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    CONSTRAINT pk_identity_receiver_registrations PRIMARY KEY (id),
    CONSTRAINT fk_identity_accounts_id FOREIGN KEY (account_id)
    REFERENCES pin_glide.identity_accounts (id),
    CONSTRAINT chk_receiver_registrations_are_owned CHECK (
        state = 'pending' OR account_id IS NOT NULL
    ),
    CONSTRAINT chk_revoked_receiver_registrations_have_revoked_at CHECK (
        state <> 'revoked' OR revoked_at IS NOT NULL
    )
);

CREATE INDEX IF NOT EXISTS idx_identity_receiver_registrations_account_id
ON pin_glide.identity_receiver_registrations (account_id);

COMMENT ON TABLE pin_glide.identity_receiver_registrations
IS 'Configuration Data: Receiver registrations and revocation.';
COMMENT ON COLUMN pin_glide.identity_receiver_registrations.id
IS 'Receiver deviceId; a reinstall creates a new registration.';
COMMENT ON COLUMN pin_glide.identity_receiver_registrations.account_id
IS 'Owning Account once a Sender claim fixed it; NULL before that.';
COMMENT ON COLUMN pin_glide.identity_receiver_registrations.state
IS 'Pending until a Sender claim fixes ownership, then active.';
COMMENT ON COLUMN pin_glide.identity_receiver_registrations.created_at
IS 'When the Receiver context was allocated.';
COMMENT ON COLUMN pin_glide.identity_receiver_registrations.revoked_at
IS 'When Receiver revocation permanently closed the registration.';
