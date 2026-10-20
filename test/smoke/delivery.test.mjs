import assert from 'node:assert/strict';
import test from 'node:test';

import { SmokeHttp, SmokeProblem } from './lib/http.mjs';
import {
  deliveryItem,
  expectMetadata,
  expectResult,
  runDeliveryScenario,
} from './lib/session_delivery_support.mjs';
import { TARGET } from './lib/target.mjs';

function response(body, status = 207) {
  return {
    status,
    requestId: `request-${status}`,
    body,
  };
}

test('Delivery results require the original index, readable Receiver and stable business code', () => {
  const result = { index: 0, receiverId: 'receiver-1', status: 'offline', error: { code: 'offline' } };
  assert.equal(expectResult(response({ results: [result] }), 0, 'receiver-1', 'offline', 'offline'), result);
  const unreadable = { index: 0, status: 'rejected', error: { code: 'invalid_delivery_item' } };
  assert.equal(
    expectResult(response({ results: [unreadable] }), 0, '', 'rejected', 'invalid_delivery_item'),
    unreadable,
  );
  assert.throws(
    () => expectResult(response({ results: [{ ...result, index: 1 }] }), 0, 'receiver-1', 'offline', 'offline'),
    /Result 0/,
  );
  assert.throws(
    () => expectResult(response({ results: [{ ...result, error: { code: 'send_failed' } }] }), 0, 'receiver-1', 'offline', 'offline'),
    /code = send_failed/,
  );
});

test('retained metadata requires one view per Receiver, exact outcomes and stable terminal fields', () => {
  const created = '2026-10-21T00:00:00Z';
  const completed = '2026-10-21T00:00:05Z';
  const body = {
    deliveries: [
      { receiverId: 'receiver-1', status: 'received', createdAt: created, completedAt: completed },
      { receiverId: 'receiver-2', status: 'offline', createdAt: created, completedAt: completed, error: { code: 'offline' } },
    ],
  };
  const views = expectMetadata(response(body, 200), [
    { receiverID: 'receiver-1', status: 'received', createdAt: created, completedAt: completed },
    { receiverID: 'receiver-2', status: 'offline', code: 'offline' },
  ]);
  assert.equal(views['receiver-1'].status, 'received');
  assert.throws(
    () => expectMetadata(response(body, 200), [{ receiverID: 'receiver-1', status: 'offline' }]),
    /reports 2 Deliveries/,
  );
  assert.throws(
    () => expectMetadata(response({ deliveries: [{ ...body.deliveries[0], completedAt: 'changed' }] }, 200), [
      { receiverID: 'receiver-1', status: 'received', completedAt: completed },
    ]),
    /terminal time changed/,
  );
});

test('opaque Delivery fixtures carry random padded Base64 nonce and ciphertext fields', () => {
  const pairing = { receiverID: 'receiver-1', pairingID: 'pairing-1' };
  const first = deliveryItem(pairing);
  const second = deliveryItem(pairing);
  assert.notEqual(first.nonce, second.nonce);
  assert.notEqual(first.ciphertext, second.ciphertext);
  assert.equal(Buffer.from(first.nonce, 'base64').length, 12);
  assert.equal(Buffer.from(first.ciphertext, 'base64').length, 48);
  assert.equal(first.cryptoVersion, 1);
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
