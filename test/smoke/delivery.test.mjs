import assert from 'node:assert/strict';
import test from 'node:test';

import { SmokeHttp, SmokeProblem } from './lib/http.mjs';
import { withDeliveryStorageFault } from './lib/delivery_faults.mjs';
import { runDeliveryScenario } from './lib/session_delivery_support.mjs';
import { expectNoMessage } from './lib/socket_assertions.mjs';
import { TARGET } from './lib/target.mjs';

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

test('the Delivery storage fault is restored after a failed scenario', async () => {
  const calls = [];
  const remote = {
    target: { deploymentDir: '/srv/pin-glide-dev' },
    compose: "sudo -n docker compose --project-directory '/srv/pin-glide-dev'",
    ssh(script) {
      calls.push(script);
      return Promise.resolve({
        stdout: calls.length === 1
          ? '/tmp/pin-glide-dev-smoke-delivery.Ab3dEf|/tmp/pin-glide-dev-smoke-delivery.Ab3dEf.sh\n'
          : '',
      });
    },
    sql(statement) {
      calls.push(statement);
      return Promise.resolve();
    },
  };
  await assert.rejects(
    () => withDeliveryStorageFault(remote, async () => {
      throw new Error('scenario failed');
    }),
    /scenario failed/,
  );
  assert.match(calls[1], /current_database\(\) = 'pin_glide'[\s\S]*ALTER TABLE pin_glide\.delivery_deliveries RENAME TO smoke_delivery_hidden/);
  assert.match(calls[2], /EXECUTE .*ALTER TABLE pin_glide\.smoke_delivery_hidden RENAME TO delivery_deliveries/);
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
