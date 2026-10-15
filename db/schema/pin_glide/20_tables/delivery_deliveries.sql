CREATE TABLE IF NOT EXISTS pin_glide.delivery_deliveries (
    id text NOT NULL,
    sender_id text NOT NULL,
    message_id text NOT NULL,
    receiver_id text NOT NULL,
    pairing_id text NOT NULL,
    status pin_glide.delivery_status NOT NULL DEFAULT 'processing',
    created_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    CONSTRAINT pk_delivery_deliveries PRIMARY KEY (id),
    CONSTRAINT fk_identity_sender_registrations_id FOREIGN KEY (sender_id)
    REFERENCES pin_glide.identity_sender_registrations (id),
    CONSTRAINT fk_identity_receiver_registrations_id FOREIGN KEY (receiver_id)
    REFERENCES pin_glide.identity_receiver_registrations (id),
    CONSTRAINT fk_pairing_pairings_id FOREIGN KEY (pairing_id)
    REFERENCES pin_glide.pairing_pairings (id),
    CONSTRAINT chk_completed_deliveries_have_completed_at CHECK (
        status = 'processing' OR completed_at IS NOT NULL
    )
);

-- One retained identity per Sender, Message and Receiver: the atomic claim
-- that makes a concurrent duplicate join the first Delivery instead of
-- sending a second time. The row keeps that identity until scheduled cleanup
-- deletes it, and it carries no ciphertext, key or Receiver payload.
CREATE UNIQUE INDEX IF NOT EXISTS
uidx_delivery_deliveries_sender_id_message_id_receiver_id
ON pin_glide.delivery_deliveries (sender_id, message_id, receiver_id);

-- The retention clock of this table, so scheduled cleanup can delete by age.
CREATE INDEX IF NOT EXISTS idx_delivery_deliveries_created_at
ON pin_glide.delivery_deliveries (created_at);

COMMENT ON TABLE pin_glide.delivery_deliveries
IS 'Transaction Data: admitted Deliveries, their dedup identity and outcome.';
COMMENT ON COLUMN pin_glide.delivery_deliveries.id
IS 'Opaque Delivery identity; it carries no forwarding authority.';
COMMENT ON COLUMN pin_glide.delivery_deliveries.sender_id
IS 'Sender the authenticated upload credential belonged to.';
COMMENT ON COLUMN pin_glide.delivery_deliveries.message_id
IS 'Message ID the Sender generated once for the SMS operation.';
COMMENT ON COLUMN pin_glide.delivery_deliveries.receiver_id
IS 'Receiver one entry selected; every receiver is counted separately.';
COMMENT ON COLUMN pin_glide.delivery_deliveries.pairing_id
IS 'Pairing that authorized this Delivery when it was admitted.';
COMMENT ON COLUMN pin_glide.delivery_deliveries.status
IS 'Processing until the one terminal outcome of this Delivery is stored.';
COMMENT ON COLUMN pin_glide.delivery_deliveries.created_at
IS 'When the identity was claimed; the retention clock of this table.';
COMMENT ON COLUMN pin_glide.delivery_deliveries.completed_at
IS 'When the terminal outcome was stored; NULL while processing.';
