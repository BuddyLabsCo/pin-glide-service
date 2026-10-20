import assert from 'node:assert/strict';
import test from 'node:test';

import { createFixturePlan } from './lib/fixtures.mjs';
import { SmokeProblem } from './lib/http.mjs';
import { SmokeSession } from './lib/session.mjs';
import { TARGET } from './lib/target.mjs';
import { integration } from './runner.mjs';

test('refresh authority is proved with the rotated management and uploader credentials', async () => {
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
        return { status: 200, requestId: 'request-refresh', body: { senderId: sender.id, ...rotated } };
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
    rotated.userSession.token,
    rotated.uploader.token,
  ]);
  assert.equal(sender.credentials.find(item => item.scope === 'refresh').token, rotated.refresh.token);
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
          return { status: 201, body: { session: { sessionId: 'session-1', receiverId: 'receiver-1' }, pairingToken: 'restricted' } };
        }
        if (request.path.endsWith('/authorize')) {
          return { body: { sessionId: 'session-1', receiverId: 'receiver-1', pairingId: 'pairing-1', state: 'awaiting_confirmation' } };
        }
        if (request.path.endsWith('/complete')) return { body: { state: 'completed' } };
        if (request.path.endsWith('/receiver-token')) {
          return { body: { receiverId: 'receiver-1', receiverToken: 'receiver-secret' } };
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

test('failed Pairing work still runs its cleanup and records that outcome', async () => {
  const plan = createFixturePlan({ runId: 'pairing-failure-run' });
  const recorded = [];
  let closes = 0;
  const session = {
    async createPairing() {
      throw new Error('authorization failed');
    },
    async close() {
      closes++;
    },
  };
  await assert.rejects(() => integration({
    target: TARGET,
    checkResult: {},
    evidence: { record(entry) { recorded.push(entry); } },
    plan,
    createSession: async () => session,
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
    async createPairing() {
      throw new Error('authorization failed');
    },
    async close() {
      closes++;
      throw new Error('cleanup failed');
    },
  };
  await assert.rejects(() => integration({
    target: TARGET,
    checkResult: {},
    evidence: { record(entry) { recorded.push(entry); } },
    plan,
    createSession: async () => session,
  }), /authorization failed/);
  assert.equal(closes, 1);
  assert.equal(recorded.at(-1).status, 'failed');
  assert.match(recorded.at(-1).details.reason, /cleanup failed/);

  let closeCount = 0;
  const successful = {
    async createPairing() { return { receiverID: 'receiver-1', pairingID: 'pairing-1' }; },
    async completePairing() {},
    async redeemReceiver() { return { receiverId: 'receiver-1', receiverToken: 'secret' }; },
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
  }), /evidence failed/);
  assert.equal(closeCount, 1);
});
