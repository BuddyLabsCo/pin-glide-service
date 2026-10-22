import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import test from 'node:test';

import { createFixturePlan } from '../harness/fixtures.mjs';
import { SmokeBlocked, SmokeProblem } from '../harness/http.mjs';
import { SmokeSession } from '../harness/session.mjs';
import { runScenario, verifyCheckpoints } from '../harness/scenario.mjs';
import { verifyDiagnosticRedaction } from '../harness/diagnostics.mjs';
import { TARGET } from '../harness/target.mjs';

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
  sender.latestRefresh = { operationID: 'operation-authority', attempt: 0 };
  const checkpoint = { ...sender.latestRefresh };
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
  assert.deepEqual(sender.latestRefresh, checkpoint);
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

test('direct and abandoned Delivery requests register payload secrets before transmission', async () => {
  const plan = createFixturePlan({ runId: 'direct-delivery-secrets' });
  const session = new SmokeSession({
    target: TARGET,
    remote: {},
    http: {
      async call() {
        return { status: 202, requestId: 'request-direct', body: { results: [] } };
      },
      callAbandoned() {
        return {
          request: Promise.reject(new SmokeProblem(
            500,
            { code: 'delivery_unavailable', detail: 'abandoned-body' },
            'request-abandoned',
          )),
          abort() {},
          aborted() {
            return false;
          },
        };
      },
    },
    evidence: { addSecret() {} },
    plan,
  });
  await session.request({
    method: 'POST',
    path: '/v1/messages',
    body: {
      messageId: 'direct-message',
      items: [{ nonce: 'direct-nonce', ciphertext: 'direct-ciphertext' }],
    },
  });
  const abandoned = session.abandonedRequest({
    method: 'POST',
    path: '/v1/messages',
    body: {
      messageId: 'abandoned-message',
      items: [{ nonce: 'abandoned-nonce', ciphertext: 'abandoned-ciphertext' }],
    },
  });
  await assert.rejects(() => abandoned.request, /HTTP 500/);
  assert.deepEqual(session.errorBodies, [{
    code: 'delivery_unavailable',
    detail: 'abandoned-body',
  }]);
  assert.deepEqual([...session.secretValues()].sort(), [
    'abandoned-ciphertext',
    'abandoned-nonce',
    'direct-ciphertext',
    'direct-nonce',
  ]);
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
    requiredGroups: [],
    logsScanned: false,
    artifactsScanned: 0,
    errorBodiesScanned: 0,
    databaseRowsScanned: 0,
  });
  assert.equal(reads, 2);

  session.requestIds = ['request-1', 'request-2'];
  session.remote.sqlJSON = async () => [{ requestId: 'request-1', subject: null, method: 'POST' }];
  assert.deepEqual(await verifyDiagnosticRedaction(session, { settleMs: 0 }), {
    records: 1,
    scannedRequestIds: 1,
    missingRequestIds: 1,
    requiredGroups: [],
    logsScanned: false,
    artifactsScanned: 0,
    errorBodiesScanned: 0,
    databaseRowsScanned: 0,
  });

  session.requestIds = ['request-1', 'request-2'];
  session.remote.sqlJSON = async () => [{ requestId: 'request-1', subject: null, method: 'POST' }];
  await assert.rejects(
    () => verifyDiagnosticRedaction(session, {
      settleMs: 0,
      requiredGroups: {
        pairing: ['request-1'],
        model: ['request-2'],
      },
    }),
    error => error instanceof SmokeBlocked && /model/.test(error.message),
  );

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
