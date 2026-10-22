import { createHash, randomBytes } from 'node:crypto';

import { withFailureJoiningCleanup } from '../support/cleanup.mjs';
import {
  renderRetentionCleanupSQL,
  runOwnedPrivacyQuery,
} from './checks.mjs';
import {
  renderRetentionAgingSQL,
} from './fixtures.mjs';
import { runScenario } from '../harness/scenario.mjs';
import { verifyDiagnosticRedaction } from '../harness/diagnostics.mjs';
import { openReceiver } from '../delivery/support.mjs';
import { pairReceiver } from '../pairing/helpers.mjs';
import { runDeterministicTests } from '../harness/deterministic.mjs';
import {
  assertRetentionCounts,
  databaseNow,
  retentionState,
  runScheduledCleanupProbe,
  stableRetentionJob,
} from './runtime.mjs';
import {
  assertDurableBaseline,
  assertDurableState,
  boundaryCleanup,
  configurationDeletionProbe,
  deliveryLifetime,
  interruptedCleanup,
  loadArtifacts,
  proveSlidingFamily,
  restartWithoutPayload,
} from './operations.mjs';

export {
  assertRetentionCounts,
  jsonFromOutput,
  retentionProbeCleanupSQL,
  retentionProbeHistoryCleanupSQL,
  retentionDrainProbeCommand,
  retentionProbeCommand,
  runScheduledCleanupProbe,
  stableRetentionJob,
  startRunningRetentionProbe,
  terminateApplicationBackendsSQL,
  unknownWriterBlocksSnapshot,
} from './runtime.mjs';

export const retentionDeterministicTestReferences = [
  {
    test: 'TestScheduledCleanupJobConvergesThroughCreateUpdateActiveAndRetirement',
    package: './test/integration/privacy',
    level: 'integration',
    covers: ['job-lifecycle'],
    observes: 'immutable Data Scripts create, update, activate and retire the stable cleanup job',
  },
  {
    test: 'TestScheduledCleanupPrivilegesAreConstrainedToTheReleaseOwner',
    package: './test/integration/privacy',
    level: 'integration',
    covers: ['job-privileges'],
    observes: 'unrelated roles cannot rewrite cleanup targets or run cleanup',
  },
  {
    test: 'TestScheduledCleanupPreservesLiveFamilyAndGuardClocks',
    package: './test/integration/privacy',
    level: 'integration',
    covers: ['live-family-clock'],
    observes: 'cleanup at a controlled reference time preserves a live family and its ordering guard',
  },
  {
    test: 'TestScheduledCleanupCoversNullableCronHistoryAndRetriesWithoutCheckpoints',
    package: './test/integration/privacy',
    level: 'integration',
    covers: ['nullable-history', 'retry-without-checkpoint'],
    observes: 'NULL cron end times survive and a rolled-back run retries without progress state',
  },
];
const zeroAging = {
  'aging.deliveryOld': 0,
  'aging.credentialOld': 0,
  'aging.operationOld': 0,
  'aging.sessionOld': 0,
  'aging.diagnosticOld': 0,
  'aging.auditOld': 0,
  'aging.cronOld': 0,
  'aging.deliveryFresh': 1,
  'aging.credentialFresh': 1,
  'aging.operationFresh': 1,
  'aging.sessionFresh': 1,
  'aging.diagnosticFresh': 1,
  'aging.auditFresh': 1,
  'aging.cronFresh': 1,
  'aging.cronRunning': 1,
};

export async function runRetentionScenarios(session, { root } = {}) {
  const plan = session.plan;
  const pairing = await pairReceiver(session, 'pair2');
  const socket = await openReceiver(session, pairing.receiverToken);
  const senderID = session.sender('pair2').id;
  const fixture = {
    senderID,
    receiverID: pairing.receiverID,
    pairingID: pairing.pairingID,
    liveToken: `smoke-${plan.runId}-retention-live-${randomBytes(24).toString('hex')}`,
  };
  fixture.liveTokenDigest = createHash('sha256').update(fixture.liveToken).digest('hex');
  fixture.liveGuardDigest = createHash('sha256')
    .update(`${fixture.liveToken}:guard`)
    .digest('hex');
  session.addSecret(fixture.liveToken);
  const scenarios = [];
  return withFailureJoiningCleanup(
    async () => {
      const job = await stableRetentionJob(session);
      fixture.jobID = Number(job.jobID);
      fixture.referenceTime = await databaseNow(session);
      const before = await retentionState(session, plan, fixture);
      assertDurableBaseline(before);

      await session.remote.sql(renderRetentionAgingSQL(plan, fixture));
      scenarios.push(await runScenario(
        session,
      'interrupted retention cleanup rolls back atomically',
        () => interruptedCleanup(session, plan, fixture),
      ));
      const probe = await runScheduledCleanupProbe(session);
      const afterProbe = await retentionState(session, plan, fixture);
      assertRetentionCounts(afterProbe, zeroAging);
      scenarios.push({
        name: 'pg_cron executes configuration-driven retention',
        status: 'passed',
        details: probe,
      });
      session.evidence.record({
        scenario: 'pg_cron executes configuration-driven retention',
        status: 'passed',
        details: {
          job,
          probe,
          deletedOutsideWindows: afterProbe.aging,
          nullableCronEndTimeRetained: afterProbe.aging.cronRunning,
        },
      });

      const boundary = await boundaryCleanup(session, plan, fixture);
      const family = await proveSlidingFamily(session, plan, fixture);
      const afterBoundary = await retentionState(session, plan, fixture);
      assertDurableState(before, afterBoundary);
      scenarios.push({
        name: 'cutoff clocks preserve live credentials and ordering guards',
        status: 'passed',
        details: {
          removedPastCutoff: boundary.removed,
          cutoffRowsRetained: afterBoundary.boundary,
          slidingFamily: family,
        },
      });
      session.evidence.record({
        scenario: 'cutoff clocks preserve live credentials and ordering guards',
        status: 'passed',
        details: {
          removedPastCutoff: boundary.removed,
          cutoffRowsRetained: afterBoundary.boundary,
          slidingFamily: family,
        },
      });

      scenarios.push(await runScenario(
        session,
        'configuration deletion retains durable authority and historical rows',
        () => configurationDeletionProbe(session, plan, fixture, before.durable),
      ));
      const lifetime = await deliveryLifetime(session, plan, fixture, pairing, socket);
      await socket.close();
      const restart = await restartWithoutPayload(
        session,
        pairing.receiverID,
        pairing.receiverToken,
        lifetime.message,
      );
      scenarios.push({
        name: 'retained dedup ends only with scheduled row deletion',
        status: 'passed',
        details: { ...lifetime, restart },
      });
      session.evidence.record({
        scenario: 'retained dedup ends only with scheduled row deletion',
        status: 'passed',
        details: { ...lifetime, restart },
      });

      const artifacts = await loadArtifacts(root);
      scenarios.push(await runScenario(
        session,
        'immutable retention job lifecycle and retry semantics have deterministic coverage',
        async () => {
          const deterministic = await runDeterministicTests(
            root,
            retentionDeterministicTestReferences,
          );
          return {
            ...deterministic,
            references: retentionDeterministicTestReferences.map(reference => ({
              test: reference.test,
              covers: reference.covers,
              observes: reference.observes,
            })),
          };
        },
        { evidenceType: 'deterministic' },
      ));
      const databaseRows = await session.remote.sqlJSON(runOwnedPrivacyQuery(plan, {
        receiverIDs: [...session.receivers],
        requestIDs: [...new Set(session.requestIds)],
        subjects: [...session.subjects],
      }));
      scenarios.push(await runScenario(
        session,
        'run-owned database, service logs and public artifacts retain no payload or credential',
        async () => ({
          ...(await verifyDiagnosticRedaction(session, {
            includeLogs: true,
            artifacts,
            databaseRows,
            requiredGroups: session.diagnosticGroups || {},
          })),
          fixtureIdentityVerified: false,
        }),
      ));
      return {
        scenarios,
        accounts: plan.accounts.length,
        senders: plan.senders.length,
        receivers: session.receivers.size,
        fixtureIdentity: 'synthetic-sql-not-provider-verified',
        job: {
          name: job.jobname,
          schedule: job.schedule,
          owner: job.username,
          database: job.database,
          active: job.active,
        },
        cleanupRuns: session.retentionRunIDs.size,
        deterministicTestReferences: retentionDeterministicTestReferences,
      };
    },
    async () => {
      await socket.close().catch(() => {});
      if (session.retentionRunIDs.size > 0) {
        await session.remote.sql(renderRetentionCleanupSQL(plan, [...session.retentionRunIDs]));
      } else {
        await session.remote.sql(renderRetentionCleanupSQL(plan));
      }
    },
    'Retention scenarios and cleanup both failed',
  );
}
