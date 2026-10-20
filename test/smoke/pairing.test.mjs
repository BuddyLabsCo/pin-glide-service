import assert from 'node:assert/strict';
import test from 'node:test';

import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';

import { createFixturePlan, renderExpirePairingSessionSQL } from './lib/fixtures.mjs';
import { SmokeSession } from './lib/session.mjs';
import { opaqueRelayPayload, runPairingScenario } from './lib/session_pairing_support.mjs';
import { TARGET } from './lib/target.mjs';

function sessionWith(http, plan = createFixturePlan({ runId: 'pairing-test' })) {
  return new SmokeSession({
    target: TARGET,
    remote: {},
    http,
    evidence: { addSecret() {} },
    plan,
  });
}

test('new_receiver creation fixes the mode and target and returns B only to the Receiver', async () => {
  const requests = [];
  const session = sessionWith({
    async call(request) {
      requests.push(request);
      if (request.path === '/v1/pairing-sessions') {
        return {
          status: 201,
          body: {
            session: {
              sessionId: 'session-1',
              mode: 'new_receiver',
              receiverId: 'receiver-1',
              state: 'awaiting_authorization',
              expiresAt: '2026-10-21T00:05:00Z',
            },
            pairingToken: 'pairing-secret',
            pairingTokenExpiresAt: '2026-10-21T00:05:00Z',
          },
        };
      }
      if (request.path === '/v1/pairing-sessions/session-1/authorize') {
        return {
          status: 200,
          body: {
            sessionId: 'session-1',
            mode: 'new_receiver',
            receiverId: 'receiver-1',
            pairingId: 'pairing-1',
            state: 'awaiting_confirmation',
            senderId: session.sender('pair1').id,
          },
        };
      }
      throw new Error(`unexpected request ${request.path}`);
    },
  });

  const created = await session.createPairingSession({ mode: 'new_receiver' });
  assert.deepEqual(requests[0].body, { mode: 'new_receiver' });
  assert.equal(created.receiverID, 'receiver-1');
  assert.equal(created.token, 'pairing-secret');
  assert.equal(created.mode, 'new_receiver');
  assert.equal(created.expiresAt, '2026-10-21T00:05:00Z');
  assert.equal(created.tokenExpiresAt, created.expiresAt);
  assert.equal(session.secrets.has('pairing-secret'), true);

  const claimed = await session.claimPairing('pair1', created, { alias: 'Phone' });
  assert.deepEqual(requests[1].body, { alias: 'Phone' });
  assert.equal(requests[1].token, session.token('pair1', 'user_session'));
  assert.equal(claimed.pairingID, 'pairing-1');
  assert.equal(claimed.receiverID, created.receiverID);
  assert.equal(claimed.mode, created.mode);
});

test('issuance retransmission accepts a dropped original response without exposing R to the Sender', async () => {
  const requests = [];
  const session = sessionWith({
    async call(request) {
      requests.push({ dropped: false, ...request });
      return {
        status: 200,
        requestId: 'request-issued',
        cacheControl: 'no-store',
        body: { receiverId: 'receiver-1', receiverToken: 'receiver-secret' },
      };
    },
    async callDropped(request) {
      requests.push({ dropped: true, ...request });
      return { status: 200, requestId: 'request-dropped' };
    },
  });
  const pairing = { sessionID: 'session-1', token: 'pairing-secret' };

  const dropped = await session.redeemReceiver(pairing, {
    operationID: 'operation-1',
    attempt: 0,
    dropped: true,
  });
  assert.equal(dropped.requestId, 'request-dropped');
  assert.equal(session.secrets.has('receiver-secret'), false);

  const issued = await session.redeemReceiver(pairing, {
    operationID: 'operation-1',
    attempt: 1,
  });
  assert.equal(issued.receiverToken, 'receiver-secret');
  assert.equal(session.secrets.has('receiver-secret'), true);
  assert.deepEqual(requests.map(request => request.body), [
    { operationId: 'operation-1', attempt: 0 },
    { operationId: 'operation-1', attempt: 1 },
  ]);
  assert.deepEqual(requests.map(request => request.dropped), [true, false]);
});

test('a Pairing Session cannot change its fixed Receiver target during creation', async () => {
  const session = sessionWith({
    async call() {
      return {
        status: 201,
        body: {
          session: {
            sessionId: 'session-fixed',
            mode: 'add_sender',
            receiverId: 'receiver-other',
            state: 'awaiting_authorization',
            expiresAt: '2026-10-21T00:05:00Z',
          },
          pairingToken: 'pairing-secret',
          pairingTokenExpiresAt: '2026-10-21T00:05:00Z',
        },
      };
    },
  });
  await assert.rejects(
    () => session.createPairingSession({
      mode: 'add_sender',
      receiverID: 'receiver-requested',
      token: 'receiver-secret',
    }),
    /fixed Receiver target/,
  );
});

test('opaque relay fixtures are padded Base64 JSON with the adopted field sizes', () => {
  const init = JSON.parse(Buffer.from(opaqueRelayPayload('hpke_init', 7), 'base64').toString('utf8'));
  assert.deepEqual(Object.keys(init), ['kind', 'cryptoVersion', 'enc', 'ciphertext']);
  assert.equal(init.kind, 'hpke_init');
  assert.equal(init.cryptoVersion, 1);
  assert.equal(Buffer.from(init.enc, 'base64').length, 65);

  const confirmation = JSON.parse(
    Buffer.from(opaqueRelayPayload('key_confirmation', 9), 'base64').toString('utf8'),
  );
  assert.equal(confirmation.kind, 'key_confirmation');
  assert.equal(confirmation.cryptoVersion, 1);
  assert.equal(Buffer.from(confirmation.nonce, 'base64').length, 12);
});

test('Pairing Session expiry SQL is guarded and escapes its run-owned identifier', () => {
  assert.throws(() => renderExpirePairingSessionSQL(''), /session ID/);
  const sql = renderExpirePairingSessionSQL("session-'run");
  assert.match(sql, /current_database\(\) = 'pin_glide'/);
  assert.match(sql, /WHERE id = 'session-''run'/);
});

test('Pairing evidence records the contract operations, messages and source commit', async () => {
  const recorded = [];
  const session = {
    requestIds: ['request-1'],
    target: { ...TARGET, sourceCommit: 'source-commit' },
    evidence: { record(entry) { recorded.push(entry); } },
  };
  await runPairingScenario(
    session,
    'contract identity',
    { http: ['PostPairingSessions'], wss: ['pairingState'] },
    async () => ({ exercised: true }),
  );
  const contract = {
    openapi: {
      source: 'generated/protocol/openapi.yaml',
      digest: createHash('sha256')
        .update(readFileSync('generated/protocol/openapi.yaml'))
        .digest('hex'),
    },
    asyncapi: {
      source: 'generated/protocol/asyncapi.yaml',
      digest: createHash('sha256')
        .update(readFileSync('generated/protocol/asyncapi.yaml'))
        .digest('hex'),
    },
    http: [{ name: 'PostPairingSessions', source: 'generated/protocol/openapi.yaml' }],
    wss: [{ name: 'pairing_state', source: 'generated/protocol/asyncapi.yaml' }],
    sourceCommit: 'source-commit',
  };
  assert.deepEqual(recorded, [{
    scenario: 'contract identity',
    status: 'passed',
    details: {
      contract,
      exercised: true,
      requestIds: [],
    },
  }]);
});
