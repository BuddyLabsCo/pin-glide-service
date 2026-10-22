import {
  credentialLifetimeInterval,
  escapeSQLString,
  fixtureSender,
} from '../harness/fixtures.mjs';
import { transaction } from '../harness/sql-transaction.mjs';

export function renderExpireAccessSQL(plan, label) {
  const sender = fixtureSender(plan, label);
  return transaction({ senderIDs: [sender.id] }, `
UPDATE pin_glide.identity_credentials
SET expires_at = now() - interval '1 second'
WHERE sender_id = ${escapeSQLString(sender.id)}
  AND scope IN ('user_session', 'uploader')
  AND revoked_at IS NULL
  AND expires_at > now();
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pin_glide.identity_credentials
        WHERE sender_id = ${escapeSQLString(sender.id)}
          AND scope IN ('user_session', 'uploader')
          AND expires_at <= now()
    ) THEN
        RAISE EXCEPTION 'smoke fixture access credentials are missing';
    END IF;
END
$$;
`);
}

export function renderExpireRefreshSQL(plan, label) {
  const sender = fixtureSender(plan, label);
  return transaction({ senderIDs: [sender.id] }, `
UPDATE pin_glide.identity_credentials
SET expires_at = now() - interval '1 second'
WHERE sender_id = ${escapeSQLString(sender.id)}
  AND scope = 'refresh'
  AND revoked_at IS NULL
  AND expires_at > now();
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pin_glide.identity_credentials
        WHERE sender_id = ${escapeSQLString(sender.id)}
          AND scope = 'refresh'
          AND expires_at <= now()
    ) THEN
        RAISE EXCEPTION 'smoke fixture refresh credential is missing';
    END IF;
END
$$;
`);
}

export function renderAgeRefreshOperationSQL(plan, label, operationID, seconds = 31) {
  const sender = fixtureSender(plan, label);
  if (!Number.isInteger(seconds) || seconds <= 0) throw new Error('Refresh operation age must be a positive integer');
  return transaction({ senderIDs: [sender.id] }, `
UPDATE pin_glide.identity_refresh_operations
SET committed_at = now() - interval '${seconds} seconds'
WHERE sender_id = ${escapeSQLString(sender.id)}
  AND operation_id = ${escapeSQLString(operationID)};
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pin_glide.identity_refresh_operations
        WHERE sender_id = ${escapeSQLString(sender.id)}
          AND operation_id = ${escapeSQLString(operationID)}
    ) THEN
        RAISE EXCEPTION 'smoke fixture refresh operation is missing';
    END IF;
END
$$;
`);
}

export function renderExpireRefreshParentSQL(plan, label, operationID) {
  const sender = fixtureSender(plan, label);
  return transaction({ senderIDs: [sender.id] }, `
UPDATE pin_glide.identity_credentials AS credential
SET expires_at = now() - interval '1 second'
WHERE credential.sender_id = ${escapeSQLString(sender.id)}
  AND EXISTS (
      SELECT 1
      FROM pin_glide.identity_refresh_operations AS operation
      WHERE operation.sender_id = credential.sender_id
        AND operation.operation_id = ${escapeSQLString(operationID)}
        AND operation.parent_digest = credential.token_digest
  );
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pin_glide.identity_credentials AS credential
        JOIN pin_glide.identity_refresh_operations AS operation
          ON operation.sender_id = credential.sender_id
         AND operation.parent_digest = credential.token_digest
        WHERE operation.sender_id = ${escapeSQLString(sender.id)}
          AND operation.operation_id = ${escapeSQLString(operationID)}
          AND credential.expires_at <= now()
    ) THEN
        RAISE EXCEPTION 'smoke fixture refresh predecessor is missing';
    END IF;
END
$$;
`);
}

export function renderRevokeSenderRegistrationSQL(plan, label) {
  const sender = fixtureSender(plan, label);
  return transaction({ senderIDs: [sender.id] }, `
UPDATE pin_glide.identity_credentials
SET revoked_at = coalesce(revoked_at, now())
WHERE sender_id = ${escapeSQLString(sender.id)};

UPDATE pin_glide.identity_sender_registrations
SET state = 'revoked',
    revoked_at = coalesce(revoked_at, now())
WHERE id = ${escapeSQLString(sender.id)};
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pin_glide.identity_sender_registrations
        WHERE id = ${escapeSQLString(sender.id)}
          AND state = 'revoked'
    ) THEN
        RAISE EXCEPTION 'smoke fixture Sender registration was not revoked';
    END IF;
END
$$;
`);
}

export function renderSupersedeFamilySQL(plan, label) {
  const sender = fixtureSender(plan, label);
  if (!sender.replacementFamilyID || !sender.replacementCredentials) {
    throw new Error(`Smoke Sender ${label} has no replacement credential family`);
  }
  const rows = sender.replacementCredentials.map(credential => `(
    ${escapeSQLString(sender.id)}, ${escapeSQLString(sender.replacementFamilyID)},
    ${escapeSQLString(credential.scope)}, ${escapeSQLString(credential.digest)},
    now(), now() + interval '${credentialLifetimeInterval(credential.scope)}'
  )`).join(',\n');
  return transaction({ senderIDs: [sender.id] }, `
UPDATE pin_glide.identity_credentials
SET revoked_at = coalesce(revoked_at, now())
WHERE sender_id = ${escapeSQLString(sender.id)};

INSERT INTO pin_glide.identity_credentials
    (sender_id, family_id, scope, token_digest, issued_at, expires_at)
VALUES
${rows};
DO $$
BEGIN
    IF (
        SELECT count(*)
        FROM pin_glide.identity_credentials
        WHERE sender_id = ${escapeSQLString(sender.id)}
          AND family_id = ${escapeSQLString(sender.replacementFamilyID)}
          AND revoked_at IS NULL
    ) <> ${sender.replacementCredentials.length} THEN
        RAISE EXCEPTION 'smoke fixture replacement family is incomplete';
    END IF;
END
$$;
`);
}
