import { randomBytes } from 'node:crypto';
import path from 'node:path';

import { validateConfiguration } from './database.mjs';
import {
  retentionDrainProbeSchedule,
  startRunningRetentionProbe,
  unknownWriterBlocksSnapshot,
} from './retention_runtime.mjs';
import { restoreSnapshotToDisposableDatabase } from './restore.mjs';

export const recoveryRestoreScenario = 'container-run development snapshot restore';

function errorMessage(error) {
  return error instanceof AggregateError
    ? error.errors.map(errorMessage).join('; ')
    : error.message;
}

function assertDrainProbe(state, { restored }) {
  if (!state.jobExists || state.schedule !== retentionDrainProbeSchedule) {
    throw new Error('The run-owned retention drain job was not preserved through the snapshot');
  }
  if (restored && !state.active) {
    throw new Error('The run-owned retention drain job schedule was not restored');
  }
  if (state.status !== 'succeeded' || !state.endTime || Number(state.cronBackends) !== 0) {
    throw new Error('The run-owned retention SQL had not drained before the snapshot');
  }
  return state;
}

export async function verifyRecovery({
  checkResult,
  evidence,
  restore = restoreSnapshotToDisposableDatabase,
  probeWriter = unknownWriterBlocksSnapshot,
  startDrainProbe = startRunningRetentionProbe,
}) {
  const backupScenario = 'stopped-writer container snapshot';
  let artifact = checkResult.snapshot;
  let backupComplete = false;
  let failure;
  let drainProbe;
  let probeCleaned = false;
  let newSnapshot;
  let heldScheduledWork;
  let heldDrainProbe;
  let restoredDrainProbe;
  try {
    drainProbe = await startDrainProbe(checkResult.remote, {
      runId: evidence.runId || `smoke-${randomBytes(6).toString('hex')}`,
    });
    newSnapshot = await checkResult.remote.createStoppedSnapshot(
      checkResult.release.tag.slice(1),
      {
        afterStop: async () => {
          const held = await checkResult.remote.scheduledWorkState();
          if ((held.activeJobs || []).length > 0) {
            throw new Error('Scheduled jobs remained active after the API stopped');
          }
          if (Number(held.runningRuns) !== 0 || Number(held.cronBackends) !== 0) {
            throw new Error('Scheduled SQL had not drained after the API stopped');
          }
          heldDrainProbe = assertDrainProbe(await drainProbe.inspect(), { restored: false });
          const writerApplication = `smoke-${evidence.runId || randomBytes(6).toString('hex')}-writer`;
          const writer = await probeWriter(checkResult.remote, writerApplication);
          if (!writer?.blocked) throw new Error('The stopped recovery baseline accepted an external writer');
          await new Promise(resolve => setTimeout(resolve, 250));
          const later = await checkResult.remote.scheduledWorkState();
          if ((later.activeJobs || []).length > 0) {
            throw new Error('Scheduled jobs became active while the recovery baseline was held');
          }
          if (Number(later.runningRuns) !== 0 || Number(later.cronBackends) !== 0) {
            throw new Error('Scheduled SQL started while the recovery baseline was held');
          }
          if (Number(later.maxRunID) !== Number(held.maxRunID)) {
            throw new Error('New scheduled work started while the recovery baseline was held');
          }
          heldScheduledWork = {
            activeJobs: held.activeJobs,
            runningRuns: Number(held.runningRuns),
            cronBackends: Number(held.cronBackends),
            newRunsWhileHeld: Number(later.maxRunID) - Number(held.maxRunID),
            unknownWriterBlocked: writer.blocked,
          };
        },
      },
    );
    restoredDrainProbe = assertDrainProbe(await drainProbe.inspect(), { restored: true });
    await drainProbe.cleanup();
    probeCleaned = true;
    const health = await checkResult.http.call({ path: '/health' });
    if (health.body?.status !== 'ok') throw new Error('API did not recover after the stopped snapshot');
    const liveFacts = await checkResult.remote.schemaAudits();
    const liveConfiguration = validateConfiguration(liveFacts, checkResult.model);
    if (Number(liveFacts.runningCronJobs) !== 0 || Number(liveFacts.cronBackends) !== 0) {
      throw new Error('Scheduled SQL was still running after the recovery snapshot completed');
    }
    evidence.record({
      scenario: backupScenario,
      status: 'passed',
      details: {
        snapshot: newSnapshot,
        writersStopped: true,
        healthRequestId: health.requestId,
        schedule: liveConfiguration.cron,
        heldScheduledWork,
        retentionDrainProbe: {
          name: drainProbe.probeName,
          jobID: drainProbe.jobID,
          runID: drainProbe.runID,
          observedRunning: true,
          held: heldDrainProbe,
          restoredAfterSnapshot: restoredDrainProbe,
          cleanedUp: true,
        },
        runningCronJobs: Number(liveFacts.runningCronJobs),
        cronBackends: Number(liveFacts.cronBackends),
      },
    });
    backupComplete = true;
    artifact = {
      name: newSnapshot,
      remotePath: path.posix.join(checkResult.remote.target.snapshotDir, newSnapshot),
      localPath: null,
    };
    const restored = await restore(
      checkResult.remote,
      artifact.remotePath,
      {
        release: checkResult.release.tag.slice(1),
        origins: checkResult.release.origins,
        upgradeOrigin: checkResult.release.upgradeOrigin,
        model: checkResult.model,
      },
    );
    evidence.record({
      scenario: recoveryRestoreScenario,
      status: 'passed',
      details: {
        snapshot: artifact.name,
        ...restored,
      },
    });
  } catch (error) {
    failure = error;
  } finally {
    if (drainProbe && !probeCleaned) {
      try {
        await drainProbe.cleanup();
        probeCleaned = true;
      } catch (cleanupError) {
        failure = failure
          ? new AggregateError([failure, cleanupError], 'Recovery and retention probe cleanup both failed')
          : cleanupError;
      }
    }
  }
  if (failure) {
    if (backupComplete) {
      evidence.record({
        scenario: recoveryRestoreScenario,
        status: 'failed',
        details: { snapshot: artifact?.name || null, reason: errorMessage(failure) },
      });
    } else {
      evidence.record({
        scenario: backupScenario,
        status: 'failed',
        details: {
          snapshot: newSnapshot || null,
          retentionDrainProbeCleanedUp: probeCleaned,
          reason: errorMessage(failure),
        },
      });
      evidence.record({
        scenario: recoveryRestoreScenario,
        status: 'blocked',
        details: { reason: 'the stopped snapshot did not complete' },
      });
    }
    throw failure;
  }
}
