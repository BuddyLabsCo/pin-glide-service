import assert from 'node:assert/strict';
import test from 'node:test';

import { createFixturePlan } from '../harness/fixtures.mjs';
import { renderInvalidateReceiverCredentialSQL } from './fixtures.mjs';
import { SmokeBlocked, SmokeProblem } from '../harness/http.mjs';
import { runProtocolScenario } from '../pairing/support.mjs';
import {
  coverageTests,
  deliverOnce,
  deterministicTestReferences,
  expectedProblem,
  parseRanTests,
  runDeterministicTests,
  withApiRestart,
} from './support.mjs';
import { TARGET } from '../harness/target.mjs';

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
  const nonce = 'cmVjZWl2ZXItdGVzdA==';
  const ciphertext = Buffer.from('0123456789abcdefg').toString('base64');
  const messages = [{
    json: {
      type: 'delivery',
      senderId: 'sender-1',
      receiverId: 'receiver-1',
      pairingId: 'pairing-1',
      messageId: 'message-1',
      nonce,
      ciphertext,
    },
  }];
  const receipts = [];
  const session = {
    addSecret() {},
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
    { nonce, ciphertext },
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

test('deterministic evidence requires every referenced test to have passed', () => {
  const references = deterministicTestReferences.filter(item => item.level === 'unit');
  const output = references.map(item => `--- PASS: ${item.test} (0.00s)`).join('\n');
  assert.deepEqual(parseRanTests(output, references), references.map(item => item.test));
  assert.throws(
    () => parseRanTests(output.replace(references[0].test, 'TestSomethingElse'), references),
    new RegExp(references[0].test),
  );
});

test('deterministic tests run once over the referenced packages', async () => {
  const references = deterministicTestReferences.filter(item => item.package === './internal/api/wss');
  const calls = [];
  const outcome = await runDeterministicTests('/repository', references, async (command, args, options) => {
    calls.push({ command, args, options });
    return { stdout: references.map(item => `--- PASS: ${item.test} (0.00s)`).join('\n') };
  });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].command, 'go');
  assert.equal(calls[0].options.cwd, '/repository');
  assert.equal(calls[0].args[calls[0].args.indexOf('-run') + 1], `^(${references.map(item => item.test).join('|')})$`);
  assert.equal(calls[0].args.at(-1), './internal/api/wss');
  assert.deepEqual(outcome.packages, ['./internal/api/wss']);
});

test('deterministic evidence binds a pass to its own package', async () => {
  // The same test name in two packages: a name-union implementation would
  // accept the second package's reference from the first package's run.
  const references = [
    { test: 'TestSameName', package: './internal/api/wss', level: 'unit', covers: [], observes: '' },
    { test: 'TestSameName', package: './test/integration/pairing', level: 'integration', covers: [], observes: '' },
  ];
  const calls = [];
  await assert.rejects(
    () => runDeterministicTests('/repository', references, async (command, args) => {
      calls.push(args);
      if (command === 'docker') return { stdout: '29.0.0' };
      // Only the first package reports the shared name.
      return { stdout: args.at(-1) === './internal/api/wss' ? '--- PASS: TestSameName (0.00s)' : 'no tests to run' };
    }),
    /TestSameName/,
  );
  assert.equal(calls.filter(args => args[0] === 'test').length, 2);
});

test('a failed deterministic run names the failing test', async () => {
  const references = deterministicTestReferences.filter(item => item.package === './internal/api/wss');
  await assert.rejects(
    () => runDeterministicTests('/repository', references, async () => {
      const error = new Error('go failed with exit code 1');
      error.result = {
        stdout: [
          `--- PASS: ${references[0].test} (0.00s)`,
          `--- FAIL: ${references[1].test} (0.01s)`,
        ].join('\n'),
      };
      throw error;
    }),
    new RegExp(`${references[1].test} failed`),
  );
});

test('a required coverage with no executed test is a hole, not a pass', () => {
  const references = [
    { test: 'TestPolicy', covers: ['policy-close'], package: './p', level: 'unit', observes: '' },
  ];
  assert.deepEqual(coverageTests(references, new Set(['TestPolicy']), 'policy-close'), ['TestPolicy']);
  assert.throws(
    () => coverageTests(references, new Set(['TestPolicy']), 'failed-upgrade-cleanup'),
    /no executed deterministic test covers failed-upgrade-cleanup/,
  );
  assert.throws(
    () => coverageTests(references, new Set(), 'policy-close'),
    /no executed deterministic test covers policy-close/,
  );
});

test('a missing Go toolchain blocks rather than fails the deterministic check', async () => {
  const references = deterministicTestReferences.filter(item => item.level === 'unit');
  await assert.rejects(
    () => runDeterministicTests('/repository', references, async () => {
      const error = new Error('spawn go ENOENT');
      error.code = 'ENOENT';
      throw error;
    }),
    error => error instanceof SmokeBlocked,
  );
});

test('unavailable Docker blocks the deterministic integration checks', async () => {
  const references = deterministicTestReferences.filter(item => item.level === 'integration');
  await assert.rejects(
    () => runDeterministicTests('/repository', references, async () => {
      throw new Error('Cannot connect to the Docker daemon');
    }),
    error => error instanceof SmokeBlocked && /Docker/.test(error.message),
  );
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
