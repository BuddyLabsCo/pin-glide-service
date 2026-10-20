import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import test from 'node:test';

import { cronCommand, retention } from './lib/database.mjs';
import { acquireEnvironmentLock, RemoteEnvironment } from './lib/remote.mjs';
import { TARGET } from './lib/target.mjs';
import { verifyRecovery } from './runner.mjs';

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

test('a timed-out lock acquisition kills the SSH child instead of leaking its lock', async () => {
  const child = new EventEmitter();
  child.stdout = new PassThrough();
  child.stderr = new PassThrough();
  child.stdin = new PassThrough();
  let signal;
  child.kill = received => {
    signal = received;
  };
  await assert.rejects(
    () => acquireEnvironmentLock({
      target: TARGET,
      timeoutMs: 10,
      spawnImpl: () => child,
    }),
    /timed out acquiring/,
  );
  assert.equal(signal, 'SIGKILL');
  assert.equal(child.stdin.destroyed, true);
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
