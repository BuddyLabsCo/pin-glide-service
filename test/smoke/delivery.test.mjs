import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import {
  chmodSync,
  existsSync,
  mkdtempSync,
  readFileSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';

import { deliveryRestoreWatchdog, withDeliveryBatchLock } from './lib/delivery_faults.mjs';
import { createFixturePlan } from './lib/fixtures.mjs';
import { SmokeHttp, SmokeProblem } from './lib/http.mjs';
import {
  expectMetadata,
  nextDelivery,
  runDeliveryScenario,
  waitForAdmitted,
} from './lib/session_delivery_support.mjs';
import { expectNoMessage } from './lib/socket_assertions.mjs';
import { TARGET } from './lib/target.mjs';

test('Delivery admission polling accepts a committed count after retrying', async () => {
  const session = {
    plan: createFixturePlan({ runId: 'delivery-admission' }),
    remote: {
      async sqlJSON() {
        return { count: 1 };
      },
    },
  };
  await waitForAdmitted(session, 'pair2', ['message-1'], 1, 100);
});

test('Delivery metadata compares timestamp instants across wire formats', () => {
  expectMetadata({
    status: 200,
    body: {
      deliveries: [{
        receiverId: 'receiver-1',
        status: 'received',
        createdAt: '2026-10-21T17:00:00.123456+00:00',
        completedAt: '2026-10-21T17:00:01.123456+00:00',
      }],
    },
  }, [{
    receiverID: 'receiver-1',
    status: 'received',
    createdAt: '2026-10-21T17:00:00.123456Z',
    completedAt: '2026-10-21T17:00:01.123456Z',
  }]);
});

test('Delivery frames must carry the exact submitted payload', async () => {
  const item = {
    receiverId: 'receiver-1',
    pairingId: 'pairing-1',
    cryptoVersion: 1,
    nonce: 'nonce-1',
    ciphertext: 'ciphertext-1',
  };
  const association = {
    senderID: 'sender-1',
    receiverID: item.receiverId,
    pairingID: item.pairingId,
  };
  const frame = {
    type: 'delivery',
    senderId: association.senderID,
    receiverId: association.receiverID,
    pairingId: association.pairingID,
    messageId: 'message-1',
    cryptoVersion: item.cryptoVersion,
    nonce: item.nonce,
    ciphertext: item.ciphertext,
  };
  await nextDelivery({ next: async () => ({ json: frame }) }, association, {
    messageId: frame.messageId,
    item,
  });
  for (const replacement of [
    { messageId: 'other-message' },
    { cryptoVersion: 2 },
    { nonce: 'other-nonce' },
    { ciphertext: 'other-ciphertext' },
  ]) {
    await assert.rejects(
      () => nextDelivery(
        { next: async () => ({ json: { ...frame, ...replacement } }) },
        association,
        { messageId: frame.messageId, item },
      ),
      /did not carry a submitted payload/,
    );
  }
});

test('an unexpected Delivery frame is reported without its ciphertext', async () => {
  const socket = {
    next: async () => ({
      text: '{"type":"delivery","nonce":"secret-nonce","ciphertext":"secret-ciphertext"}',
      json: { type: 'delivery', nonce: 'secret-nonce', ciphertext: 'secret-ciphertext' },
    }),
  };
  await assert.rejects(
    () => expectNoMessage(socket, 'Unexpected frame'),
    error => /Unexpected frame: received an unexpected WebSocket frame delivery/.test(error.message)
      && !error.message.includes('secret'),
  );
});

test('the HTTP peer carries malformed raw upload bodies without JSON re-encoding them', async () => {
  let received;
  const peer = new SmokeHttp({
    origin: TARGET.httpOrigin,
    fetchImpl: async (_url, options) => {
      received = options.body;
      return new Response(JSON.stringify({
        type: 'about:blank',
        title: 'Invalid request',
        status: 400,
        code: 'invalid_request',
        requestId: 'request-1',
      }), { status: 400, headers: { 'content-type': 'application/problem+json' } });
    },
  });
  await assert.rejects(
    () => peer.call({ method: 'POST', path: '/v1/messages', rawBody: '{"messageId":' }),
    error => error instanceof SmokeProblem
      && error.status === 400
      && error.code === 'invalid_request',
  );
  assert.equal(received, '{"messageId":');
  await assert.rejects(
    () => peer.call({ path: '/v1/messages', body: {}, rawBody: '{}' }),
    /both body and rawBody/,
  );
});

test('the Delivery restore watchdog retries until the restore succeeds', () => {
  const directory = mkdtempSync(path.join(tmpdir(), 'pin-glide-delivery-watchdog-'));
  const guard = path.join(directory, 'guard');
  const watchdog = `${guard}.sh`;
  const calls = path.join(directory, 'calls');
  const lockCalls = path.join(directory, 'lock-calls');
  const psql = path.join(directory, 'psql');
  const flock = path.join(directory, 'flock');
  writeFileSync(guard, '');
  writeFileSync(`${guard}.sql`, 'SELECT 1;\n');
  writeFileSync(psql, `#!/bin/sh
cat >/dev/null
test -s ${lockCalls}
printf 'run\\n' >> ${calls}
test "$(wc -l < ${calls})" -ge 2
`);
  writeFileSync(flock, `#!/bin/sh
printf '%s\\n' "$*" >> ${lockCalls}
`);
  chmodSync(psql, 0o700);
  chmodSync(flock, 0o700);
  writeFileSync(watchdog, deliveryRestoreWatchdog(
    { compose: psql },
    { intervalSeconds: 0, lockFile: path.join(directory, 'lock') },
  ));
  chmodSync(watchdog, 0o700);

  execFileSync('sh', [watchdog, guard], {
    env: { ...process.env, PATH: `${directory}:${process.env.PATH}` },
  });
  assert.equal(existsSync(guard), false);
  assert.equal(existsSync(`${guard}.sql`), false);
  assert.equal(readFileSync(calls, 'utf8').trim().split('\n').length, 2);
  assert.equal(readFileSync(lockCalls, 'utf8').trim(), '9');
});

test('a failed batch-lock acquisition waits for the holder before rejecting', async () => {
  const order = [];
  const remote = {
    compose: "sudo -n docker compose --project-directory '/srv/pin-glide-dev'",
    ssh() {
      return new Promise(resolve => {
        setTimeout(() => {
          order.push('holder');
          resolve({ stdout: '' });
        }, 20);
      });
    },
    sqlJSON() {
      if (!order.includes('holder')) {
        order.push('poll');
        throw new Error('transient poll failure');
      }
      return Promise.resolve({ held: 0 });
    },
  };
  await assert.rejects(
    () => withDeliveryBatchLock(remote, async () => {
      order.push('callback');
    }),
    /transient poll failure/,
  );
  assert.deepEqual(order, ['poll', 'holder']);
});

test('Delivery evidence records published operations and messages with their source digests', async () => {
  const recorded = [];
  const session = {
    requestIds: ['request-1'],
    target: { ...TARGET, sourceCommit: 'source-commit' },
    evidence: { record(entry) { recorded.push(entry); } },
  };
  await runDeliveryScenario(
    session,
    'delivery contract identity',
    { http: ['PostMessages', 'GetMessage'], wss: ['delivery', 'receipt'] },
    async () => ({ exercised: true }),
  );
  const details = recorded[0].details;
  assert.deepEqual(details.contract.http.map(item => item.name), ['PostMessages', 'GetMessage']);
  assert.deepEqual(details.contract.wss.map(item => item.name), ['delivery', 'receipt']);
  assert.match(details.contract.openapi.digest, /^[a-f0-9]{64}$/);
  assert.match(details.contract.asyncapi.digest, /^[a-f0-9]{64}$/);
  assert.equal(details.contract.sourceCommit, 'source-commit');
});
