import assert from 'node:assert/strict';
import test from 'node:test';

import { cronCommand, retention } from './lib/database.mjs';
import { verifyRecovery } from './lib/recovery.mjs';
import { TARGET } from './lib/target.mjs';

function drainProbeFixture() {
  let inspections = 0;
  let cleanups = 0;
  return {
    probeName: 'held-state-run-retention-drain-abc123',
    jobID: 42,
    runID: 11,
    async inspect() {
      inspections++;
      return {
        jobExists: true,
        schedule: '0 0 1 1 *',
        active: inspections > 1,
        runID: 11,
        status: 'succeeded',
        endTime: '2026-10-20T04:00:00.000Z',
        cronBackends: 0,
      };
    },
    async cleanup() {
      cleanups++;
    },
    get inspections() {
      return inspections;
    },
    get cleanups() {
      return cleanups;
    },
  };
}

test('recovery rejects scheduled work that becomes active while held', async () => {
  let stateReads = 0;
  const recorded = [];
  const drainProbe = drainProbeFixture();
  await assert.rejects(
    () => verifyRecovery({
      checkResult: {
        remote: {
          target: TARGET,
          createStoppedSnapshot: async (_version, { afterStop }) => {
            await afterStop();
            throw new Error('snapshot must not run');
          },
          scheduledWorkState: async () => {
            stateReads++;
            return stateReads === 1
              ? { activeJobs: [], runningRuns: 0, cronBackends: 0, maxRunID: 7 }
              : { activeJobs: ['new-job'], runningRuns: 0, cronBackends: 0, maxRunID: 7 };
          },
        },
        release: { tag: 'v1.2.3' },
        snapshot: null,
        http: {},
      },
      evidence: {
        runId: 'held-state-run',
        record(entry) {
          recorded.push(entry);
        },
      },
      startDrainProbe: async () => drainProbe,
      probeWriter: async () => ({ blocked: true }),
      restore: async () => {},
    }),
    /became active/,
  );
  assert(recorded.some(entry => entry.scenario === 'stopped-writer container snapshot' && entry.status === 'failed'));
  assert.equal(drainProbe.cleanups, 1);
});

test('recovery records snapshot and restore failures in their own scenarios', async () => {
  const before = [];
  const failedBeforeStop = drainProbeFixture();
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
    startDrainProbe: async () => failedBeforeStop,
    restore: async () => {},
  }), /snapshot command failed/);
  assert(before.some(entry => entry.scenario === 'stopped-writer container snapshot' && entry.status === 'failed'));
  assert(before.some(entry => entry.scenario === 'container-run development snapshot restore' && entry.status === 'blocked'));
  assert.equal(failedBeforeStop.cleanups, 1);

  const after = [];
  const drainProbe = drainProbeFixture();
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
    runningCronJobs: 0,
    cronBackends: 0,
  };
  const checkResult = {
    remote: {
      target: TARGET,
      createStoppedSnapshot: async (_version, { afterStop }) => {
        await afterStop();
        return 'pin-glide-snapshot-20261020T040000.000000002Z-2.dump';
      },
      scheduledWorkState: async () => ({
        activeJobs: [],
        runningRuns: 0,
        cronBackends: 0,
        maxRunID: 7,
      }),
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
    startDrainProbe: async () => drainProbe,
    probeWriter: async () => ({ blocked: true }),
    restore: async (remote, snapshot) => {
      restoredSnapshot = snapshot;
      throw new Error('restore failed');
    },
  }), /restore failed/);
  assert.equal(
    restoredSnapshot,
    `${TARGET.snapshotDir}/pin-glide-snapshot-20261020T040000.000000002Z-2.dump`,
  );
  const backup = after.find(entry => entry.scenario === 'stopped-writer container snapshot');
  assert.equal(backup?.status, 'passed');
  assert.equal(backup?.details?.heldScheduledWork?.unknownWriterBlocked, true);
  assert.equal(backup?.details?.retentionDrainProbe?.observedRunning, true);
  assert.equal(backup?.details?.retentionDrainProbe?.cleanedUp, true);
  assert.equal(drainProbe.inspections, 2);
  assert.equal(drainProbe.cleanups, 1);
  assert(after.some(entry => entry.scenario === 'container-run development snapshot restore' && entry.status === 'failed'));
});
