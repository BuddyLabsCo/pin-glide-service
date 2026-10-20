import { createHash, randomBytes } from 'node:crypto';

const scopes = ['user_session', 'uploader', 'refresh'];

function value(prefix) {
  return `${prefix}_${randomBytes(32).toString('hex')}`;
}

function digest(token) {
  return createHash('sha256').update(token).digest('hex');
}

export function escapeSQLString(value) {
  return `'${String(value).replaceAll("'", "''")}'`;
}

function sqlArray(values) {
  if (values.length === 0) return 'ARRAY[]::text[]';
  return `ARRAY[${values.map(escapeSQLString).join(', ')}]::text[]`;
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
  ];
  const senders = senderSpecs.map(({ label, accountID }) => {
    const id = `smoke-${runId}-sender-${label}`;
    const familyID = `smoke-${runId}-family-${label}`;
    return {
      id,
      accountID,
      familyID,
      credentials: scopes.map(scope => {
        const token = value(`smoke-${runId}-${label}-${scope}`);
        return {
          scope,
          token,
          digest: digest(token),
          expiresAt: new Date(now.getTime() + (scope === 'refresh' ? 7 * 24 : 3) * 60 * 60 * 1000),
        };
      }),
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
WHERE account_id = ANY(${sqlArray(accountIDs)})
   OR id = ANY(${sqlArray(receiverIDs)})
ON CONFLICT DO NOTHING;

DELETE FROM pin_glide.delivery_deliveries
WHERE sender_id = ANY(${sqlArray(senderIDs)})
   OR receiver_id IN (SELECT id FROM smoke_owned_receivers);

DELETE FROM pin_glide.pairing_sessions
WHERE authorized_sender_id = ANY(${sqlArray(senderIDs)})
   OR receiver_id IN (SELECT id FROM smoke_owned_receivers);

DELETE FROM pin_glide.pairing_pairings
WHERE sender_id = ANY(${sqlArray(senderIDs)})
   OR receiver_id IN (SELECT id FROM smoke_owned_receivers);

DELETE FROM pin_glide.identity_receiver_credentials
WHERE receiver_id IN (SELECT id FROM smoke_owned_receivers);

DELETE FROM pin_glide.identity_receiver_registrations
WHERE id IN (SELECT id FROM smoke_owned_receivers);

DELETE FROM pin_glide.identity_refresh_operations
WHERE sender_id = ANY(${sqlArray(senderIDs)});

DELETE FROM pin_glide.identity_credentials
WHERE sender_id = ANY(${sqlArray(senderIDs)});

DELETE FROM pin_glide.identity_sender_registrations
WHERE id = ANY(${sqlArray(senderIDs)});

DELETE FROM pin_glide.identity_accounts
WHERE id = ANY(${sqlArray(accountIDs)});

DELETE FROM pin_glide.service_diagnostics
WHERE request_id = ANY(${sqlArray(requestIds)})
   OR subject = ANY(${sqlArray(subjects)});
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
    now(), now() + interval '${credential.scope === 'refresh' ? '7 days' : '3 hours'}'
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
        WHERE sender_id = ANY(${sqlArray(plan.senders.map(sender => sender.id))})
          AND token_digest ~ '^[0-9a-f]{64}$'
    ) <> ${plan.senders.length * scopes.length} THEN
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
