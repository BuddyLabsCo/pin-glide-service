import { createHash, randomBytes } from 'node:crypto';

export const credentialLifetimeHours = Object.freeze({
  user_session: 3,
  uploader: 3,
  refresh: 7 * 24,
});

export const credentialScopes = Object.keys(credentialLifetimeHours);

export function credentialLifetimeInterval(scope) {
  const hours = credentialLifetimeHours[scope];
  if (hours === undefined) throw new Error(`Unknown smoke credential scope ${scope}`);
  return `${hours} hours`;
}

function value(prefix) {
  return `${prefix}_${randomBytes(32).toString('hex')}`;
}

function digest(token) {
  return createHash('sha256').update(token).digest('hex');
}

export function escapeSQLString(value) {
  return `'${String(value).replaceAll("'", "''")}'`;
}

export function sqlTextArray(values) {
  if (values.length === 0) return 'ARRAY[]::text[]';
  return `ARRAY[${values.map(escapeSQLString).join(', ')}]::text[]`;
}

export function retentionMarker(plan) {
  return `smoke-${plan.runId}-retention`;
}

export function fixtureSender(plan, label) {
  const sender = plan.senders.find(item => item.label === label);
  if (!sender) throw new Error(`Unknown smoke Sender ${label}`);
  return sender;
}

export function createFixturePlan({ runId = `smoke-${Date.now()}-${randomBytes(6).toString('hex')}`, now = new Date() } = {}) {
  if (!/^[a-z0-9][a-z0-9-]{0,63}$/.test(runId)) {
    throw new Error('Smoke run ID must be a short lowercase identifier');
  }
  const accounts = ['a', 'b'].map(suffix => ({
    id: `smoke-${runId}-account-${suffix}`,
    provider: 'qq',
    appID: 'smoke-fixture',
    openID: `smoke-fixture-${runId}-${suffix}`,
  }));
  const senderSpecs = [
    { label: 'a1', accountID: accounts[0].id },
    { label: 'a2', accountID: accounts[0].id },
    { label: 'foreign', accountID: accounts[1].id },
    { label: 'scope', accountID: accounts[0].id },
    { label: 'switch', accountID: accounts[0].id },
    { label: 'order', accountID: accounts[0].id },
    { label: 'recover', accountID: accounts[0].id },
    { label: 'window', accountID: accounts[0].id },
    { label: 'aging', accountID: accounts[0].id },
    { label: 'race', accountID: accounts[0].id },
    { label: 'permanent', accountID: accounts[0].id },
    { label: 'superseded', accountID: accounts[0].id },
    { label: 'pair1', accountID: accounts[0].id },
    { label: 'pair2', accountID: accounts[0].id },
    { label: 'receiver-main', accountID: accounts[0].id },
    { label: 'receiver-shared', accountID: accounts[0].id },
    { label: 'receiver-other', accountID: accounts[1].id },
    { label: 'model', accountID: accounts[0].id },
    { label: 'model-expired', accountID: accounts[0].id },
    { label: 'model-revoked', accountID: accounts[0].id },
  ];
  const senders = senderSpecs.map(({ label, accountID }) => {
    const id = `smoke-${runId}-sender-${label}`;
    const familyID = `smoke-${runId}-family-${label}`;
    const credentials = credentialScopes.map(scope => {
      const token = value(`smoke-${runId}-${label}-${scope}`);
      return {
        scope,
        token,
        digest: digest(token),
        expiresAt: new Date(
          now.getTime() + credentialLifetimeHours[scope] * 60 * 60 * 1000,
        ),
      };
    });
    return {
      label,
      id,
      accountID,
      familyID,
      credentials,
      replacementFamilyID: label === 'superseded' ? `${familyID}-replacement` : undefined,
      replacementCredentials: label === 'superseded' ? credentialScopes.map(scope => {
        const token = value(`smoke-${runId}-${label}-replacement-${scope}`);
        return {
          scope,
          token,
          digest: digest(token),
          expiresAt: new Date(
            now.getTime() + credentialLifetimeHours[scope] * 60 * 60 * 1000,
          ),
        };
      }) : undefined,
    };
  });
  return { runId, now, accounts, senders };
}

function cleanupBody(plan, receiverIDs = [], requestIds = [], subjects = []) {
  const accountIDs = plan.accounts.map(account => account.id);
  const senderIDs = plan.senders.map(sender => sender.id);
  return `
CREATE TEMP TABLE smoke_owned_receivers (id text PRIMARY KEY) ON COMMIT DROP;
INSERT INTO smoke_owned_receivers (id)
SELECT id
FROM pin_glide.identity_receiver_registrations
    WHERE account_id = ANY(${sqlTextArray(accountIDs)})
   OR id = ANY(${sqlTextArray(receiverIDs)})
ON CONFLICT DO NOTHING;

DELETE FROM pin_glide.delivery_deliveries
    WHERE sender_id = ANY(${sqlTextArray(senderIDs)})
   OR receiver_id IN (SELECT id FROM smoke_owned_receivers);

DELETE FROM pin_glide.pairing_sessions
WHERE authorized_sender_id = ANY(${sqlTextArray(senderIDs)})
   OR receiver_id IN (SELECT id FROM smoke_owned_receivers);

DELETE FROM pin_glide.pairing_pairings
WHERE sender_id = ANY(${sqlTextArray(senderIDs)})
   OR receiver_id IN (SELECT id FROM smoke_owned_receivers);

DELETE FROM pin_glide.identity_receiver_credentials
WHERE receiver_id IN (SELECT id FROM smoke_owned_receivers);

DELETE FROM pin_glide.identity_receiver_registrations
WHERE id IN (SELECT id FROM smoke_owned_receivers);

DELETE FROM pin_glide.identity_refresh_operations
WHERE sender_id = ANY(${sqlTextArray(senderIDs)});

DELETE FROM pin_glide.identity_credentials
WHERE sender_id = ANY(${sqlTextArray(senderIDs)});

DELETE FROM pin_glide.identity_sender_registrations
WHERE id = ANY(${sqlTextArray(senderIDs)});

DELETE FROM pin_glide.identity_accounts
WHERE id = ANY(${sqlTextArray(accountIDs)});

DELETE FROM pin_glide.service_diagnostics
WHERE request_id = ANY(${sqlTextArray(requestIds)})
   OR subject = ANY(${sqlTextArray(subjects)});
`;
}

export function transaction(message, body) {
  return `BEGIN;
SET LOCAL lock_timeout = '10s';
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION 'refusing smoke fixture outside pin_glide';
    END IF;
END
$$;
${body.trim()}
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pin_glide.identity_sender_registrations
        WHERE id = ANY(${sqlTextArray(message.senderIDs)})
    ) THEN
        RAISE EXCEPTION 'smoke fixture Sender is missing';
    END IF;
END
$$;
COMMIT;
`;
}

export function renderBootstrapSQL(plan) {
  const accountRows = plan.accounts.map(account => `(
    ${escapeSQLString(account.id)}, ${escapeSQLString(account.provider)},
    ${escapeSQLString(account.appID)}, ${escapeSQLString(account.openID)}
  )`).join(',\n');
  const senderRows = plan.senders.map(sender => `(
    ${escapeSQLString(sender.id)}, ${escapeSQLString(sender.accountID)}, 'active'
  )`).join(',\n');
  const credentialRows = plan.senders.flatMap(sender => sender.credentials.map(credential => `(
    ${escapeSQLString(sender.id)}, ${escapeSQLString(sender.familyID)}, ${escapeSQLString(credential.scope)},
    ${escapeSQLString(credential.digest)},
    now(), now() + interval '${credentialLifetimeInterval(credential.scope)}'
  )`)).join(',\n');

  return `BEGIN;
SET LOCAL lock_timeout = '10s';
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION 'refusing smoke fixture outside pin_glide';
    END IF;
END
$$;
${cleanupBody(plan)}

INSERT INTO pin_glide.identity_accounts (id, provider, app_id, open_id)
VALUES
${accountRows};

INSERT INTO pin_glide.identity_sender_registrations (id, account_id, state)
VALUES
${senderRows};

INSERT INTO pin_glide.identity_credentials
(sender_id, family_id, scope, token_digest, issued_at, expires_at)
VALUES
${credentialRows};

DO $$
BEGIN
    IF (
        SELECT count(*)
        FROM pin_glide.identity_credentials
        WHERE sender_id = ANY(${sqlTextArray(plan.senders.map(sender => sender.id))})
          AND token_digest ~ '^[0-9a-f]{64}$'
    ) <> ${plan.senders.length * credentialScopes.length} THEN
        RAISE EXCEPTION 'smoke fixture credential bootstrap is incomplete';
    END IF;
END
$$;
COMMIT;
`;
}

export function renderCleanupSQL(plan, receiverIDs = [], requestIds = [], subjects = []) {
  return `BEGIN;
SET LOCAL lock_timeout = '10s';
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION 'refusing smoke cleanup outside pin_glide';
    END IF;
END
$$;
${cleanupBody(plan, receiverIDs, requestIds, subjects)}
COMMIT;
`;
}

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

export function renderExpirePairingSessionSQL(sessionID) {
  if (typeof sessionID !== 'string' || sessionID === '') {
    throw new Error('Smoke Pairing Session expiry needs a session ID');
  }
  const id = escapeSQLString(sessionID);
  return `BEGIN;
SET LOCAL lock_timeout = '10s';
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION 'refusing smoke Pairing Session expiry outside pin_glide';
    END IF;
END
$$;
UPDATE pin_glide.pairing_sessions
SET expires_at = now() - interval '1 second'
WHERE id = ${id};
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pin_glide.pairing_sessions
        WHERE id = ${id}
          AND expires_at <= now()
    ) THEN
        RAISE EXCEPTION 'smoke Pairing Session is missing';
    END IF;
END
$$;
COMMIT;
`;
}
