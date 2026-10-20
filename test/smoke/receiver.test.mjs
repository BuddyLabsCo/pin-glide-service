import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

import {
  createFixturePlan,
  renderInvalidateReceiverCredentialSQL,
} from './lib/fixtures.mjs';
import { SmokeProblem } from './lib/http.mjs';
import { runProtocolScenario } from './lib/session_pairing_support.mjs';
import {
  deterministicEvidence,
  deliverOnce,
  expectedProblem,
  withApiRestart,
} from './lib/session_receiver_support.mjs';
import { TARGET } from './lib/target.mjs';

test('Receiver credential invalidation SQL is guarded, run-owned and escaped', () => {
  const plan = createFixturePlan({ runId: 'receiver-test' });
  assert.throws(() => renderInvalidateReceiverCredentialSQL(plan, ''), /Receiver ID/);

  const receiverID = "receiver-'run";
  const sql = renderInvalidateReceiverCredentialSQL(plan, receiverID);
  assert.match(sql, /current_database\(\) = 'pin_glide'/);
  assert.match(sql, /WHERE id = 'receiver-''run'/);
  assert.match(sql, /account_id = ANY\(ARRAY\['smoke-receiver-test-account-a'/);
  assert.match(sql, /SET revoked_at = now\(\)/);
  assert.match(sql, /revoked_at IS NULL/);
  assert(!sql.includes(plan.senders[0].credentials[0].token));
});

test('an API restart restores the service after success or failure', async () => {
  const actions = [];
  const remote = {
    async stopApi() {
      actions.push('stop');
    },
    async startApi() {
      actions.push('start');
    },
  };
  assert.equal(await withApiRestart(remote, async () => {
    actions.push('run');
    return 'closed';
  }), 'closed');
  assert.deepEqual(actions, ['stop', 'run', 'start']);

  actions.length = 0;
  await assert.rejects(
    () => withApiRestart(remote, async () => {
      actions.push('run');
      throw new Error('upgrade did not close');
    }),
    /upgrade did not close/,
  );
  assert.deepEqual(actions, ['stop', 'run', 'start']);

  actions.length = 0;
  await assert.rejects(
    () => withApiRestart({
      async stopApi() {
        actions.push('stop');
        throw new Error('stop answered after stopping');
      },
      async startApi() {
        actions.push('start');
      },
    }, async () => {
      actions.push('run');
    }),
    /stop answered after stopping/,
  );
  assert.deepEqual(actions, ['stop', 'start']);
});

test('exact pre-upgrade problems require every published field', () => {
  const complete = new SmokeProblem(409, {
    type: '/problems/receiver_already_connected',
    title: 'Receiver already connected',
    status: 409,
    detail: 'This Receiver already holds a delivery connection.',
    code: 'receiver_already_connected',
    requestId: 'request-1',
  }, 'request-1');
  assert(expectedProblem(409, 'receiver_already_connected')(complete));

  const missingDetail = new SmokeProblem(409, {
    type: '/problems/receiver_already_connected',
    title: 'Receiver already connected',
    status: 409,
    code: 'receiver_already_connected',
    requestId: 'request-1',
  }, 'request-1');
  assert(!expectedProblem(409, 'receiver_already_connected')(missingDetail));

  const wrongBodyStatus = new SmokeProblem(409, {
    type: '/problems/receiver_already_connected',
    title: 'Receiver already connected',
    status: 401,
    detail: 'This Receiver already holds a delivery connection.',
    code: 'receiver_already_connected',
    requestId: 'request-1',
  }, 'request-1');
  assert(!expectedProblem(409, 'receiver_already_connected')(wrongBodyStatus));
});

test('a live delivery is correlated and acknowledged before its replay check', async () => {
  const messages = [{
    json: {
      type: 'delivery',
      senderId: 'sender-1',
      receiverId: 'receiver-1',
      pairingId: 'pairing-1',
      messageId: 'message-1',
      nonce: 'AAAAAAAAAAAAAAAA',
      ciphertext: 'AQIDBAUGBwgJCgsMDQ4PEBESExQ=',
    },
  }];
  const receipts = [];
  const session = {
    newOperationID: () => 'message-1',
    sender: () => ({ id: 'sender-1' }),
    token: () => 'uploader-secret',
    request: async () => ({
      status: 207,
      requestId: 'request-1',
      body: { results: [{ status: 'received' }] },
    }),
  };
  const socket = {
    next: async () => messages.shift(),
    sendJSON: value => receipts.push(value),
  };

  const delivered = await deliverOnce(
    session,
    'receiver-main',
    { receiverID: 'receiver-1', pairingID: 'pairing-1' },
    socket,
    'test',
  );
  assert.equal(delivered.messageId, 'message-1');
  assert.equal(delivered.status, 'received');
  assert.deepEqual(receipts, [{
    version: 1,
    type: 'receipt',
    senderId: 'sender-1',
    messageId: 'message-1',
  }]);
});

test('deterministic evidence names tests that still exist at their stated level', () => {
  for (const item of deterministicEvidence) {
    assert.match(item.level, /^(unit|integration)$/);
    const source = readFileSync(new URL(`../../${item.source}`, import.meta.url), 'utf8');
    assert(source.includes(`func ${item.test}(`), `${item.test} is missing from ${item.source}`);
  }
});

test('Receiver evidence maps published HTTP operations and WSS messages', async () => {
  const recorded = [];
  const session = {
    requestIds: [],
    target: { ...TARGET, sourceCommit: 'source-commit' },
    evidence: {
      record(entry) {
        recorded.push(entry);
      },
    },
  };
  await runProtocolScenario(
    session,
    'Receiver contract identity',
    { http: ['GetSelfReceivers'], wss: ['receipt'] },
    async () => ({ exercised: true }),
  );
  assert.equal(recorded[0].scenario, 'Receiver contract identity');
  assert.deepEqual(recorded[0].details.contract.http, [{
    name: 'GetSelfReceivers',
    source: 'generated/protocol/openapi.yaml',
  }]);
  assert.deepEqual(recorded[0].details.contract.wss, [{
    name: 'receipt',
    source: 'generated/protocol/asyncapi.yaml',
  }]);
  assert.equal(recorded[0].details.contract.sourceCommit, 'source-commit');
  assert.equal(recorded[0].details.exercised, true);
});
