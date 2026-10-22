import { createHash } from 'node:crypto';

import { escapeSQLString, sqlTextArray } from '../support/sql.mjs';
import { credentialLifetimeInterval } from './fixtures.mjs';
import { SmokeBlocked, SmokeProblem } from './http.mjs';

export function snapshot(sender) {
  return Object.fromEntries(sender.credentials.map(item => [item.scope, { ...item }]));
}
export function settle(promise) {
  return promise.then(value => ({ value }), error => ({ error }));
}

export async function runScenario(session, name, callback, { evidenceType = 'smoke' } = {}) {
  const start = session.requestIds.length;
  try {
    const details = await callback();
    session.evidence.record({
      scenario: name,
      status: 'passed',
      evidenceType,
      details: {
        ...details,
        requestIds: session.requestIds.slice(start),
      },
    });
    return { name, status: 'passed' };
  } catch (error) {
    const status = error instanceof SmokeBlocked ? 'blocked' : 'failed';
    session.evidence.record({
      scenario: name,
      status,
      evidenceType,
      code: error instanceof SmokeProblem ? error.code : '',
      requestId: error instanceof SmokeProblem ? error.requestId : '',
      details: {
        reason: error.message,
        requestIds: session.requestIds.slice(start),
      },
    });
    throw error;
  }
}

export async function durableFacts(session, label, operationIDs = []) {
  const sender = session.sender(label);
  const id = escapeSQLString(sender.id);
  return session.remote.sqlJSON(`
SELECT json_build_object(
  'senderId', ${id},
  'registrationState', (
    SELECT state
    FROM pin_glide.identity_sender_registrations
    WHERE id = ${id}
  ),
  'activeCredentials', (
    SELECT count(*)
    FROM pin_glide.identity_credentials
    WHERE sender_id = ${id}
      AND revoked_at IS NULL
      AND expires_at > now()
  ),
  'revokedCredentials', (
    SELECT count(*)
    FROM pin_glide.identity_credentials
    WHERE sender_id = ${id}
      AND revoked_at IS NOT NULL
  ),
  'credentials', (
    SELECT coalesce(json_agg(json_build_object(
      'scope', scope,
      'tokenDigest', token_digest,
      'issuedAt', issued_at,
      'expiresAt', expires_at,
      'revokedAt', revoked_at
    ) ORDER BY id), '[]'::json)
    FROM pin_glide.identity_credentials
    WHERE sender_id = ${id}
  ),
  'badLifetimes', (
    SELECT count(*)
    FROM pin_glide.identity_credentials
    WHERE sender_id = ${id}
      AND (
        (
          scope IN ('user_session', 'uploader')
          AND expires_at - issued_at <> interval '${credentialLifetimeInterval('user_session')}'
        )
        OR (
          scope = 'refresh'
          AND expires_at - issued_at <> interval '${credentialLifetimeInterval('refresh')}'
        )
      )
  ),
  'pairings', (
    SELECT coalesce(json_agg(json_build_object(
      'id', id,
      'receiverId', receiver_id,
      'state', state
    ) ORDER BY id), '[]'::json)
    FROM pin_glide.pairing_pairings
    WHERE sender_id = ${id}
  ),
  'operations', (
    SELECT coalesce(json_agg(json_build_object(
      'operationId', operation_id,
      'attempt', attempt,
      'parentDigest', parent_digest,
      'committedAt', committed_at,
      'expiresAt', expires_at
    ) ORDER BY operation_id), '[]'::json)
    FROM pin_glide.identity_refresh_operations
    WHERE sender_id = ${id}
      AND operation_id = ANY(${sqlTextArray(operationIDs)})
  )
);
`);
}

export async function verifyCheckpoints(session) {
  const expected = session.plan.senders
    .filter(sender => sender.latestRefresh)
    .map(sender => ({
      senderId: sender.id,
      operationId: sender.latestRefresh.operationID,
      attempt: sender.latestRefresh.attempt,
    }));
  if (expected.length === 0) throw new Error('No refresh checkpoint was retained');
  const senders = session.plan.senders.map(sender => sender.id);
  const actual = await session.remote.sqlJSON(`
SELECT coalesce(json_agg(json_build_object(
  'senderId', sender_id,
  'operationId', operation_id,
  'attempt', attempt,
  'latest', is_latest
)), '[]'::json)
FROM (
  SELECT sender_id, operation_id, attempt,
    row_number() OVER (
      PARTITION BY sender_id
      ORDER BY committed_at DESC, operation_id DESC
    ) = 1 AS is_latest
  FROM pin_glide.identity_refresh_operations
  WHERE sender_id = ANY(${sqlTextArray(senders)})
) AS sender_operations;
`);
  for (const checkpoint of expected) {
    const row = actual.find(item => (
      item.senderId === checkpoint.senderId
      && item.operationId === checkpoint.operationId
      && item.attempt === checkpoint.attempt
    ));
    if (!row) throw new Error(`Runner refresh checkpoint was not persisted for ${checkpoint.senderId}`);
    if (!row.latest) throw new Error(`Runner refresh checkpoint is stale for ${checkpoint.senderId}`);
  }
  const expectedCredentials = expected.flatMap(checkpoint => {
    const sender = session.plan.senders.find(item => item.id === checkpoint.senderId);
    return sender.credentials.map(item => ({
      senderId: checkpoint.senderId,
      tokenDigest: createHash('sha256').update(item.token).digest('hex'),
    }));
  });
  const digests = expectedCredentials.map(item => item.tokenDigest);
  const persisted = await session.remote.sqlJSON(`
SELECT coalesce(json_agg(json_build_object(
  'senderId', sender_id,
  'tokenDigest', token_digest
)), '[]'::json)
FROM pin_glide.identity_credentials
WHERE sender_id = ANY(${sqlTextArray(senders)})
  AND token_digest = ANY(${sqlTextArray(digests)});
`);
  for (const expectedCredential of expectedCredentials) {
    if (!persisted.some(item => (
      item.senderId === expectedCredential.senderId
      && item.tokenDigest === expectedCredential.tokenDigest
    ))) {
      throw new Error('Runner credential bundle does not match the persisted credential rows');
    }
  }
  if (session.plan.senders.some(sender => sender.pendingRefresh)) {
    throw new Error('An uncertain refresh attempt remained pending after recovery');
  }
  return { checkpoints: expected, credentialBundles: expected.length };
}
