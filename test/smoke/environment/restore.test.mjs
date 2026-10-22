import assert from 'node:assert/strict';
import test from 'node:test';

import { cronJobs, retention, retentionJobName } from './database.mjs';
import {
  restoreFactsInDisposablePostgres,
  restoreSnapshotToDisposableDatabase,
  validateRestoredFacts,
} from './restore.mjs';
import { TARGET } from '../harness/target.mjs';

test('disposable restore validates restored facts and propagates failure', async () => {
  const expectedJob = cronJobs.get(retentionJobName);
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
      jobname: retentionJobName,
      username: expectedJob.username,
      database: expectedJob.database,
      schedule: expectedJob.schedule,
      active: false,
      command: expectedJob.command,
    }],
    cronTimezone: 'GMT',
    pgCronVersion: '1.6',
    runningCronJobs: 0,
    otherConnections: 0,
  };
  let restoreInput;
  const restoreFacts = async (_remote, input) => {
    restoreInput = input;
    return { ...facts, database: TARGET.database };
  };
  const restored = await restoreSnapshotToDisposableDatabase({ target: TARGET }, '/snapshots/test.dump', {
    release: '1.2.3',
    upgradeOrigin: '1.2.2',
    model: facts.model,
    postgresImage: 'ghcr.io/example/postgres:18',
    restoreFacts,
  });
  assert.equal(restored.release, '1.2.3');
  assert.equal(restoreInput.snapshot, '/snapshots/test.dump');
  assert.equal(restoreInput.image, 'ghcr.io/example/postgres:18');
  assert.match(restoreInput.name, /^smoke_restore_[a-f0-9]{16}$/);

  await assert.rejects(() => restoreSnapshotToDisposableDatabase({ target: TARGET }, '/snapshots/test.dump', {
    release: '1.2.3',
    postgresImage: 'ghcr.io/example/postgres:18',
    restoreFacts: async () => {
      throw new Error('restore failed');
    },
  }), /restore failed/);
});

test('disposable PostgreSQL restore returns restored facts and propagates failures', async () => {
  const name = 'smoke_restore_0123456789abcdef';
  const snapshot = `${TARGET.snapshotDir}/pin-glide-snapshot-20261020T030000.000000001Z-1.dump`;
  const phases = [];
  const remote = {
    target: TARGET,
    async exec(script, options) {
      phases.push(options.phase);
      if (script.includes(' psql ')) {
        return { stdout: '{"database":"restored"}\n', stderr: '' };
      }
      return { stdout: '', stderr: '' };
    },
  };
  assert.deepEqual(await restoreFactsInDisposablePostgres(remote, {
    name,
    snapshot,
    image: 'ghcr.io/example/postgres:18',
    factsQuery: 'SELECT 1;',
  }), { database: 'restored' });
  assert.deepEqual(phases, ['create', 'ready', 'restore', 'facts', 'cleanup']);

  phases.length = 0;
  await assert.rejects(() => restoreFactsInDisposablePostgres({
    target: TARGET,
    async exec(script, options) {
      phases.push(options.phase);
      if (script.includes('pg_restore')) throw new Error('restore failed');
      return { stdout: '', stderr: '' };
    },
  }, {
    name,
    snapshot,
    image: 'ghcr.io/example/postgres:18',
    factsQuery: 'SELECT 1;',
  }), /restore failed/);
  assert.deepEqual(phases, ['create', 'ready', 'restore', 'cleanup']);
});

test('restored facts reject incomplete extension or scheduled-work state', () => {
  const expectedJob = cronJobs.get(retentionJobName);
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
      jobname: retentionJobName,
      username: expectedJob.username,
      database: expectedJob.database,
      schedule: expectedJob.schedule,
      active: false,
      command: expectedJob.command,
    }],
    cronTimezone: 'GMT',
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

  const probe = {
    jobname: 'smoke-run-retention-drain-abc123',
    username: 'pin_glide',
    database: 'pin_glide',
    schedule: '0 0 1 1 *',
    active: false,
    command: 'DO $$ BEGIN PERFORM pin_glide.pin_glide_fn_cleanup_expired_rows(\'2000-01-01\'); PERFORM pg_sleep(8); END $$;',
  };
  const restored = validateRestoredFacts({ ...facts, cron: [...facts.cron, probe] }, {
    database: 'smoke_restore_1',
    release: '1.2.3',
    model,
    retentionDrainProbe: { name: probe.jobname, schedule: probe.schedule },
  });
  assert.deepEqual(restored.retentionDrainProbe, {
    name: probe.jobname,
    active: false,
    schedule: '0 0 1 1 *',
  });
});
