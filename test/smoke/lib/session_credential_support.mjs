import { createHash } from 'node:crypto';

import { escapeSQLString, sqlTextArray } from './fixtures.mjs';
import { SmokeProblem } from './http.mjs';

export function snapshot(sender) {
  return Object.fromEntries(sender.credentials.map(item => [item.scope, { ...item }]));
}
export function settle(promise) {
  return promise.then(value => ({ value }), error => ({ error }));
}

export async function runScenario(session, name, callback) {
  const start = session.requestIds.length;
  try {
    const details = await callback();
    session.evidence.record({
      scenario: name,
      status: 'passed',
      details: {
        ...details,
        requestIds: session.requestIds.slice(start),
      },
    });
    return { name, status: 'passed' };
  } catch (error) {
    session.evidence.record({
      scenario: name,
      status: 'failed',
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
        (scope IN ('user_session', 'uploader') AND expires_at - issued_at <> interval '3 hours')
        OR (scope = 'refresh' AND expires_at - issued_at <> interval '7 days')
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

export async function receiverState(session, receiverID) {
  const rows = await session.remote.sqlJSON(`
SELECT json_build_object(
  'state', state
)
FROM pin_glide.identity_receiver_registrations
WHERE id = ${escapeSQLString(receiverID)};
`);
  return rows.state;
}

export async function associations(session, label) {
  const listed = await session.verifyManagement(session.token(label, 'user_session'));
  return listed.body.items || [];
}

export async function pairReceiver(session, label) {
  const pairing = await session.createPairing(label);
  if (!pairing.pairingID || !pairing.receiverID) throw new Error('Pairing activation returned no association');
  await session.completePairing(label, pairing);
  const issued = await session.redeemReceiver(pairing);
  if (!issued.receiverToken || issued.receiverId !== pairing.receiverID) {
    throw new Error('Receiver credential does not belong to the paired Receiver');
  }
  const listed = await associations(session, label);
  if (!listed.some(item => item.pairingId === pairing.pairingID)) {
    throw new Error('Activated Pairing is absent from the Sender association list');
  }
  return { ...pairing, receiverToken: issued.receiverToken };
}

export async function addSenderToReceiver(session, label, pairing) {
  const added = await session.createPairing(label, {
    mode: 'add_sender',
    receiverID: pairing.receiverID,
    token: pairing.receiverToken,
  });
  if (!added.pairingID || added.receiverID !== pairing.receiverID) {
    throw new Error('add_sender did not target the paired Receiver');
  }
  await session.completePairing(label, added);
  const listed = await associations(session, label);
  if (!listed.some(item => item.pairingId === added.pairingID)) {
    throw new Error('add_sender Pairing is absent from the Sender association list');
  }
  return added;
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
  'attempt', attempt
)), '[]'::json)
FROM pin_glide.identity_refresh_operations
WHERE sender_id = ANY(${sqlTextArray(senders)});
`);
  for (const checkpoint of expected) {
    const row = actual.find(item => (
      item.senderId === checkpoint.senderId
      && item.operationId === checkpoint.operationId
      && item.attempt === checkpoint.attempt
    ));
    if (!row) throw new Error(`Runner refresh checkpoint was not persisted for ${checkpoint.senderId}`);
  }
  const digests = expected.flatMap(checkpoint => {
    const sender = session.plan.senders.find(item => item.id === checkpoint.senderId);
    return sender.credentials.map(item => createHash('sha256').update(item.token).digest('hex'));
  });
  const persisted = await session.remote.sqlJSON(`
SELECT coalesce(json_agg(json_build_object(
  'senderId', sender_id,
  'tokenDigest', token_digest
)), '[]'::json)
FROM pin_glide.identity_credentials
WHERE sender_id = ANY(${sqlTextArray(senders)})
  AND token_digest = ANY(${sqlTextArray(digests)});
`);
  if (persisted.length !== digests.length) {
    throw new Error('Runner credential bundle does not match the persisted credential rows');
  }
  if (session.plan.senders.some(sender => sender.pendingRefresh)) {
    throw new Error('An uncertain refresh attempt remained pending after recovery');
  }
  return { checkpoints: expected, credentialBundles: expected.length };
}

export async function verifyDiagnosticRedaction(session) {
  let records = [];
  for (let attempt = 0; attempt < 40; attempt++) {
    records = await session.remote.sqlJSON(`
SELECT coalesce(json_agg(json_build_object(
  'requestId', request_id,
  'subject', subject,
  'method', method
)), '[]'::json)
FROM pin_glide.service_diagnostics
WHERE request_id = ANY(${sqlTextArray(session.requestIds)})
   OR subject = ANY(${sqlTextArray([...session.subjects])});
`);
    if (records.length > 0) break;
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  if (records.length === 0) throw new Error('No run-owned service diagnostics were available to inspect');
  const serialized = JSON.stringify(records);
  for (const secret of session.secretValues()) {
    if (serialized.includes(secret)) throw new Error('A raw credential appeared in service diagnostics');
  }
  return { records: records.length, scannedRequestIds: session.requestIds.length };
}
