import { escapeSQLString, fixtureSender } from './fixtures.mjs';

export function renderRevokeReceiverRegistrationSQL(receiverID) {
  if (typeof receiverID !== 'string' || receiverID === '') {
    throw new Error('Smoke Receiver revocation needs a receiver ID');
  }
  const id = escapeSQLString(receiverID);
  return `BEGIN;
SET LOCAL lock_timeout = '10s';
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION 'refusing smoke Receiver revocation outside pin_glide';
    END IF;
END
$$;
UPDATE pin_glide.identity_receiver_registrations
SET state = 'revoked',
    revoked_at = coalesce(revoked_at, now())
WHERE id = ${id};
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pin_glide.identity_receiver_registrations
        WHERE id = ${id}
          AND state = 'revoked'
    ) THEN
        RAISE EXCEPTION 'smoke Receiver registration is missing';
    END IF;
END
$$;
COMMIT;
`;
}

export function renderPurgeDeliverySQL(plan, label, messageID) {
  const sender = fixtureSender(plan, label);
  if (typeof messageID !== 'string' || messageID === '') {
    throw new Error('Smoke Delivery purge needs a message ID');
  }
  const senderID = escapeSQLString(sender.id);
  const message = escapeSQLString(messageID);
  return `BEGIN;
SET LOCAL lock_timeout = '10s';
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION 'refusing smoke Delivery purge outside pin_glide';
    END IF;
END
$$;
DELETE FROM pin_glide.delivery_deliveries
WHERE sender_id = ${senderID}
  AND message_id = ${message};
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pin_glide.delivery_deliveries
        WHERE sender_id = ${senderID}
          AND message_id = ${message}
    ) THEN
        RAISE EXCEPTION 'smoke Delivery rows were not purged';
    END IF;
END
$$;
COMMIT;
`;
}
