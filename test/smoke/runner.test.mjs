import assert from 'node:assert/strict';
import { createHash, randomBytes } from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import test from 'node:test';

import { createFixturePlan, escapeSQLString, renderBootstrapSQL } from './lib/fixtures.mjs';
import { SmokeValidationError } from './lib/errors.mjs';
import { modelConfiguration } from './lib/configuration.mjs';
import { Evidence } from './lib/evidence.mjs';
import { SmokeHttp, SmokeProblem } from './lib/http.mjs';
import { summarizeCompose, validateReleaseMetadata } from './lib/installation.mjs';
import { cronCommand, retention, validateDatabaseFacts } from './lib/database.mjs';
import { acquireEnvironmentLock, RemoteEnvironment } from './lib/remote.mjs';
import {
  contractBytes,
  parseReleaseSupport,
  publishedUpgradeOrigin,
  releaseSupport,
  validatePublishedRelease,
} from './lib/release.mjs';
import {
  lastCompletedSnapshot,
  listLocalSnapshots,
  recoverySnapshot,
  restoreSnapshotToDisposableDatabase,
  validateRestoredFacts,
  verifyLocalSnapshotDirectory,
  verifySnapshotDirectory,
} from './lib/restore.mjs';
import { checkExitCode, integration, verifyRecovery } from './runner.mjs';
import { TARGET, targetFromOptions, validateTarget } from './lib/target.mjs';
import { openWebSocket } from './lib/websocket.mjs';
import { EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import { SmokeSession } from './lib/session.mjs';

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
  assert.deepEqual(plan.senders.map(sender => sender.accountID), [
    plan.accounts[0].id,
    plan.accounts[0].id,
    plan.accounts[1].id,
  ]);

  const bootstrap = renderBootstrapSQL(plan);
  for (const sender of plan.senders) {
    assert.equal(sender.credentials.length, 3);
    for (const credential of sender.credentials) {
      assert.match(credential.digest, /^[a-f0-9]{64}$/);
      assert.equal(credential.digest, createHash('sha256').update(credential.token).digest('hex'));
      assert.equal(
        credential.expiresAt.getTime() - plan.now.getTime(),
        (credential.scope === 'refresh' ? 7 * 24 : 3) * 60 * 60 * 1000,
      );
      assert(!bootstrap.includes(credential.token), 'raw credential entered fixture SQL');
      assert(bootstrap.includes(credential.digest));
    }
  }
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

test('a read-only check without a completed snapshot exits blocked', () => {
  assert.equal(checkExitCode('check', { snapshot: null }), 2);
  assert.equal(checkExitCode('check', { snapshot: { name: 'snapshot.dump' } }), 0);
  assert.equal(checkExitCode('integration-ready', { snapshot: null }), 0);
});

function serverFrame(opcode, payload = Buffer.alloc(0)) {
  const body = Buffer.from(payload);
  const header = body.length < 126
    ? Buffer.from([0x80 | opcode, body.length])
    : Buffer.from([0x80 | opcode, 126, body.length >> 8, body.length & 0xff]);
  return Buffer.concat([header, body]);
}

test('the WebSocket peer presents bearer authority and answers service pings', async t => {
  let authorization = '';
  let protocol = '';
  let pong = '';
  const server = http.createServer();
  server.on('upgrade', (request, socket) => {
    authorization = request.headers.authorization || '';
    protocol = request.headers['sec-websocket-protocol'] || '';
    const accept = createHash('sha1')
      .update(`${request.headers['sec-websocket-key']}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`)
      .digest('base64');
    socket.write([
      'HTTP/1.1 101 Switching Protocols',
      'Upgrade: websocket',
      'Connection: Upgrade',
      `Sec-WebSocket-Accept: ${accept}`,
      'Sec-WebSocket-Protocol: pin-glide.v1',
      '',
      '',
    ].join('\r\n'));
    socket.write(serverFrame(1, Buffer.from('{"type":"pairing_state"}'))
      .toString('binary'), 'binary');
    socket.write(serverFrame(9, Buffer.from('probe')).toString('binary'), 'binary');
    socket.once('data', data => {
      const masked = (data[1] & 0x80) !== 0;
      const length = data[1] & 0x7f;
      const offset = 2 + (length === 126 ? 2 : 0) + (masked ? 4 : 0);
      const payload = Buffer.from(data.subarray(offset, offset + length));
      if (masked) {
        const mask = data.subarray(offset - 4, offset);
        for (let index = 0; index < payload.length; index++) payload[index] ^= mask[index % 4];
      }
      pong = payload.toString();
    });
    t.after(() => socket.destroy());
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const origin = `ws://127.0.0.1:${server.address().port}`;
  const localTarget = { ...TARGET, wsOrigin: origin };

  const socket = await openWebSocket({
    origin,
    path: '/v1/receiver/events',
    token: 'receiver-secret',
    target: localTarget,
  });
  t.after(() => socket.close());
  const message = await socket.next();
  assert.deepEqual(message.json, { type: 'pairing_state' });

  for (let attempt = 0; attempt < 50 && pong !== 'probe'; attempt++) {
    await new Promise(resolve => setTimeout(resolve, 10));
  }
  assert.equal(authorization, 'Bearer receiver-secret');
  assert.equal(protocol, 'pin-glide.v1');
  assert.equal(pong, 'probe');
});

function composeFixture(version = '1.2.3') {
  return {
    name: TARGET.project,
    services: {
      postgres: {
        image: 'ghcr.io/buddylabsco/pin-glide-postgres:18-1.6.8',
        environment: {
          POSTGRES_USER: 'pin_glide',
          POSTGRES_PASSWORD: 'secret',
          POSTGRES_DB: 'pin_glide',
        },
      },
      'db-tools': {
        image: `ghcr.io/buddylabsco/pin-glide-db-tools:${version}`,
        environment: {
          PIN_GLIDE_ENV: 'production',
          PIN_GLIDE_DATABASE_URL: 'postgres://pin_glide:secret@postgres:5432/pin_glide?sslmode=disable',
        },
      },
      api: {
        image: `ghcr.io/buddylabsco/pin-glide-api:${version}`,
        environment: {
          PIN_GLIDE_ENV: 'production',
          PIN_GLIDE_API_ADDRESS: ':8080',
          PIN_GLIDE_DATABASE_URL: 'postgres://pin_glide:secret@postgres:5432/pin_glide?sslmode=disable',
          PIN_GLIDE_BAILIAN_API_KEY: 'parent-secret',
          PIN_GLIDE_QQ_APP_ID: 'qq-app',
          PIN_GLIDE_QQ_APP_KEY: 'qq-key',
        },
        ports: [{ mode: 'ingress', target: 8080, published: '33489', protocol: 'tcp' }],
      },
    },
  };
}

test('the installation summary verifies exact project, port, database and release images without retaining secrets', () => {
  const summary = summarizeCompose(composeFixture(), TARGET, '1.2.3');
  assert.equal(summary.project, TARGET.project);
  assert.equal(summary.apiAddress, ':8080');
  assert.equal(summary.publishedPort, '33489');
  assert.equal(summary.database, 'pin_glide');
  assert.equal(summary.images.api, 'ghcr.io/buddylabsco/pin-glide-api:1.2.3');
  assert(!JSON.stringify(summary).includes('secret'));

  const wrongPort = composeFixture();
  wrongPort.services.api.ports[0].published = '8080';
  assert.throws(() => summarizeCompose(wrongPort, TARGET, '1.2.3'), /33489/);

  const wrongDatabase = composeFixture();
  wrongDatabase.services.api.environment.PIN_GLIDE_DATABASE_URL =
    'postgres://pin_glide:secret@postgres:5432/production?sslmode=disable';
  assert.throws(() => summarizeCompose(wrongDatabase, TARGET, '1.2.3'), /pin_glide/);

  const wrongImage = composeFixture();
  wrongImage.services.api.image = 'ghcr.io/buddylabsco/pin-glide-api:latest';
  assert.throws(() => summarizeCompose(wrongImage, TARGET, '1.2.3'), /immutable release image/);

  const unrelated = composeFixture();
  unrelated.services.debug = { image: 'example/debug:latest' };
  assert.throws(() => summarizeCompose(unrelated, TARGET, '1.2.3'), /unrelated service/);
});

test('release identity requires a published exact commit and both immutable contract assets', () => {
  const digest = createHash('sha256').update('openapi\n').digest('hex');
  const metadata = {
    tagName: 'v1.2.3',
    isDraft: false,
    isPrerelease: false,
    targetCommitish: 'main',
    assets: [
      { name: 'openapi.yaml', digest: `sha256:${digest}`, size: 8 },
      { name: 'asyncapi.yaml', digest: `sha256:${createHash('sha256').update('asyncapi\n').digest('hex')}`, size: 9 },
    ],
  };
  const validated = validateReleaseMetadata(metadata, {
    version: '1.2.3',
    sourceCommit: 'a'.repeat(40),
    assetBytes: {
      'openapi.yaml': Buffer.from('openapi\n'),
      'asyncapi.yaml': Buffer.from('asyncapi\n'),
    },
  });
  assert.equal(validated.assets['openapi.yaml'].digest, digest);

  assert.throws(() => validateReleaseMetadata({ ...metadata, isDraft: true }, {
    version: '1.2.3',
    sourceCommit: 'a'.repeat(40),
    assetBytes: {
      'openapi.yaml': Buffer.from('openapi\n'),
      'asyncapi.yaml': Buffer.from('asyncapi\n'),
    },
  }), /published/);

  assert.throws(() => validateReleaseMetadata(metadata, {
    version: '1.2.3',
    sourceCommit: 'a'.repeat(40),
    assetBytes: {
      'openapi.yaml': Buffer.from('tampered\n'),
      'asyncapi.yaml': Buffer.from('asyncapi\n'),
    },
  }), /openapi\.yaml/);
});

test('database facts require the selected release, approved model, complete ledgers and scheduled cleanup state', () => {
  const checksum = 'b'.repeat(64);
  const model = {
    id: 'default',
    workspaceId: 'ws-kea2u5v9va6wsdvh',
    model: 'qwen3.7-flash',
    enabled: true,
  };
  const facts = {
    database: 'pin_glide',
    release: '1.2.3',
    history: ['1.2.2', '1.2.3'],
    model: {
      id: 'default',
      workspaceId: 'ws-kea2u5v9va6wsdvh',
      model: 'qwen3.7-flash',
      enabled: true,
    },
    retention: [
      { schema: 'pin_glide', table: 'delivery_deliveries', days: 7, column: 'created_at' },
      { schema: 'pin_glide', table: 'identity_credentials', days: 7, column: 'expires_at' },
      { schema: 'pin_glide', table: 'identity_refresh_operations', days: 7, column: 'expires_at' },
      { schema: 'pin_glide', table: 'pairing_sessions', days: 7, column: 'expires_at' },
      { schema: 'pin_glide', table: 'service_diagnostics', days: 30, column: 'occurred_at' },
      { schema: 'release', table: 'release_schema_audits', days: 30, column: 'executed_at' },
      { schema: 'cron', table: 'job_run_details', days: 30, column: 'end_time' },
    ],
    cron: [{
      jobname: 'pin-glide-retention-cleanup',
      username: 'pin_glide',
      database: 'pin_glide',
      schedule: '0 19 * * *',
      active: true,
      command: 'SELECT pin_glide.pin_glide_fn_cleanup_expired_rows(now())',
    }],
    audits: {
      'release:apply': 0,
      'release:check': 0,
      'pin_glide:apply': 0,
      'pin_glide:check': 0,
    },
    dataScripts: [{ filename: 'issue_33_1_model_configuration.sql', checksum }],
    migrationScripts: [{ schema: 'pin_glide', filename: 'issue_77_1.sql', checksum }],
  };
  const expectedScripts = {
    data: { 'issue_33_1_model_configuration.sql': checksum },
    migrations: { 'pin_glide/issue_77_1.sql': checksum },
  };
  const summary = validateDatabaseFacts(facts, {
    release: '1.2.3',
    scripts: expectedScripts,
    upgradeOrigin: '1.2.2',
    model,
  });
  assert.equal(summary.release, '1.2.3');
  assert.equal(summary.model.workspaceId, 'ws-kea2u5v9va6wsdvh');

  const wrongModel = structuredClone(facts);
  wrongModel.model.model = 'another-model';
  assert.throws(() => validateDatabaseFacts(wrongModel, {
    release: '1.2.3',
    scripts: expectedScripts,
    upgradeOrigin: '1.2.2',
    model,
  }), /release Data Script/);

  const staleAudit = structuredClone(facts);
  staleAudit.audits['pin_glide:check'] = 1;
  assert.throws(() => validateDatabaseFacts(staleAudit, {
    release: '1.2.3',
    scripts: expectedScripts,
    upgradeOrigin: '1.2.2',
    model,
  }), /schema check/);

  const incomplete = structuredClone(facts);
  incomplete.dataScripts = [];
  assert.throws(() => validateDatabaseFacts(incomplete, {
    release: '1.2.3',
    scripts: expectedScripts,
    upgradeOrigin: '1.2.2',
    model,
  }), /data script.*missing/i);

  const unsupported = structuredClone(facts);
  unsupported.history = ['1.1.0', '1.2.3'];
  assert.throws(() => validateDatabaseFacts(unsupported, {
    release: '1.2.3',
    scripts: expectedScripts,
    upgradeOrigin: '1.2.2',
    model,
  }), /origin 1\.1\.0 is not supported/);

  const sameVersionRetry = structuredClone(facts);
  sameVersionRetry.history = ['1.2.2', '1.2.3', '1.2.3'];
  assert.equal(validateDatabaseFacts(sameVersionRetry, {
    release: '1.2.3',
    scripts: expectedScripts,
    upgradeOrigin: '1.2.2',
    model,
  }).release, '1.2.3');

  const fresh = structuredClone(facts);
  fresh.history = ['1.2.3'];
  assert.equal(validateDatabaseFacts(fresh, {
    release: '1.2.3',
    scripts: expectedScripts,
    upgradeOrigin: '1.2.2',
    model,
  }).installation, 'fresh');

  const unsupportedBefore = structuredClone(facts);
  unsupportedBefore.history = ['1.2.2', '1.2.3'];
  assert.throws(() => validateDatabaseFacts(unsupportedBefore, {
    release: '1.2.3',
    scripts: expectedScripts,
    upgradeOrigin: null,
    model,
  }), /expected fresh installation/);

  const legacyCronShape = structuredClone(facts);
  legacyCronShape.cron = legacyCronShape.cron[0];
  assert.throws(() => validateDatabaseFacts(legacyCronShape, {
    release: '1.2.3',
    scripts: expectedScripts,
    upgradeOrigin: '1.2.2',
    model,
  }), /not an array/);
});

test('the approved model configuration comes from the release Data Script', async () => {
  assert.deepEqual(await modelConfiguration(process.cwd()), {
    id: 'default',
    workspaceId: 'ws-kea2u5v9va6wsdvh',
    model: 'qwen3.7-flash',
    enabled: true,
  });
});

test('published upgrade origin is the newest stable release before the target', async () => {
  const origin = await publishedUpgradeOrigin({
    version: '1.2.3',
    releaseRepo: 'BuddyLabsCo/pin-glide-service',
    run: async () => ({
      stdout: JSON.stringify([
        { tagName: 'v1.2.4', isDraft: false, isPrerelease: false },
        { tagName: 'v1.2.2', isDraft: false, isPrerelease: false },
        { tagName: 'v1.2.1', isDraft: false, isPrerelease: false },
        { tagName: 'v1.1.9', isDraft: true, isPrerelease: false },
        { tagName: 'v1.1.8', isDraft: false, isPrerelease: true },
      ]),
    }),
  });
  assert.equal(origin, '1.2.2');

  const declared = await publishedUpgradeOrigin({
    version: '1.2.3',
    releaseRepo: 'BuddyLabsCo/pin-glide-service',
    allowedOrigins: ['1.2.1'],
    run: async () => ({
      stdout: JSON.stringify([
        { tagName: 'v1.2.2', isDraft: false, isPrerelease: false },
        { tagName: 'v1.2.1', isDraft: false, isPrerelease: false },
      ]),
    }),
  });
  assert.equal(declared, '1.2.1');
});

test('release support declares upgrade origins explicitly', async () => {
  const support = await releaseSupport(process.cwd());
  assert.deepEqual(support.origins['0.1.5'], []);
  assert.deepEqual(support.origins['0.1.6'], ['0.1.5']);
});

test('release support rejects malformed or invented upgrade origins', () => {
  assert.throws(() => parseReleaseSupport({ origins: { '1.2.3': ['1.2.2', '1.2.2'] } }), /duplicated/);
  assert.throws(() => parseReleaseSupport({ origins: { '1.2.3': ['1.3.0'] } }), /earlier/);
  assert.throws(() => parseReleaseSupport({ origins: [] }), /origins object/);
});

test('malformed release payloads fail validation while command failures stay blocked infrastructure', async () => {
  await assert.rejects(
    () => publishedUpgradeOrigin({
      version: '1.2.3',
      releaseRepo: 'BuddyLabsCo/pin-glide-service',
      run: async () => ({ stdout: 'not-json' }),
    }),
    error => error instanceof SmokeValidationError && /not valid JSON/.test(error.message),
  );
  await assert.rejects(
    () => publishedUpgradeOrigin({
      version: '1.2.3',
      releaseRepo: 'BuddyLabsCo/pin-glide-service',
      run: async () => { throw new Error('gh auth failed'); },
    }),
    error => !(error instanceof SmokeValidationError) && /gh auth failed/.test(error.message),
  );
});

test('smoke evidence redacts every raw credential before serialization', () => {
  const secret = 'smoke-run-uploader-secret';
  const evidence = new Evidence({ runId: 'test-run', secrets: [secret] });
  evidence.record({
    scenario: 'credential check',
    status: 'passed',
    requestId: 'request-1',
    details: {
      token: secret,
      reflected: `prefix-${secret}-suffix`,
      nested: [{ authorization: `Bearer ${secret}` }],
    },
  });
  const serialized = JSON.stringify(evidence.report());
  assert(!serialized.includes(secret));
  assert.match(serialized, /REDACTED/);
  assert.equal(evidence.report().scenarios[0].status, 'passed');
});

test('the environment lock is shared and refuses a second concurrent run', async () => {
  const child = () => {
    const process = new EventEmitter();
    process.stdout = new PassThrough();
    process.stderr = new PassThrough();
    process.stdin = new PassThrough();
    process.kill = () => {};
    return process;
  };
  const first = child();
  const second = child();
  const spawned = [];
  const spawnImpl = () => {
    const process = spawned.length === 0 ? first : second;
    spawned.push(process);
    return process;
  };
  const pendingFirst = acquireEnvironmentLock({ target: TARGET, spawnImpl });
  first.stdout.write('PIN_GLIDE_SMOKE_LOCK_ACQUIRED\n');
  const lock = await pendingFirst;
  const pendingSecond = acquireEnvironmentLock({ target: TARGET, spawnImpl });
  second.stderr.write('flock: failed to get lock\n');
  second.emit('exit', 1);
  await assert.rejects(() => pendingSecond, /holds the environment lock/);
  first.emit('exit', 0);
  await lock.release();
});

test('published release checks refuse a dirty worktree before contacting the target', async () => {
  const calls = [];
  await assert.rejects(
    () => validatePublishedRelease({
      target: { ...TARGET, release: '0.0.0' },
      root: process.cwd(),
      runContracts: false,
      run: async (command, args) => {
        calls.push([command, ...args]);
        if (command === 'git' && args[0] === 'rev-parse') return { stdout: `${'a'.repeat(40)}\n`, stderr: '' };
        if (command === 'git' && args[0] === 'status') return { stdout: ' M changed\n', stderr: '' };
        throw new Error(`unexpected command ${command}`);
      },
    }),
    /source worktree must be clean/,
  );
  assert(!calls.some(([command]) => command === 'gh'));
});

test('remote SQL is sent through stdin and never embedded in the SSH command', async () => {
  const calls = [];
  const remote = new RemoteEnvironment({
    target: TARGET,
    run: async (command, args, options) => {
      calls.push({ command, args, options });
      return { command, args, code: 0, stdout: '{}', stderr: '' };
    },
  });
  const secret = 'smoke-secret-token';
  await remote.sql('SELECT 1; -- token should stay in stdin');
  assert.equal(calls.length, 1);
  assert.equal(calls[0].command, 'ssh');
  assert(!calls[0].args.at(-1).includes('token should stay in stdin'));
  assert.equal(calls[0].options.input, 'SELECT 1; -- token should stay in stdin');
  assert(!secret.includes(calls[0].args.at(-1)));
});

test('the external-writer guard excludes only the snapshot dump connection', async () => {
  const remote = new RemoteEnvironment({ target: TARGET, run: async () => ({ stdout: '' }) });
  let query = '';
  remote.sqlJSON = async statement => {
    query = statement;
    return { connections: 0 };
  };
  await remote.assertNoExternalWriters();
  assert.match(query, /coalesce\(application_name, ''\) <> 'pg_dump'/);
});

test('the schema audit parses the JSON object returned by PostgreSQL', async () => {
  const remote = new RemoteEnvironment({
    target: TARGET,
    run: async () => ({ stdout: '', stderr: '' }),
  });
  remote.ssh = async () => ({ code: 0, stdout: '{"database":"pin_glide"}\n', stderr: '' });
  assert.deepEqual(await remote.schemaAudits(), { database: 'pin_glide' });
});

test('the external-writer guard accepts no clients and rejects a remaining writer', async () => {
  const remote = new RemoteEnvironment({ target: TARGET });
  remote.sqlJSON = async () => ({ connections: 0 });
  await remote.assertNoExternalWriters();
  remote.sqlJSON = async () => ({ connections: 1 });
  await assert.rejects(() => remote.assertNoExternalWriters(), /Another database writer/);
});

test('a stopped snapshot restarts the service when snapshotting fails', async () => {
  const actions = [];
  const remote = new RemoteEnvironment({ target: TARGET });
  remote.install = async (action, version) => {
    actions.push(`${action}:${version}`);
    if (action === '--stop-api') return { code: 0, stdout: '', stderr: '' };
    return { code: 0, stdout: '', stderr: '' };
  };
  remote.assertNoExternalWriters = async () => {};
  remote.databaseWriteStats = async () => ({ inserted: 0, updated: 0, deleted: 0 });
  remote.listSnapshots = async () => [{ name: 'pin-glide-snapshot-20261020T030000.000000001Z-1.dump' }];
  remote.makeSnapshot = async () => {
    throw new Error('snapshot failed');
  };
  await assert.rejects(() => remote.createStoppedSnapshot('1.2.3'), /snapshot failed/);
  assert.deepEqual(actions, ['--prep:1.2.3', '--stop-api:1.2.3', '--start-api:1.2.3']);
});

test('a failed stopped snapshot removes only its newly published archive', async () => {
  const old = 'pin-glide-snapshot-20261020T030000.000000001Z-1.dump';
  const created = 'pin-glide-snapshot-20261020T040000.000000002Z-2.dump';
  const actions = [];
  const removed = [];
  let snapshots = 0;
  let writes = 0;
  const remote = new RemoteEnvironment({ target: TARGET });
  remote.install = async (action, version) => {
    actions.push(`${action}:${version}`);
  };
  remote.assertNoExternalWriters = async () => {};
  remote.listSnapshots = async () => {
    snapshots++;
    return snapshots === 1 ? [{ name: old }] : [{ name: old }, { name: created }];
  };
  remote.databaseWriteStats = async () => (++writes === 1
    ? { inserted: 0, updated: 0, deleted: 0 }
    : { inserted: 1, updated: 0, deleted: 0 });
  remote.makeSnapshot = async () => ({ stdout: '' });
  remote.removeSnapshot = async name => {
    removed.push(name);
  };
  await assert.rejects(() => remote.createStoppedSnapshot('1.2.3'), /Database writes occurred/);
  assert.deepEqual(removed, [created]);
  assert.deepEqual(actions, ['--prep:1.2.3', '--stop-api:1.2.3', '--start-api:1.2.3']);
});

test('a successful stopped snapshot returns its own archive and never infers another run', async () => {
  const old = 'pin-glide-snapshot-20261020T030000.000000001Z-1.dump';
  const created = 'pin-glide-snapshot-20261020T040000.000000002Z-2.dump';
  const actions = [];
  let snapshots = 0;
  const remote = new RemoteEnvironment({ target: TARGET });
  remote.install = async (action, version) => {
    actions.push(`${action}:${version}`);
  };
  remote.assertNoExternalWriters = async () => {};
  remote.databaseWriteStats = async () => ({ inserted: 0, updated: 0, deleted: 0 });
  remote.listSnapshots = async () => (++snapshots === 1 ? [{ name: old }] : [{ name: old }, { name: created }]);
  remote.makeSnapshot = async () => ({ stdout: `${created}\n` });
  assert.equal(await remote.createStoppedSnapshot('1.2.3'), created);
  assert.deepEqual(actions, ['--prep:1.2.3', '--stop-api:1.2.3', '--start-api:1.2.3']);

  const missing = new RemoteEnvironment({ target: TARGET });
  missing.install = async () => {};
  missing.assertNoExternalWriters = async () => {};
  missing.databaseWriteStats = async () => ({ inserted: 0, updated: 0, deleted: 0 });
  missing.listSnapshots = async () => [{ name: old }];
  missing.makeSnapshot = async () => ({ stdout: '' });
  await assert.rejects(() => missing.createStoppedSnapshot('1.2.3'), /did not publish/);
});

test('recovery records snapshot and restore failures in their own scenarios', async () => {
  const before = [];
  await assert.rejects(() => verifyRecovery({
    checkResult: {
      remote: {
        target: TARGET,
        createStoppedSnapshot: async () => { throw new Error('snapshot command failed'); },
      },
      release: { tag: 'v1.2.3' },
      snapshot: null,
      http: {},
    },
    evidence: { record: entry => before.push(entry) },
    listSnapshots: async () => [],
    restore: async () => {},
  }), /snapshot command failed/);
  assert(before.some(entry => entry.scenario === 'stopped-writer container snapshot' && entry.status === 'failed'));
  assert(before.some(entry => entry.scenario === 'container-run development snapshot restore' && entry.status === 'blocked'));

  const after = [];
  const model = { id: 'default', workspaceId: 'workspace', model: 'qwen3.7-flash', enabled: true };
  const facts = {
    database: 'pin_glide',
    model,
    retention: [...retention].map(([identity, value]) => {
      const [schema, table] = identity.split('/');
      return { schema, table, days: value.days, column: value.column };
    }),
    cron: [{
      jobname: 'pin-glide-retention-cleanup',
      username: 'pin_glide',
      database: 'pin_glide',
      schedule: '0 19 * * *',
      active: true,
      command: cronCommand,
    }],
  };
  const checkResult = {
    remote: {
      target: TARGET,
      createStoppedSnapshot: async () => 'pin-glide-snapshot-20261020T040000.000000002Z-2.dump',
      schemaAudits: async () => facts,
    },
    release: { tag: 'v1.2.3', upgradeOrigin: '1.2.2' },
    snapshot: null,
    model,
    http: { call: async () => ({ body: { status: 'ok' }, requestId: 'request-1' }) },
  };
  let restoredSnapshot;
  await assert.rejects(() => verifyRecovery({
    checkResult,
    evidence: { record: entry => after.push(entry) },
    restore: async (remote, snapshot) => {
      restoredSnapshot = snapshot;
      throw new Error('restore failed');
    },
  }), /restore failed/);
  assert.equal(
    restoredSnapshot,
    `${TARGET.snapshotDir}/pin-glide-snapshot-20261020T040000.000000002Z-2.dump`,
  );
  assert(after.some(entry => entry.scenario === 'stopped-writer container snapshot' && entry.status === 'passed'));
  assert(after.some(entry => entry.scenario === 'container-run development snapshot restore' && entry.status === 'failed'));
});

test('fixture values escape SQL string boundaries', () => {
  assert.equal(
    escapeSQLString("'; DROP TABLE pin_glide.identity_accounts; --"),
    "'''; DROP TABLE pin_glide.identity_accounts; --'",
  );
});

test('published release checks require a clean worktree, matching commit and immutable assets', async () => {
  const head = 'a'.repeat(40);
  const contracts = await contractBytes(process.cwd());
  const openapi = contracts['openapi.yaml'];
  const asyncapi = contracts['asyncapi.yaml'];
  const calls = [];
  const run = async (command, args) => {
    calls.push([command, ...args]);
    if (command === 'git' && args[0] === 'rev-parse') return { stdout: `${head}\n`, stderr: '' };
    if (command === 'git' && args[0] === 'status') return { stdout: '', stderr: '' };
    if (command === 'gh' && args[0] === 'api') return { stdout: `${head}\n`, stderr: '' };
    if (command === 'gh' && args[0] === 'release' && args[1] === 'list') {
      return { stdout: JSON.stringify([{ tagName: 'v1.2.2', isDraft: false, isPrerelease: false }]), stderr: '' };
    }
    if (command === 'gh' && args[0] === 'release') {
      return {
        stdout: JSON.stringify({
          tagName: 'v1.2.3',
          isDraft: false,
          isPrerelease: false,
          assets: [
            { name: 'openapi.yaml', digest: `sha256:${createHash('sha256').update(openapi).digest('hex')}`, size: openapi.length },
            { name: 'asyncapi.yaml', digest: `sha256:${createHash('sha256').update(asyncapi).digest('hex')}`, size: asyncapi.length },
          ],
        }),
        stderr: '',
      };
    }
    return { stdout: '', stderr: '' };
  };
  const validated = await validatePublishedRelease({
    target: { ...TARGET, release: '1.2.3' },
    root: process.cwd(),
    run,
    runContracts: false,
  });
  assert.equal(validated.sourceCommit, head);
  assert.equal(validated.tag, 'v1.2.3');
  assert(calls.some(call => call[0] === 'git' && call[1] === 'status'));
});

test('snapshot discovery selects only completed tool-owned files and never an unrelated backup', () => {
  const entries = [
    { name: 'backup-0.1.5.dump', isFile: () => true },
    { name: 'unrelated.txt', isFile: () => true },
    { name: 'pin-glide-snapshot-20261020T030000.000000001Z-1.dump', isFile: () => true },
    { name: 'pin-glide-snapshot-20261019T030000.000000001Z-2.dump', isFile: () => true },
    { name: 'pin-glide-snapshot-invalid.dump', isFile: () => true },
    { name: 'pin-glide-snapshot-20261021T030000.000000001Z-3.dump', isFile: () => false },
  ];
  assert.equal(
    lastCompletedSnapshot(entries, '/mnt/share/workspace/snapshots/pin-glide', 'local-server'),
    '/mnt/share/workspace/snapshots/pin-glide/pin-glide-snapshot-20261020T030000.000000001Z-1.dump',
  );
  assert.throws(
    () => lastCompletedSnapshot([entries[0]], '/mnt/share/workspace/snapshots/pin-glide', 'local-server'),
    /No completed snapshot/,
  );
});

test('snapshot directory verification accepts the resolved adopted path and rejects another', async () => {
  const calls = [];
  const remote = {
    target: TARGET,
    ssh: async (script, options) => {
      calls.push({ script, options });
      return { stdout: `${TARGET.snapshotDir}\n`, stderr: '' };
    },
  };
  await verifySnapshotDirectory(remote);
  assert.equal(calls.length, 1);
  assert.match(calls[0].script, /sudo -n test -w '\/mnt\/share\/workspace\/snapshots\/pin-glide'/);

  remote.ssh = async () => ({ stdout: '/some/other/path\n', stderr: '' });
  await assert.rejects(() => verifySnapshotDirectory(remote), /must resolve/);
});

test('local snapshot paths are exact and list only completed dumps', async t => {
  const directory = fs.mkdtempSync(`${os.tmpdir()}/pin-glide-smoke-snapshot-`);
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  fs.writeFileSync(`${directory}/unrelated.txt`, 'not a dump');
  fs.writeFileSync(`${directory}/pin-glide-snapshot-20261020T030000.000000001Z-1.dump`, 'dump');
  assert.equal(await verifyLocalSnapshotDirectory(directory), directory);
  assert.deepEqual(await listLocalSnapshots(directory), [
    'pin-glide-snapshot-20261020T030000.000000001Z-1.dump',
  ]);
  await assert.rejects(() => verifyLocalSnapshotDirectory(`${directory}/missing`), /unavailable/);
});

test('recovery requires the server and workstation to expose the same completed snapshot', async t => {
  const directory = fs.mkdtempSync(`${os.tmpdir()}/pin-glide-smoke-shared-`);
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const name = 'pin-glide-snapshot-20261020T030000.000000001Z-1.dump';
  fs.writeFileSync(`${directory}/${name}`, 'PGDMP test archive');
  const target = { ...TARGET, snapshotDir: directory };
  const remote = {
    target,
    ssh: async script => ({
      stdout: script.includes('find ')
        ? `${name} 4\n`
        : `${directory}\n`,
      stderr: '',
    }),
  };
  const snapshot = await recoverySnapshot(remote);
  assert.equal(snapshot.name, name);
  assert.equal(snapshot.remotePath, `${directory}/${name}`);
});

test('integration recovery can start from an empty shared snapshot directory', async t => {
  const directory = fs.mkdtempSync(`${os.tmpdir()}/pin-glide-smoke-empty-`);
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const target = { ...TARGET, snapshotDir: directory };
  const remote = {
    target,
    ssh: async script => ({
      stdout: script.includes('find ') ? '' : `${directory}\n`,
      stderr: '',
    }),
  };
  assert.equal(await recoverySnapshot(remote, { allowMissing: true }), null);
});

test('disposable restore validates restored facts and drops its database even on failure', async () => {
  const calls = [];
  const facts = {
    database: 'smoke_restore_placeholder',
    release: '1.2.3',
    history: ['1.2.2', '1.2.3'],
    model: { id: 'default', workspaceId: 'workspace', model: 'qwen3.7-flash', enabled: true },
    retention: [...retention].map(([identity, value]) => {
      const [schema, table] = identity.split('/');
      return { schema, table, days: value.days, column: value.column };
    }),
    cron: [{
      jobname: 'pin-glide-retention-cleanup',
      username: 'pin_glide',
      database: 'pin_glide',
      schedule: '0 19 * * *',
      active: false,
      command: cronCommand,
    }],
    pgCronVersion: '1.6',
    runningCronJobs: 0,
    otherConnections: 0,
  };
  let restoredDatabase;
  let restoredSnapshot;
  let listedSnapshot;
  const remote = {
    async sqlOn(database, statement) {
      calls.push(['sql', database, statement]);
    },
    async restoreList(snapshot) {
      calls.push(['list']);
      listedSnapshot = snapshot;
      return { stdout: '1; 1 0 TABLE public thing\n' };
    },
    async restoreDump(database, snapshot) {
      calls.push(['dump']);
      restoredDatabase = database;
      restoredSnapshot = snapshot;
    },
    async sqlJSONOn(database) {
      calls.push(['facts']);
      facts.database = database;
      return facts;
    },
  };
  const restored = await restoreSnapshotToDisposableDatabase(remote, '/snapshots/test.dump', {
    release: '1.2.3',
    upgradeOrigin: '1.2.2',
    model: facts.model,
  });
  assert.equal(restored.release, '1.2.3');
  assert.deepEqual(calls.filter(call => call[0] === 'sql').map(call => call[1]), ['postgres', 'postgres']);
  assert.equal(calls.filter(call => call[0] === 'list').length, 1);
  assert.equal(calls.filter(call => call[0] === 'dump').length, 1);
  assert.equal(listedSnapshot, '/snapshots/test.dump');
  assert.equal(restoredSnapshot, '/snapshots/test.dump');
  assert.match(restoredDatabase, /^smoke_restore_[a-f0-9]{16}$/);

  const failedCalls = [];
  await assert.rejects(() => restoreSnapshotToDisposableDatabase({
    async sqlOn(database, statement) {
      failedCalls.push(['sql', database, statement]);
    },
    async restoreList() {
      return { stdout: '1; 1 0 TABLE public thing\n' };
    },
    async restoreDump() {
      throw new Error('restore failed');
    },
    async sqlJSONOn() {
      throw new Error('facts should not be queried');
    },
  }, '/snapshots/test.dump', { release: '1.2.3' }), /restore failed/);
  assert.deepEqual(failedCalls.filter(call => call[0] === 'sql').map(call => call[1]), ['postgres', 'postgres']);
});

test('restored facts reject incomplete extension or scheduled-work state', () => {
  const facts = {
    database: 'smoke_restore_1',
    release: '1.2.3',
    history: ['1.2.3'],
    model: { id: 'default', workspaceId: 'workspace', model: 'qwen3.7-flash', enabled: true },
    retentionPolicies: 7,
    retention: [...retention].map(([identity, value]) => {
      const [schema, table] = identity.split('/');
      return { schema, table, days: value.days, column: value.column };
    }),
    cron: [{
      jobname: 'pin-glide-retention-cleanup',
      username: 'pin_glide',
      database: 'pin_glide',
      schedule: '0 19 * * *',
      active: false,
      command: cronCommand,
    }],
    pgCronVersion: '1.6',
    runningCronJobs: 0,
    otherConnections: 0,
  };
  const model = { id: 'default', workspaceId: 'workspace', model: 'qwen3.7-flash', enabled: true };
  assert.equal(validateRestoredFacts(facts, { database: 'smoke_restore_1', release: '1.2.3', model }).pgCronVersion, '1.6');
  assert.throws(() => validateRestoredFacts({ ...facts, pgCronVersion: '1.5' }, {
    database: 'smoke_restore_1',
    release: '1.2.3',
    model,
  }), /pg_cron 1\.6/);
  assert.throws(() => validateRestoredFacts({ ...facts, cron: [{ ...facts.cron[0], active: true }] }, {
    database: 'smoke_restore_1',
    release: '1.2.3',
    model,
  }), /wrong active state/);
  assert.throws(() => validateRestoredFacts({ ...facts, model: { ...model, enabled: false } }, {
    database: 'smoke_restore_1',
    release: '1.2.3',
    model,
  }), /release Data Script/);
  assert.throws(() => validateRestoredFacts({ ...facts, retention: facts.retention.slice(1) }, {
    database: 'smoke_restore_1',
    release: '1.2.3',
    model,
  }), /Retention policies/);
  assert.throws(() => validateRestoredFacts({ ...facts, cron: [{ ...facts.cron[0], command: 'SELECT 1' }] }, {
    database: 'smoke_restore_1',
    release: '1.2.3',
    model,
  }), /Scheduled job .* command/);
  assert.throws(() => validateRestoredFacts({
    ...facts,
    cron: [...facts.cron, { ...facts.cron[0], jobname: 'unexpected-job' }],
  }, {
    database: 'smoke_restore_1',
    release: '1.2.3',
    model,
  }), /Scheduled job inventory/);
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
