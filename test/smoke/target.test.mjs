import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import test from 'node:test';

import {
  createFixturePlan,
  credentialLifetimeHours,
  escapeSQLString,
  fixtureSender,
  renderBootstrapSQL,
} from './lib/fixtures.mjs';
import { SmokeHttp, SmokeProblem } from './lib/http.mjs';
import { checkExitCode } from './runner.mjs';
import { TARGET, targetFromOptions, validateTarget } from './lib/target.mjs';

test('the smoke target accepts only the adopted LAN installation', () => {
  const resolved = validateTarget({ release: '1.2.3' });
  assert.deepEqual(resolved, { ...TARGET, release: '1.2.3' });

  for (const override of [
    { sshHost: 'production' },
    { project: 'pin-glide' },
    { deploymentDir: '/srv/pin-glide' },
    { database: 'postgres' },
    { httpOrigin: 'https://192.168.10.17:33489' },
    { wsOrigin: 'ws://192.168.10.17:8080' },
  ]) {
    assert.throws(() => validateTarget({ release: '1.2.3', ...override }), /adopted/i);
  }

  assert.throws(() => validateTarget({}), /PIN_GLIDE_SMOKE_RELEASE/);
  assert.throws(() => validateTarget({ release: 'latest' }), /X\.Y\.Z/);
});

test('the fixture plan stores only lowercase SHA-256 digests with normal lifetimes', () => {
  const plan = createFixturePlan({ runId: 'test-run', now: new Date('2026-10-20T00:00:00Z') });
  assert.equal(plan.accounts.length, 2);
  assert.deepEqual(plan.senders.slice(0, 3).map(sender => sender.accountID), [
    plan.accounts[0].id,
    plan.accounts[0].id,
    plan.accounts[1].id,
  ]);
  assert.deepEqual(
    plan.senders.map(sender => sender.id.slice(`smoke-test-run-sender-`.length)),
    [
      'a1', 'a2', 'foreign', 'scope', 'switch', 'order', 'recover', 'window',
      'aging', 'race', 'permanent', 'superseded', 'pair1', 'pair2', 'model',
      'model-expired', 'model-revoked',
    ],
  );

  const bootstrap = renderBootstrapSQL(plan);
  for (const sender of plan.senders) {
    assert.equal(sender.credentials.length, 3);
    for (const credential of sender.credentials) {
      assert.match(credential.digest, /^[a-f0-9]{64}$/);
      assert.equal(credential.digest, createHash('sha256').update(credential.token).digest('hex'));
      assert.equal(
        credential.expiresAt.getTime() - plan.now.getTime(),
        credentialLifetimeHours[credential.scope] * 60 * 60 * 1000,
      );
      assert(!bootstrap.includes(credential.token), 'raw credential entered fixture SQL');
      assert(bootstrap.includes(credential.digest));
    }
  }
  const superseded = plan.senders.find(sender => sender.id.endsWith('-superseded'));
  for (const credential of superseded.replacementCredentials) {
    assert(!bootstrap.includes(credential.token), 'replacement credential entered initial fixture SQL');
  }
});

test('fixture lifetimes match the adopted three-hour and seven-day contract', () => {
  // These literals protect the published contract from fixture-only drift.
  assert.deepEqual(credentialLifetimeHours, {
    user_session: 3,
    uploader: 3,
    refresh: 7 * 24,
  });
});

test('fixture Sender lookup follows the label rather than an ID suffix', () => {
  const sender = { label: 'scope', id: 'unrelated-sender-id' };
  assert.equal(fixtureSender({ senders: [sender] }, 'scope'), sender);
});

test('the HTTP peer refuses cross-origin paths before issuing a request and reports stable problems', async () => {
  let calls = 0;
  const peer = new SmokeHttp({
    origin: TARGET.httpOrigin,
    fetchImpl: async () => {
      calls++;
      return new Response(JSON.stringify({
        type: 'about:blank',
        title: 'Unauthorized',
        status: 401,
        code: 'credential_invalid',
        requestId: 'request-1',
      }), { status: 401, headers: { 'content-type': 'application/problem+json' } });
    },
  });

  await assert.rejects(() => peer.call({ path: 'https://example.invalid/v1/messages' }), /relative/);
  await assert.rejects(() => peer.call({ path: '//example.invalid/v1/messages' }), /relative/);
  await assert.rejects(() => peer.call({ path: '/\\example.invalid/v1/messages' }), /relative/);
  await assert.rejects(() => peer.call({ path: '/\t/example.invalid/v1/messages' }), /relative/);
  assert.equal(calls, 0);

  await assert.rejects(
    () => peer.call({ path: '/v1/messages/missing', token: 'secret' }),
    error => error instanceof SmokeProblem
      && error.status === 401
      && error.code === 'credential_invalid'
      && error.requestId === 'request-1',
  );
  assert.equal(calls, 1);
});

test('the HTTP peer can abandon a committed response so refresh recovery is exercised', async () => {
  let signal;
  const peer = new SmokeHttp({
    origin: TARGET.httpOrigin,
    fetchImpl: async (_url, options) => {
      signal = options.signal;
      return new Response('{"token":"must-not-be-read"}', {
        status: 200,
        headers: { 'x-request-id': 'request-dropped' },
      });
    },
  });

  const result = await peer.callDropped({
    method: 'POST',
    path: '/v1/auth/refresh',
    body: { refreshToken: 'secret', operationId: 'operation-1', attempt: 0 },
  });
  assert.equal(result.status, 200);
  assert.equal(result.requestId, 'request-dropped');
  assert.equal(signal.aborted, true);
});

test('the runner rejects an overridden target instead of ignoring it', () => {
  assert.throws(
    () => targetFromOptions({ release: '1.2.3' }, { PIN_GLIDE_SMOKE_PROJECT: 'pin-glide' }),
    /non-adopted/,
  );
  assert.equal(
    targetFromOptions({ release: '1.2.3' }, { PIN_GLIDE_SMOKE_PROJECT: 'pin-glide-dev' }).project,
    'pin-glide-dev',
  );
});

test('a read-only check is never reported as a completed acceptance run', () => {
  assert.equal(checkExitCode('check'), 2);
  assert.equal(checkExitCode('integration-ready'), 0);
});
