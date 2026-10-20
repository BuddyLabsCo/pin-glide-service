import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import test from 'node:test';

import { cronCommand, retention } from './lib/database.mjs';
import {
  lastCompletedSnapshot,
  listLocalSnapshots,
  recoverySnapshot,
  restoreSnapshotToDisposableDatabase,
  validateRestoredFacts,
  verifyLocalSnapshotDirectory,
  verifySnapshotDirectory,
} from './lib/restore.mjs';
import { TARGET } from './lib/target.mjs';

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
