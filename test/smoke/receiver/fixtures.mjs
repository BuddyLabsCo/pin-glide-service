import { escapeSQLString, sqlTextArray } from '../support/sql.mjs';
import { transaction } from '../harness/fixtures.mjs';

export function renderInvalidateReceiverCredentialSQL(plan, receiverID) {
  if (typeof receiverID !== 'string' || receiverID === '') {
    throw new Error('Smoke Receiver credential invalidation needs a Receiver ID');
  }
  const id = escapeSQLString(receiverID);
  const accountIDs = plan.accounts.map(account => account.id);
  return transaction({ senderIDs: plan.senders.map(sender => sender.id) }, `
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pin_glide.identity_receiver_registrations
        WHERE id = ${id}
          AND account_id = ANY(${sqlTextArray(accountIDs)})
    ) THEN
        RAISE EXCEPTION 'smoke fixture Receiver is missing or not run-owned';
    END IF;
END
$$;

UPDATE pin_glide.identity_receiver_credentials
SET revoked_at = now()
WHERE receiver_id = ${id}
  AND revoked_at IS NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pin_glide.identity_receiver_credentials
        WHERE receiver_id = ${id}
          AND revoked_at IS NOT NULL
    ) OR EXISTS (
        SELECT 1
        FROM pin_glide.identity_receiver_credentials
        WHERE receiver_id = ${id}
          AND revoked_at IS NULL
    ) THEN
        RAISE EXCEPTION 'smoke fixture Receiver credentials were not invalidated';
    END IF;
END
$$;
`);
}
