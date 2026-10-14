CREATE TABLE IF NOT EXISTS pin_glide.pairing_sessions (
    id text NOT NULL,
    mode pin_glide.pairing_session_mode NOT NULL,
    receiver_id text NOT NULL,
    authorized_sender_id text,
    pending_pairing_id text,
    state pin_glide.pairing_session_state NOT NULL
    DEFAULT 'awaiting_authorization',
    token_digest text NOT NULL,
    issuance_operation_id text,
    issuance_attempt smallint,
    issued_generation bigint,
    sender_confirmed_at timestamptz,
    receiver_confirmed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    closed_at timestamptz,
    CONSTRAINT pk_pairing_sessions PRIMARY KEY (id),
    CONSTRAINT fk_identity_receiver_registrations_id FOREIGN KEY (receiver_id)
    REFERENCES pin_glide.identity_receiver_registrations (id),
    CONSTRAINT fk_identity_sender_registrations_id
    FOREIGN KEY (authorized_sender_id)
    REFERENCES pin_glide.identity_sender_registrations (id),
    CONSTRAINT fk_pairing_pairings_id FOREIGN KEY (pending_pairing_id)
    REFERENCES pin_glide.pairing_pairings (id) ON DELETE SET NULL,
    CONSTRAINT chk_recover_sessions_have_no_pairing_id CHECK (
        mode <> 'recover_receiver' OR pending_pairing_id IS NULL
    ),
    CONSTRAINT chk_pairing_sessions_issuance_fields_together CHECK (
        (issuance_operation_id IS NULL) = (issuance_attempt IS NULL)
        AND (issuance_operation_id IS NULL) = (issued_generation IS NULL)
    ),
    CONSTRAINT chk_pairing_sessions_attempts_are_retransmissions CHECK (
        issuance_attempt IS NULL OR issuance_attempt IN (0, 1)
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS uidx_pairing_sessions_token_digest
ON pin_glide.pairing_sessions (token_digest);

CREATE UNIQUE INDEX IF NOT EXISTS uidx_pairing_sessions_authorized_sender_id_id
ON pin_glide.pairing_sessions (authorized_sender_id, id);

CREATE INDEX IF NOT EXISTS idx_pairing_sessions_receiver_id
ON pin_glide.pairing_sessions (receiver_id);

CREATE INDEX IF NOT EXISTS idx_pairing_sessions_expires_at
ON pin_glide.pairing_sessions (expires_at);

COMMENT ON TABLE pin_glide.pairing_sessions
IS 'Transaction Data: restricted five-minute Pairing Sessions.';
COMMENT ON COLUMN pin_glide.pairing_sessions.id
IS 'Session ID the QR references; it carries no authority by itself.';
COMMENT ON COLUMN pin_glide.pairing_sessions.mode
IS 'Fixed at creation; no later request can switch the session.';
COMMENT ON COLUMN pin_glide.pairing_sessions.receiver_id
IS 'Receiver-side target fixed at creation.';
COMMENT ON COLUMN pin_glide.pairing_sessions.authorized_sender_id
IS 'First Sender whose claim succeeded; NULL while the session is unclaimed.';
COMMENT ON COLUMN pin_glide.pairing_sessions.pending_pairing_id
IS 'Reserved Pairing ID; NULL for recovery and released sessions.';
COMMENT ON COLUMN pin_glide.pairing_sessions.state
IS 'Current state; expiry and cancellation win over stored progress.';
COMMENT ON COLUMN pin_glide.pairing_sessions.token_digest
IS 'SHA-256 digest of the restricted B credential, never the raw one.';
COMMENT ON COLUMN pin_glide.pairing_sessions.issuance_operation_id
IS 'Credential-issuance operation recorded; NULL before redemption.';
COMMENT ON COLUMN pin_glide.pairing_sessions.issuance_attempt
IS 'Highest committed attempt; 0 original, 1 retransmission.';
COMMENT ON COLUMN pin_glide.pairing_sessions.issued_generation
IS 'Receiver credential generation handed out by this session.';
COMMENT ON COLUMN pin_glide.pairing_sessions.sender_confirmed_at
IS 'When the Sender side reported key confirmation, if it has.';
COMMENT ON COLUMN pin_glide.pairing_sessions.receiver_confirmed_at
IS 'When the Receiver side reported key confirmation, if it has.';
COMMENT ON COLUMN pin_glide.pairing_sessions.created_at
IS 'When the session was created.';
COMMENT ON COLUMN pin_glide.pairing_sessions.expires_at
IS 'Absolute end of the session and its B credential.';
COMMENT ON COLUMN pin_glide.pairing_sessions.closed_at
IS 'When the session was canceled or superseded; NULL while live.';
