import assert from 'node:assert/strict';
import test from 'node:test';

import { createFixturePlan } from './lib/fixtures.mjs';
import { SmokeSession } from './lib/session.mjs';
import { TARGET } from './lib/target.mjs';
import { integration } from './runner.mjs';

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
  const order = [];
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
    repairDeliveryStorage: async () => {
      order.push('repair');
    },
    createSession: async () => {
      order.push('create');
      return session;
    },
    runModel: async () => ({ provider: 'bailian' }),
    runLifecycle: async () => {
      order.push('lifecycle');
      throw new Error('authorization failed');
    },
  }), /authorization failed/);
  assert.deepEqual(order, ['repair', 'create', 'lifecycle']);
  assert.equal(closes, 1);
  assert.equal(recorded.at(-1).scenario, 'run-owned fixture cleanup');
  assert.equal(recorded.at(-1).status, 'passed');
});

test('a failed cleanup is recorded and an error after close never cleans up twice', async () => {
  const plan = createFixturePlan({ runId: 'cleanup-failure-run' });
  const recorded = [];
  const order = [];
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
    repairDeliveryStorage: async () => {
      order.push('repair');
    },
    createSession: async () => {
      order.push('create');
      return session;
    },
    runModel: async () => ({ provider: 'bailian' }),
    runLifecycle: async () => {
      throw new Error('authorization failed');
    },
  }), /authorization failed/);
  assert.deepEqual(order, ['repair', 'create']);
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

test('fixture cleanup passes only after re-querying every run-owned resource class', async () => {
  const plan = createFixturePlan({ runId: 'cleanup-verification-run' });
  const empty = {
    accounts: 0,
    senders: 0,
    credentials: 0,
    refreshOperations: 0,
    receivers: 0,
    receiverCredentials: 0,
    pairings: 0,
    pairingSessions: 0,
    deliveries: 0,
    diagnostics: 0,
    schemaAudits: 0,
    cronJobs: 0,
    cronRuns: 0,
  };
  let sqlJSONCalls = 0;
  const session = new SmokeSession({
    target: TARGET,
    remote: {
      async sql() {},
      async sqlJSON() {
        sqlJSONCalls++;
        return empty;
      },
    },
    http: {},
    evidence: { addSecret() {} },
    plan,
  });
  session.requestIds.push('request-cleanup');
  session.subjects.add('pairing-cleanup');
  session.receivers.add('receiver-cleanup');
  session.retentionRunIDs.add(7);
  await session.cleanup();
  assert.equal(sqlJSONCalls, 1);
  assert.equal(session.requestIds.length, 0);
  assert.equal(session.subjects.size, 0);
  assert.equal(session.receivers.size, 0);
  assert.equal(session.retentionRunIDs.size, 0);

  const dirty = new SmokeSession({
    target: TARGET,
    remote: {
      async sql() {},
      async sqlJSON() {
        return { ...empty, deliveries: 1 };
      },
    },
    http: {},
    evidence: { addSecret() {} },
    plan,
  });
  await assert.rejects(() => dirty.cleanup(), /left rows.*deliveries/);
});
