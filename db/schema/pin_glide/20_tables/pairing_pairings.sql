CREATE TABLE IF NOT EXISTS pin_glide.pairing_pairings (
    id text NOT NULL,
    sender_id text NOT NULL,
    receiver_id text NOT NULL,
    alias text NOT NULL DEFAULT '',
    state pin_glide.pairing_state NOT NULL DEFAULT 'pending',
    created_at timestamptz NOT NULL DEFAULT now(),
    activated_at timestamptz,
    removed_at timestamptz,
    CONSTRAINT pk_pairing_pairings PRIMARY KEY (id),
    CONSTRAINT fk_identity_sender_registrations_id FOREIGN KEY (sender_id)
    REFERENCES pin_glide.identity_sender_registrations (id),
    CONSTRAINT fk_identity_receiver_registrations_id FOREIGN KEY (receiver_id)
    REFERENCES pin_glide.identity_receiver_registrations (id),
    CONSTRAINT chk_removed_pairings_have_removed_at CHECK (
        state <> 'removed' OR removed_at IS NOT NULL
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS uidx_pairing_pairings_sender_id_receiver_id
ON pin_glide.pairing_pairings (sender_id, receiver_id);

CREATE INDEX IF NOT EXISTS idx_pairing_pairings_receiver_id
ON pin_glide.pairing_pairings (receiver_id);

COMMENT ON TABLE pin_glide.pairing_pairings
IS 'Configuration Data: Sender-owned associations, one per Receiver.';
COMMENT ON COLUMN pin_glide.pairing_pairings.id
IS 'Pairing ID both clients share; allocated before confirmation.';
COMMENT ON COLUMN pin_glide.pairing_pairings.sender_id
IS 'Sender that owns this association and its alias.';
COMMENT ON COLUMN pin_glide.pairing_pairings.receiver_id
IS 'Receiver this association forwards to; one row per Sender.';
COMMENT ON COLUMN pin_glide.pairing_pairings.alias
IS 'The owning Sender''s own label for this association; empty when unset.';
COMMENT ON COLUMN pin_glide.pairing_pairings.state
IS 'Pending holds capacity until activation; cancellation/expiry releases it.';
COMMENT ON COLUMN pin_glide.pairing_pairings.created_at
IS 'When the pending association was reserved.';
COMMENT ON COLUMN pin_glide.pairing_pairings.activated_at
IS 'When both clients confirmed and the association became active.';
COMMENT ON COLUMN pin_glide.pairing_pairings.removed_at
IS 'When the owning Sender removed the association.';
