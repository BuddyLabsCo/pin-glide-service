import { escapeSQLString, renderExpirePairingSessionSQL } from './fixtures.mjs';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { WebSocketClosedError } from './websocket.mjs';
import { runScenario } from './session_credential_support.mjs';
import { associations } from './session_pairing_helpers.mjs';

const openAPI = 'generated/protocol/openapi.yaml';
const asyncAPI = 'generated/protocol/asyncapi.yaml';
const openapiBytes = readFileSync(new URL('../../../generated/protocol/openapi.yaml', import.meta.url));
const asyncapiBytes = readFileSync(new URL('../../../generated/protocol/asyncapi.yaml', import.meta.url));
const openapiDigest = createHash('sha256').update(openapiBytes).digest('hex');
const asyncapiDigest = createHash('sha256').update(asyncapiBytes).digest('hex');
const openapiText = openapiBytes.toString('utf8');
const asyncapiText = asyncapiBytes.toString('utf8');
const wssNames = {
  pairingState: 'pairing_state',
  pairingRelayIn: 'pairing_relay',
  pairingRelayOut: 'pairing_relay',
  credentialReady: 'credential_ready',
  pairingRemoved: 'pairing_removed',
  delivery: 'delivery',
  receipt: 'receipt',
  error: 'error',
};

// The published close meanings are the authority for what a connection must
// send, so scenarios assert against them rather than a second copy.
export const closeMeaning = Object.fromEntries(
  [...asyncapiText.matchAll(/- code: (\d+)\s*\n\s+description: [^\n]*\n\s+reason: ?"?([^"\n]*)"?/g)]
    .map(match => [Number(match[1]), match[2]]),
);

function operationIdentity(name) {
  if (!openapiText.includes(`operationId: ${name}`)) {
    throw new Error(`Pairing scenario names an unpublished HTTP operation ${name}`);
  }
  return { name, source: openAPI };
}

function messageIdentity(name) {
  const wireName = wssNames[name];
  if (!wireName || !asyncapiText.includes(`name: ${wireName}`)) {
    throw new Error(`Pairing scenario names an unpublished WSS message ${name}`);
  }
  return { name: wireName, source: asyncAPI };
}

export async function runProtocolScenario(session, name, exercised, callback) {
  return runScenario(session, name, async () => ({
    contract: {
      openapi: { source: openAPI, digest: openapiDigest },
      asyncapi: { source: asyncAPI, digest: asyncapiDigest },
      http: exercised.http.map(operationIdentity),
      wss: exercised.wss.map(messageIdentity),
      ...(session.target.sourceCommit ? { sourceCommit: session.target.sourceCommit } : {}),
    },
    ...(await callback()),
  }));
}

export async function runPairingScenario(session, name, exercised, callback) {
  return runProtocolScenario(session, name, exercised, callback);
}

export function sessionPath(pairing) {
  return `/v1/pairing-sessions/${encodeURIComponent(pairing.sessionID)}/events`;
}

export function opaqueRelayPayload(kind, seed) {
  if (kind === 'hpke_init') {
    return Buffer.from(JSON.stringify({
      kind,
      cryptoVersion: 1,
      enc: Buffer.alloc(65, seed).toString('base64'),
      ciphertext: Buffer.alloc(48, seed + 1).toString('base64'),
    })).toString('base64');
  }
  if (kind === 'key_confirmation') {
    return Buffer.from(JSON.stringify({
      kind,
      cryptoVersion: 1,
      nonce: Buffer.alloc(12, seed).toString('base64'),
      ciphertext: Buffer.alloc(32, seed + 1).toString('base64'),
    })).toString('base64');
  }
  throw new Error(`Unknown Pairing relay fixture ${kind}`);
}

export async function expectPairingState(socket, pairing) {
  const message = await socket.next();
  const frame = message.json;
  if (frame?.type !== 'pairing_state' || frame.session?.sessionId !== pairing.sessionID) {
    throw new Error('Pairing channel did not open with its own pairing_state');
  }
  return frame;
}

export async function expectRelay(socket, pairing, side, payload) {
  const message = await socket.next();
  const frame = message.json;
  if (
    frame?.type !== 'pairing_relay'
    || frame.sessionId !== pairing.sessionID
    || frame.side !== side
    || frame.payload !== payload
  ) {
    throw new Error(`Pairing relay did not preserve the ${side} participant's opaque bytes`);
  }
  return frame;
}

export async function expectPairingStates(socket, pairing, states) {
  const received = [];
  for (let index = 0; index < states.length;) {
    const frame = (await socket.next()).json;
    if (frame?.type !== 'pairing_state' || frame.session?.sessionId !== pairing.sessionID) {
      throw new Error('Pairing channel did not publish its own pairing_state');
    }
    if (frame.session.state === states[index]) {
      received.push(frame.session);
      index++;
      continue;
    }
    if (index > 0 && frame.session.state === states[index - 1]) {
      continue;
    }
    throw new Error(`Pairing channel published ${frame.session.state}, expected ${states[index]}`);
  }
  return received;
}

export async function expectCredentialReady(socket, pairing) {
  const [senderState, receiverState] = await expectPairingStates(
    socket,
    pairing,
    ['awaiting_confirmation', 'credential_ready'],
  );
  const frame = await socket.next().then(message => message.json);
  if (
    senderState?.state !== 'awaiting_confirmation'
    || !senderState.senderConfirmed
    || receiverState?.state !== 'credential_ready'
    || !receiverState.senderConfirmed
    || !receiverState.receiverConfirmed
    || frame?.type !== 'credential_ready'
    || frame.sessionId !== pairing.sessionID
    || frame.receiverId !== pairing.receiverID
    || frame.expiresAt !== pairing.expiresAt
  ) {
    throw new Error('Pairing notifications did not report activation and credential readiness');
  }
  return frame;
}

export async function completePairingReports(
  session,
  label,
  pairing,
  receiverState = 'completed',
  reportedSender,
) {
  const sender = reportedSender || await session.completePairingSide(label, pairing);
  if (sender.state !== 'awaiting_confirmation' || !sender.senderConfirmed || sender.receiverConfirmed) {
    throw new Error('One authenticated report activated the Pairing');
  }
  const receiver = await session.completePairingSide(label, pairing, pairing.token);
  if (
    receiver.state !== receiverState
    || !receiver.senderConfirmed
    || !receiver.receiverConfirmed
  ) {
    throw new Error(`Both authenticated reports did not reach ${receiverState}`);
  }
  return { sender, receiver };
}

export async function expectNoFrame(socket, description, timeoutMs = 300) {
  try {
    const message = await socket.next(timeoutMs);
    throw new Error(`${description}: ${message.text}`);
  } catch (error) {
    if (error instanceof WebSocketClosedError) throw error;
    if (!String(error.message).includes('timed out waiting')) throw error;
  }
}

export async function expectSocketClose(socket, code) {
  try {
    await socket.next(5_000);
  } catch (error) {
    if (error instanceof WebSocketClosedError && error.code === code) return error;
    throw error;
  }
  throw new Error(`Pairing channel stayed open instead of closing with ${code}`);
}

export async function drainPairingUntilClosed(socket) {
  const states = [];
  try {
    for (;;) {
      const message = await socket.next(5_000);
      if (message.json?.type === 'pairing_state') states.push(message.json.session);
    }
  } catch (error) {
    if (!(error instanceof WebSocketClosedError)) throw error;
  }
  return states;
}

export function openDelivery(session, receiverToken) {
  return session.openSocket({ path: '/v1/receiver/events', token: receiverToken });
}

export async function associationRow(session, pairingID) {
  const rows = await session.remote.sqlJSON(`
SELECT coalesce((
  SELECT json_build_object(
    'id', id,
    'senderId', sender_id,
    'receiverId', receiver_id,
    'alias', alias,
    'state', state,
    'replacesPairingId', replaces_pairing_id
  )
  FROM pin_glide.pairing_pairings
  WHERE id = ${escapeSQLString(pairingID)}
), 'null'::json);
`);
  return rows;
}

export async function receiverCredentialFacts(session, receiverID) {
  return session.remote.sqlJSON(`
SELECT json_build_object(
  'current', (
    SELECT count(*)
    FROM pin_glide.identity_receiver_credentials
    WHERE receiver_id = ${escapeSQLString(receiverID)}
      AND revoked_at IS NULL
  ),
  'generation', (
    SELECT coalesce(max(generation), 0)
    FROM pin_glide.identity_receiver_credentials
    WHERE receiver_id = ${escapeSQLString(receiverID)}
      AND revoked_at IS NULL
  )
);
`);
}

export async function expirePairingSession(session, sessionID) {
  await session.remote.sql(renderExpirePairingSessionSQL(sessionID));
}

export async function waitForPresence(session, label, pairingID, wanted, {
  attempts = 30,
  intervalMs = 100,
} = {}) {
  let last;
  for (let attempt = 0; attempt < attempts; attempt++) {
    const listed = await associations(session, label);
    last = listed.find(item => item.pairingId === pairingID);
    if (last?.presence === wanted) return last;
    await new Promise(resolve => setTimeout(resolve, intervalMs));
  }
  throw new Error(`Pairing ${pairingID} never reported presence ${wanted}`);
}

export function assertNoSecret(value, secret, description) {
  if (secret && JSON.stringify(value).includes(secret)) {
    throw new Error(`${description} exposed a credential`);
  }
}
