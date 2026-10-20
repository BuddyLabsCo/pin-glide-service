import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import test from 'node:test';

import { createFixturePlan } from './lib/fixtures.mjs';
import { SmokeBlocked, SmokeProblem } from './lib/http.mjs';
import { SmokeSession } from './lib/session.mjs';
import { runScenario, verifyCheckpoints } from './lib/session_credential_support.mjs';
import { verifyDiagnosticRedaction } from './lib/session_diagnostics.mjs';
import { TARGET } from './lib/target.mjs';
import { integration } from './runner.mjs';

test('fixture authority proves scope and Account/Sender ownership through refresh', async () => {
  const plan = createFixturePlan({ runId: 'refresh-authority-run' });
  plan.senders = [plan.senders[0]];
  const sender = plan.senders[0];
  const initialManagement = sender.credentials.find(item => item.scope === 'user_session').token;
  const initialUploader = sender.credentials.find(item => item.scope === 'uploader').token;
  const initialRefresh = sender.credentials.find(item => item.scope === 'refresh').token;
  const rotated = {
    userSession: { token: 'rotated-management', expiresAt: '2026-10-20T03:00:00Z' },
    uploader: { token: 'rotated-uploader', expiresAt: '2026-10-20T03:00:00Z' },
    refresh: { token: 'rotated-refresh', expiresAt: '2026-10-27T00:00:00Z' },
  };
  const authorizations = [];
  const http = {
    async call(request) {
      if (request.path === '/v1/auth/refresh') {
        assert.equal(request.body.refreshToken, initialRefresh);
        return {
          status: 200,
          requestId: 'request-refresh',
          cacheControl: 'no-store',
          body: {
            accountId: sender.accountID,
            senderId: sender.id,
            operationId: request.body.operationId,
            attempt: request.body.attempt,
            ...rotated,
          },
        };
      }
      authorizations.push(request.token);
      if (request.token === initialManagement || request.token === rotated.userSession.token) {
        return { status: 200, requestId: 'request-management', body: { receivers: [] } };
      }
      if (request.token === initialUploader || request.token === rotated.uploader.token) {
        throw new SmokeProblem(404, { code: 'resource_not_found' }, 'request-upload');
      }
      throw new SmokeProblem(401, { code: 'credential_invalid' }, 'request-invalid');
    },
  };
  const evidence = { addSecret() {} };
  const session = new SmokeSession({ target: TARGET, remote: {}, http, evidence, plan });
  session.verifyRows = async () => {};

  await session.verifyAuthority();
  assert.deepEqual(authorizations, [
    initialManagement,
    initialUploader,
    initialRefresh,
    rotated.userSession.token,
    rotated.uploader.token,
  ]);
  assert.equal(sender.credentials.find(item => item.scope === 'refresh').token, rotated.refresh.token);
});

test('a refresh response updates the runner checkpoint with its accepted attempt and bundle', async () => {
  const plan = createFixturePlan({ runId: 'refresh-checkpoint-run' });
  const sender = plan.senders.find(item => item.id.endsWith('-scope'));
  const issuedAt = new Date('2026-10-20T04:00:00Z').toISOString();
  const refreshExpiresAt = new Date('2026-10-27T04:00:00Z').toISOString();
  const http = {
    async call(request) {
      assert.equal(request.body.operationId, 'operation-1');
      assert.equal(request.body.attempt, 1);
      return {
        status: 200,
        requestId: 'request-refresh',
        cacheControl: 'no-store',
        body: {
          accountId: sender.accountID,
          senderId: sender.id,
          operationId: 'operation-1',
          attempt: 1,
          userSession: { token: 'new-management', expiresAt: issuedAt },
          uploader: { token: 'new-uploader', expiresAt: issuedAt },
          refresh: { token: 'new-refresh', expiresAt: refreshExpiresAt },
        },
      };
    },
  };
  const session = new SmokeSession({
    target: TARGET,
    remote: {},
    http,
    evidence: { addSecret() {} },
    plan,
  });

  await session.refresh('scope', { operationID: 'operation-1', attempt: 1 });
  assert.deepEqual(sender.latestRefresh, { operationID: 'operation-1', attempt: 1 });
  assert.equal(sender.credentials.find(item => item.scope === 'user_session').token, 'new-management');
  assert.equal(sender.credentials.find(item => item.scope === 'uploader').token, 'new-uploader');
  assert.equal(sender.credentials.find(item => item.scope === 'refresh').token, 'new-refresh');
  assert.equal(
    sender.credentials.find(item => item.scope === 'refresh').digest,
    createHash('sha256').update('new-refresh').digest('hex'),
  );
});

test('an abandoned refresh response records an uncertain attempt without replacing the bundle', async () => {
  const plan = createFixturePlan({ runId: 'refresh-dropped-run' });
  const sender = plan.senders.find(item => item.id.endsWith('-recover'));
  const original = sender.credentials.map(item => ({ ...item }));
  const http = {
    async callDropped(request) {
      assert.equal(request.body.operationId, 'operation-dropped');
      assert.equal(request.body.attempt, 0);
      return { status: 200, requestId: 'request-dropped' };
    },
  };
  const session = new SmokeSession({
    target: TARGET,
    remote: {},
    http,
    evidence: { addSecret() {} },
    plan,
  });

  const result = await session.refresh('recover', {
    operationID: 'operation-dropped',
    attempt: 0,
    dropped: true,
  });
  assert.equal(result.requestId, 'request-dropped');
  assert.deepEqual(sender.pendingRefresh, { operationID: 'operation-dropped', attempt: 0 });
  assert.equal(sender.latestRefresh, undefined);
  assert.deepEqual(sender.credentials, original);
});

test('a refresh bundle must contain independent M/U/F tokens', () => {
  const plan = createFixturePlan({ runId: 'refresh-independence-run' });
  const sender = plan.senders.find(item => item.label === 'scope');
  const session = new SmokeSession({
    target: TARGET,
    remote: {},
    http: {},
    evidence: { addSecret() {} },
    plan,
  });
  const response = {
    accountId: sender.accountID,
    senderId: sender.id,
    operationId: 'operation-1',
    attempt: 0,
    userSession: { token: 'same-token', expiresAt: '2026-10-20T03:00:00Z' },
    uploader: { token: 'same-token', expiresAt: '2026-10-20T03:00:00Z' },
    refresh: { token: 'same-token', expiresAt: '2026-10-27T00:00:00Z' },
  };
  assert.throws(
    () => session.acceptRefresh('scope', response, { operationID: 'operation-1', attempt: 0 }),
    /non-independent/,
  );
});

test('runner refresh checkpoints must match the persisted operation and attempt', async () => {
  const plan = createFixturePlan({ runId: 'checkpoint-proof-run' });
  const sender = plan.senders.find(item => item.id.endsWith('-scope'));
  sender.latestRefresh = { operationID: 'operation-1', attempt: 1 };
  const session = {
    plan,
    remote: {
      async sqlJSON(statement) {
        if (statement.includes('token_digest')) {
          return sender.credentials.map(item => ({ senderId: sender.id, tokenDigest: item.digest }));
        }
        return [{ senderId: sender.id, operationId: 'operation-1', attempt: 1, latest: true }];
      },
    },
  };
  assert.deepEqual(await verifyCheckpoints(session), {
    checkpoints: [{ senderId: sender.id, operationId: 'operation-1', attempt: 1 }],
    credentialBundles: 1,
  });

  session.remote.sqlJSON = async statement => (
    statement.includes('token_digest')
      ? sender.credentials.map(item => ({ senderId: 'another-sender', tokenDigest: item.digest }))
      : [{ senderId: sender.id, operationId: 'operation-1', attempt: 1, latest: true }]
  );
  await assert.rejects(() => verifyCheckpoints(session), /credential bundle/);

  session.remote.sqlJSON = async statement => (
    statement.includes('token_digest')
      ? sender.credentials.map(item => ({ senderId: sender.id, tokenDigest: item.digest }))
      : [{ senderId: sender.id, operationId: 'operation-1', attempt: 1, latest: false }]
  );
  await assert.rejects(() => verifyCheckpoints(session), /checkpoint is stale/);
});

test('service diagnostics settle and scan every available run-owned record', async () => {
  let reads = 0;
  const session = {
    requestIds: ['request-1', 'request-2'],
    subjects: new Set(),
    secretValues: () => ['raw-secret'],
    remote: {
      async sqlJSON() {
        reads++;
        const records = [{ requestId: 'request-1', subject: null, method: 'POST' }];
        if (reads > 1) records.push({ requestId: 'request-2', subject: null, method: 'GET' });
        return records;
      },
    },
  };
  assert.deepEqual(await verifyDiagnosticRedaction(session), {
    records: 2,
    scannedRequestIds: 2,
    missingRequestIds: 0,
  });
  assert.equal(reads, 2);

  session.requestIds = ['request-1', 'request-2'];
  session.remote.sqlJSON = async () => [{ requestId: 'request-1', subject: null, method: 'POST' }];
  assert.deepEqual(await verifyDiagnosticRedaction(session, { settleMs: 0 }), {
    records: 1,
    scannedRequestIds: 1,
    missingRequestIds: 1,
  });

  session.requestIds = ['request-1'];
  session.remote.sqlJSON = async () => [{ requestId: 'request-1', subject: null, method: 'raw-secret' }];
  await assert.rejects(() => verifyDiagnosticRedaction(session), /raw credential/);
});

test('unavailable service diagnostics are recorded as blocked evidence', async () => {
  const recorded = [];
  const session = {
    requestIds: ['request-1'],
    subjects: new Set(),
    secretValues: () => [],
    remote: { async sqlJSON() { return []; } },
    evidence: { record(entry) { recorded.push(entry); } },
  };
  await assert.rejects(
    () => runScenario(
      session,
      'diagnostic availability',
      () => verifyDiagnosticRedaction(session, { settleMs: 0 }),
    ),
    error => error instanceof SmokeBlocked,
  );
  assert.equal(recorded[0].status, 'blocked');
});

test('ordinary Pairing creates, claims, relays through both peers and redeems only for its Receiver', async () => {
  const requests = [];
  const messages = {
    sender: [
      { json: { type: 'pairing_state', session: { sessionId: 'session-1' } } },
      { json: { type: 'pairing_relay', side: 'receiver', payload: 'AAAA' } },
    ],
    receiver: [{ json: { type: 'pairing_state', session: { sessionId: 'session-1' } } }],
  };
  const socket = side => ({
    async next() {
      const result = messages[side].shift();
      assert(result, `${side} socket has no queued message`);
      return result;
    },
    sendJSON(value) {
      requests.push({ side, value });
    },
    async close() {},
  });
  const plan = createFixturePlan({ runId: 'pairing-run' });
  const session = new SmokeSession({
    target: TARGET,
    remote: {},
    evidence: { addSecret() {} },
    plan,
    http: {
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
              pairingToken: 'restricted',
              pairingTokenExpiresAt: '2026-10-21T00:05:00Z',
            },
          };
        }
        if (request.path.endsWith('/authorize')) {
          return {
            body: {
              sessionId: 'session-1',
              mode: 'new_receiver',
              receiverId: 'receiver-1',
              pairingId: 'pairing-1',
              state: 'awaiting_confirmation',
            },
          };
        }
        if (request.path.endsWith('/complete')) return { body: { state: 'completed' } };
        if (request.path.endsWith('/receiver-token')) {
          return {
            cacheControl: 'no-store',
            body: { receiverId: 'receiver-1', receiverToken: 'receiver-secret' },
          };
        }
        throw new Error(`unexpected request ${request.path}`);
      },
    },
  });
  session.openSocket = ({ token }) => socket(token === 'restricted' ? 'receiver' : 'sender');

  const pairing = await session.createPairing('a1');
  assert.equal(pairing.pairingID, 'pairing-1');
  await session.completePairing('a1', pairing);
  const issued = await session.redeemReceiver(pairing);
  assert.equal(issued.receiverId, 'receiver-1');
  assert.equal(session.receivers.has('receiver-1'), true);
  assert.equal(requests.find(request => request.path === '/v1/pairing-sessions').body.mode, 'new_receiver');
  const relay = requests.find(request => request.value?.type === 'pairing_relay');
  assert.equal(relay.side, 'receiver');
  assert.equal(relay.value.version, 1);
  let cleaned = false;
  session.cleanup = async () => {
    cleaned = true;
  };
  await session.close();
  assert.equal(cleaned, true);
});

test('pairing peers are closed when the relay journey fails', async () => {
  const closed = [];
  const plan = createFixturePlan({ runId: 'pairing-close-run' });
  const session = new SmokeSession({
    target: TARGET,
    remote: {},
    evidence: { addSecret() {} },
    plan,
    http: {},
  });
  const socket = {
    async next() {
      return { json: { type: 'pairing_state' } };
    },
    sendJSON() {},
    async close() {
      closed.push('receiver');
    },
  };
  session.openSocket = async () => ({
    async next() {
      throw new Error('sender relay failed');
    },
    async close() {
      closed.push('sender');
    },
  });
  await assert.rejects(
    () => session.completePairing('a1', { sessionID: 'session-1', token: 'restricted' }, socket),
    /sender relay failed/,
  );
  assert.deepEqual(closed.sort(), ['receiver', 'sender']);
});

test('failed lifecycle work still runs its cleanup and records that outcome', async () => {
  const plan = createFixturePlan({ runId: 'pairing-failure-run' });
  const recorded = [];
  let closes = 0;
  const session = {
    requestIds: [],
    async close() {
      closes++;
    },
  };
  await assert.rejects(() => integration({
    target: TARGET,
    checkResult: { remote: {} },
    evidence: { record(entry) { recorded.push(entry); } },
    plan,
    repairDeliveryStorage: async () => {},
    createSession: async () => session,
    runModel: async () => ({ provider: 'bailian' }),
    runLifecycle: async () => {
      throw new Error('authorization failed');
    },
  }), /authorization failed/);
  assert.equal(closes, 1);
  assert.equal(recorded.at(-1).scenario, 'run-owned fixture cleanup');
  assert.equal(recorded.at(-1).status, 'passed');
});

test('a failed cleanup is recorded and an error after close never cleans up twice', async () => {
  const plan = createFixturePlan({ runId: 'cleanup-failure-run' });
  const recorded = [];
  let closes = 0;
  const session = {
    requestIds: [],
    async close() {
      closes++;
      throw new Error('cleanup failed');
    },
  };
  await assert.rejects(() => integration({
    target: TARGET,
    checkResult: { remote: {} },
    evidence: { record(entry) { recorded.push(entry); } },
    plan,
    repairDeliveryStorage: async () => {},
    createSession: async () => session,
    runModel: async () => ({ provider: 'bailian' }),
    runLifecycle: async () => {
      throw new Error('authorization failed');
    },
  }), /authorization failed/);
  assert.equal(closes, 1);
  assert.equal(recorded.at(-1).status, 'failed');
  assert.match(recorded.at(-1).details.reason, /cleanup failed/);

  let closeCount = 0;
  const successful = {
    requestIds: [],
    async close() { closeCount++; },
  };
  const evidence = {
    addSecret() {},
    record(entry) {
      if (entry.scenario === 'run-owned fixture cleanup') throw new Error('evidence failed');
    },
  };
  await assert.rejects(() => integration({
    target: TARGET,
    checkResult: {},
    evidence,
    plan,
    createSession: async () => successful,
    runModel: async () => ({ provider: 'bailian' }),
    runLifecycle: async () => ({ scenarios: [] }),
  }), /evidence failed/);
  assert.equal(closeCount, 1);
});
