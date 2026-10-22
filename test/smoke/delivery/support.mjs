import { randomBytes } from 'node:crypto';

import { escapeSQLString, sqlTextArray } from '../support/sql.mjs';
import { fixtureSender } from '../harness/fixtures.mjs';
import { runScenario } from '../harness/scenario.mjs';
import { contractEvidence } from '../harness/scenario-contract.mjs';
import { expectNoMessage, expectSocketClose } from '../harness/socket-assertions.mjs';

const wssNames = new Map([
  ['delivery', 'delivery'],
  ['receipt', 'receipt'],
  ['error', 'error'],
]);

export { expectNoMessage, expectSocketClose };

export async function runDeliveryScenario(session, name, exercised, callback) {
  return runScenario(session, name, async () => ({
    contract: contractEvidence(session, exercised, Object.fromEntries(wssNames)),
    ...(await callback()),
  }));
}

export function messageID(session, label) {
  return `smoke-${session.plan.runId}-message-${label}-${randomBytes(6).toString('hex')}`;
}

export function deliveryItem(pairing, overrides = {}) {
  return {
    receiverId: overrides.receiverId ?? pairing.receiverID,
    pairingId: pairing.pairingID,
    cryptoVersion: overrides.cryptoVersion ?? 1,
    nonce: overrides.nonce ?? randomBytes(12).toString('base64'),
    ciphertext: overrides.ciphertext ?? randomBytes(48).toString('base64'),
  };
}

export function uploadBody(message, items) {
  return { messageId: message, items };
}

export async function upload(session, label, message, items) {
  return session.request({
    method: 'POST',
    path: '/v1/messages',
    token: session.token(label, 'uploader'),
    body: uploadBody(message, items),
  });
}

export function expectResult(response, index, receiverID, status, code = '') {
  const results = response.body?.results;
  if (!Array.isArray(results) || results.length <= index) {
    throw new Error(`Upload response has no result ${index}: ${JSON.stringify(response.body)}`);
  }
  const result = results[index];
  // The contract omits receiverId when the entry carried none, so an
  // unreadable Receiver reads as the same empty value the expectation names.
  if (result.index !== index || (result.receiverId || '') !== receiverID || result.status !== status) {
    throw new Error(
      `Result ${index} = ${JSON.stringify(result)}, want ${receiverID} ${status}`,
    );
  }
  const actualCode = result.error?.code || '';
  if (actualCode !== code) throw new Error(`Result ${index} code = ${actualCode}, want ${code}`);
  return result;
}

export async function nextDelivery(socket, association, expected) {
  const message = await socket.next();
  const frame = message.json;
  if (
    frame?.type !== 'delivery'
    || frame.senderId !== association.senderID
    || frame.receiverId !== association.receiverID
    || frame.pairingId !== association.pairingID
  ) {
    const observed = frame
      ? `${frame.type}/${frame.senderId}/${frame.receiverId}/${frame.pairingId}`
      : 'unreadable';
    throw new Error(`Delivery frame does not match ${association.pairingID}: ${observed}`);
  }
  if (expected) {
    const candidates = Array.isArray(expected) ? expected : [expected];
    const exact = candidates.some(({ messageId, item }) => (
      frame.messageId === messageId
      && frame.cryptoVersion === item.cryptoVersion
      && frame.nonce === item.nonce
      && frame.ciphertext === item.ciphertext
    ));
    if (!exact) {
      throw new Error(`Delivery frame did not carry a submitted payload for ${association.pairingID}`);
    }
  }
  return frame;
}

export function sendReceipt(socket, frame) {
  socket.sendJSON({ version: 1, type: 'receipt', senderId: frame.senderId, messageId: frame.messageId });
}

export async function metadata(session, label, message) {
  return session.request({
    path: `/v1/messages/${encodeURIComponent(message)}`,
    token: session.token(label, 'uploader'),
  });
}

export function expectMetadata(response, deliveries) {
  if (response.status !== 200 || !Array.isArray(response.body?.deliveries)) {
    throw new Error(`Message metadata = ${response.status}: ${JSON.stringify(response.body)}`);
  }
  const actual = new Map(response.body.deliveries.map(item => [item.receiverId, item]));
  if (actual.size !== deliveries.length) {
    throw new Error(`Message metadata reports ${actual.size} Deliveries, want ${deliveries.length}`);
  }
  for (const wanted of deliveries) {
    const item = actual.get(wanted.receiverID);
    if (!item || item.status !== wanted.status || (item.error?.code || '') !== (wanted.code || '')) {
      throw new Error(
        `Delivery metadata for ${wanted.receiverID} = ${JSON.stringify(item)}, want ${wanted.status}`,
      );
    }
    if (wanted.createdAt && Date.parse(item.createdAt) !== Date.parse(wanted.createdAt)) {
      throw new Error(`Delivery ${wanted.receiverID} admission time changed`);
    }
    if (wanted.completedAt && Date.parse(item.completedAt) !== Date.parse(wanted.completedAt)) {
      throw new Error(`Delivery ${wanted.receiverID} terminal time changed`);
    }
  }
  return Object.fromEntries(actual);
}

export async function waitForMetadata(session, label, message, wanted, timeoutMs = 5_000) {
  const deadline = Date.now() + timeoutMs;
  let last;
  while (Date.now() < deadline) {
    try {
      const result = await metadata(session, label, message);
      last = result;
      const views = result.body?.deliveries || [];
      if (wanted.every(item => views.some(view => (
        view.receiverId === item.receiverID && view.status === item.status
      )))) {
        return result;
      }
    } catch (error) {
      if (error?.code !== 'resource_not_found') throw error;
    }
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  throw new Error(`Delivery metadata did not reach ${JSON.stringify(wanted)}: ${JSON.stringify(last?.body)}`);
}

export async function openReceiver(session, token) {
  let last;
  for (let attempt = 0; attempt < 20; attempt++) {
    try {
      return await session.openSocket({ path: '/v1/receiver/events', token });
    } catch (error) {
      last = error;
      if (error?.status !== 409 || error?.code !== 'receiver_already_connected') throw error;
      await new Promise(resolve => setTimeout(resolve, 100));
    }
  }
  throw last;
}

export function deliveryTarget(session, label, pairing) {
  return { ...pairing, senderID: session.sender(label).id };
}

export function liveTarget(session, context) {
  return {
    ...deliveryTarget(session, 'pair2', context.currentPairing),
    receiverToken: context.enrollment.receiverToken,
  };
}

export function deliveryRowQuery(session, label, message) {
  return `
SELECT json_build_object(
  'count', count(*),
  'createdAt', min(created_at),
  'completedAt', min(completed_at),
  'status', min(status::text)
)
FROM pin_glide.delivery_deliveries
WHERE sender_id = ${escapeSQLString(session.sender(label).id)}
  AND message_id = ${escapeSQLString(message)};
`;
}

export function deliveryDurationQuery(session, label, messages, status) {
  return `
SELECT json_build_object(
  'maximumMs', coalesce(extract(epoch FROM max(completed_at - created_at)) * 1000, 0)::double precision,
  'rows', count(*)
)
FROM pin_glide.delivery_deliveries
WHERE sender_id = ${escapeSQLString(fixtureSender(session.plan, label).id)}
  AND message_id = ANY(${sqlTextArray(messages)})
  AND status = ${escapeSQLString(status)};
`;
}

export async function waitForAdmitted(session, label, messages, expected = messages.length, timeoutMs = 5_000) {
  const query = `
SELECT json_build_object(
  'count', count(*)::int
) AS admitted
FROM pin_glide.delivery_deliveries
WHERE sender_id = ${escapeSQLString(fixtureSender(session.plan, label).id)}
  AND message_id = ANY(${sqlTextArray(messages)});
`;
  const deadline = Date.now() + timeoutMs;
  let last;
  while (Date.now() < deadline) {
    last = await session.remote.sqlJSON(query);
    if (Number(last.count) >= expected) return;
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  throw new Error(`Delivery admission reached ${last?.count || 0} of ${expected} entries`);
}
