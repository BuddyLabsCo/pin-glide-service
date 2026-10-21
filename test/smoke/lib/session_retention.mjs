import { createHash, randomBytes } from 'node:crypto';
import fs from 'node:fs/promises';
import path from 'node:path';

import {
  cleanupCallSQL,
  renderConfigurationDeleteProbeSQL,
  renderDeliveryAgeSQL,
  renderRetentionCleanupSQL,
  renderRetentionInterruptedSQL,
  runOwnedPrivacyQuery,
} from './retention_checks.mjs';
import {
  renderRetentionAgingSQL,
  renderRetentionBoundarySQL,
  retentionMarker,
} from './retention_fixtures.mjs';
import { runScenario } from './session_credential_support.mjs';
import { verifyDiagnosticRedaction } from './session_diagnostics.mjs';
import {
  deliveryItem,
  expectMetadata,
  expectNoMessage,
  expectResult,
  messageID,
  metadata,
  nextDelivery,
  openReceiver,
  sendReceipt,
  upload,
} from './session_delivery_support.mjs';
import { pairReceiver } from './session_pairing_helpers.mjs';
import { runDeterministicTests } from './session_receiver_support.mjs';
import {
  assertRetentionCounts,
  databaseNow,
  jsonFromOutput,
  retentionState,
  runScheduledCleanupProbe,
  stableRetentionJob,
} from './retention_runtime.mjs';

export {
  assertRetentionCounts,
  jsonFromOutput,
  retentionProbeCommand,
  runScheduledCleanupProbe,
  stableRetentionJob,
  unknownWriterBlocksSnapshot,
} from './retention_runtime.mjs';

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

async function loadArtifacts(root) {
  if (!root) return {};
  const artifacts = {};
  for (const relative of ['generated/protocol/openapi.yaml', 'generated/protocol/asyncapi.yaml']) {
    artifacts[relative] = await fs.readFile(path.join(root, relative), 'utf8');
  }
  const scripts = await fs.readdir(path.join(root, 'db/scripts'));
  for (const name of scripts.filter(item => item.endsWith('.sql')).sort()) {
    const relative = `db/scripts/${name}`;
    artifacts[relative] = await fs.readFile(path.join(root, relative), 'utf8');
  }
  return artifacts;
}

function assertDurableState(before, after) {
  if (JSON.stringify(before.durable) !== JSON.stringify(after.durable)) {
    throw new Error('Scheduled cleanup changed durable configuration, authority or release-ledger state');
  }
  if (
    after.durable.accounts !== 2
    || after.durable.senders !== before.durable.senders
    || after.durable.receiver !== 1
    || after.durable.receiverCredential !== 1
    || after.durable.pairing !== 1
    || after.durable.model !== 1
    || after.durable.retentionJob !== 1
  ) {
    throw new Error('The run-owned durable authority or configuration is incomplete');
  }
}

async function interruptedCleanup(session, plan, fixture) {
  const before = await retentionState(session, plan, fixture);
  assertRetentionCounts(before, {
    'aging.deliveryOld': 1,
    'aging.credentialOld': 1,
    'aging.operationOld': 1,
    'aging.sessionOld': 1,
    'aging.diagnosticOld': 1,
    'aging.auditOld': 1,
    'aging.cronOld': 1,
  });
  const result = await session.remote.sql(
    renderRetentionInterruptedSQL(fixture.referenceTime),
    { allowFailure: true },
  );
  if (result.code === 0) throw new Error('The interrupted cleanup unexpectedly committed');
  const after = await retentionState(session, plan, fixture);
  assertRetentionCounts(after, {
    'aging.deliveryOld': 1,
    'aging.credentialOld': 1,
    'aging.operationOld': 1,
    'aging.sessionOld': 1,
    'aging.diagnosticOld': 1,
    'aging.auditOld': 1,
    'aging.cronOld': 1,
  });
  return { interruptedExitCode: result.code, checkpoint: false, partialRowsDeleted: 0 };
}

async function boundaryCleanup(session, plan, fixture) {
  await session.remote.sql(renderRetentionBoundarySQL(plan, fixture));
  const cleanup = await session.remote.sqlJSON(cleanupCallSQL(fixture.referenceTime));
  const state = await retentionState(session, plan, fixture);
  assertRetentionCounts(state, {
    'boundary.deliveryCutoff': 1,
    'boundary.deliveryPastCutoff': 0,
    'boundary.credentialCutoff': 1,
    'boundary.credentialPastCutoff': 0,
    'boundary.operationCutoff': 1,
    'boundary.operationPastCutoff': 0,
    'boundary.sessionCutoff': 1,
    'boundary.sessionPastCutoff': 0,
    'boundary.diagnosticCutoff': 1,
    'boundary.diagnosticPastCutoff': 0,
    'boundary.auditCutoff': 1,
    'boundary.auditPastCutoff': 0,
    'boundary.cronCutoff': 1,
    'boundary.cronPastCutoff': 0,
    'live.credential': 1,
    'live.guard': 1,
  });
  return { removed: Number(cleanup.removed), state };
}

async function proveSlidingFamily(session, plan, fixture) {
  const refreshed = await session.refresh('pair2', {
    token: fixture.liveToken,
    operationID: `${retentionMarker(plan)}-live-refresh`,
    attempt: 0,
  });
  const cleanup = await session.remote.sqlJSON(cleanupCallSQL(fixture.referenceTime));
  const state = await retentionState(session, plan, fixture);
  assertRetentionCounts(state, { 'live.credential': 1, 'live.guard': 1 });
  await session.verifyUploader(refreshed.uploader.token);
  return {
    rotatedSenderID: refreshed.senderId,
    liveCredentialRows: state.live.credential,
    liveGuardRows: state.live.guard,
    cleanupRemoved: Number(cleanup.removed),
  };
}

async function deliveryLifetime(session, plan, fixture, pairing, socket) {
  const target = { ...pairing, senderID: session.sender('pair2').id };
  const message = `${retentionMarker(plan)}-delivery-lifetime`;
  const first = upload(session, 'pair2', message, [deliveryItem(target)]);
  sendReceipt(socket, await nextDelivery(socket, target));
  expectResult(await first, 0, pairing.receiverID, 'received');
  const initial = expectMetadata(await metadata(session, 'pair2', message), [{
    receiverID: pairing.receiverID,
    status: 'received',
  }])[pairing.receiverID];

  await session.remote.sql(renderDeliveryAgeSQL(plan, message));
  const lookup = expectMetadata(await metadata(session, 'pair2', message), [{
    receiverID: pairing.receiverID,
    status: 'received',
    createdAt: initial.createdAt,
    completedAt: initial.completedAt,
  }])[pairing.receiverID];
  const duplicate = await upload(session, 'pair2', message, [
    deliveryItem(target, { ciphertext: Buffer.alloc(48, 71).toString('base64') }),
  ]);
  expectResult(duplicate, 0, pairing.receiverID, 'received');
  await expectNoMessage(socket, 'A retained Delivery aged past its window sent a second frame');

  const probe = await runScheduledCleanupProbe(session);
  const purged = await session.expectProblem({
    path: `/v1/messages/${encodeURIComponent(message)}`,
    token: session.token('pair2', 'uploader'),
  }, 404, 'resource_not_found');

  const repeated = upload(session, 'pair2', message, [deliveryItem(target)]);
  sendReceipt(socket, await nextDelivery(socket, target));
  expectResult(await repeated, 0, pairing.receiverID, 'received');
  const repeatedView = expectMetadata(await metadata(session, 'pair2', message), [{
    receiverID: pairing.receiverID,
    status: 'received',
  }])[pairing.receiverID];
  if (Date.parse(repeatedView.createdAt) <= Date.parse(initial.createdAt)) {
    throw new Error('A deleted Delivery identity did not admit the accepted new send');
  }
  return {
    message,
    deferredDuplicate: duplicate.body.results[0].status,
    lookupDidNotExtendLifetime: lookup.createdAt === initial.createdAt,
    scheduledProbe: probe,
    purged,
    acceptedRepeat: repeatedView.status,
    noPayloadFields: !/ciphertext|nonce|clientKey|apiKey/i.test(JSON.stringify(repeatedView)),
  };
}

async function restartWithoutPayload(session, receiverID, receiverToken, message) {
  await session.remote.restartAPI();
  const reconnected = await openReceiver(session, receiverToken);
  try {
    await expectNoMessage(reconnected, 'Restart replayed retained ciphertext', 1_000);
    const response = await metadata(session, 'pair2', message);
    const retained = expectMetadata(response, [{
      receiverID,
      status: 'received',
    }]);
    const retainedPayloadFields = /ciphertext|nonce|clientKey|apiKey/i.test(
      JSON.stringify(response.body),
    );
    if (retainedPayloadFields) throw new Error('Retained Message metadata exposed payload or key fields');
    return {
      restarted: true,
      replayedPayload: false,
      retainedStatus: retained[Object.keys(retained)[0]].status,
      retainedPayloadFields,
    };
  } finally {
    await reconnected.close();
  }
}

async function configurationDeletionProbe(session, plan, fixture, expectedDurable) {
  const result = await session.remote.sql(renderConfigurationDeleteProbeSQL(plan, fixture));
  const probe = jsonFromOutput(result, 'Retention configuration deletion probe');
  const durable = {
    account: Number(probe.account),
    sender: Number(probe.sender),
    receiver: Number(probe.receiver),
    receiverCredential: Number(probe.receiverCredential),
    pairing: Number(probe.pairing),
    releaseDataScripts: Number(probe.releaseDataScripts),
    releaseDeployHistory: Number(probe.releaseDeployHistory),
    releaseMigrationScripts: Number(probe.releaseMigrationScripts),
    retentionPolicies: Number(probe.retentionPolicies),
    retentionJob: Number(probe.retentionJob),
  };
  if (
    Number(probe.modelConfigurations) !== 0
    || durable.account !== plan.accounts.length
    || durable.sender !== 1
    || durable.receiver !== 1
    || durable.receiverCredential !== 1
    || durable.pairing !== 1
    || durable.releaseDataScripts !== expectedDurable.releaseDataScripts
    || durable.releaseDeployHistory !== expectedDurable.releaseDeployHistory
    || durable.releaseMigrationScripts !== expectedDurable.releaseMigrationScripts
    || durable.retentionPolicies !== 7
    || durable.retentionJob !== 1
  ) {
    throw new Error(`Configuration deletion probe = ${JSON.stringify(probe)}`);
  }
  if (
    Number(probe.deliveryHistory) < 1
    || Number(probe.diagnosticHistory) < 1
    || Number(probe.auditHistory) < 1
  ) {
    throw new Error('Configuration deletion cascaded into retained transaction or log history');
  }
  return {
    transaction: 'rolled_back',
    modelRowsVisibleAfterDelete: Number(probe.modelConfigurations),
    durable,
    retainedHistory: {
      delivery: Number(probe.deliveryHistory),
      diagnostics: Number(probe.diagnosticHistory),
      audits: Number(probe.auditHistory),
    },
  };
}

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
  session.addSecret(fixture.liveToken);
  const scenarios = [];
  try {
    const job = await stableRetentionJob(session);
    fixture.jobID = Number(job.jobID);
    fixture.referenceTime = await databaseNow(session);
    const before = await retentionState(session, plan, fixture);
    assertDurableState(before, before);

    await session.remote.sql(renderRetentionAgingSQL(plan, fixture));
    scenarios.push(await runScenario(
      session,
      'interrupted retention cleanup rolls back without a checkpoint',
      () => interruptedCleanup(session, plan, fixture),
    ));
    const probe = await runScheduledCleanupProbe(session);
    const afterProbe = await retentionState(session, plan, fixture);
    assertRetentionCounts(afterProbe, zeroAging);
    scenarios.push({
      name: 'pg_cron executes configuration-driven retention with no Go fallback',
      status: 'passed',
      details: probe,
    });
    session.evidence.record({
      scenario: 'pg_cron executes configuration-driven retention with no Go fallback',
      status: 'passed',
      details: {
        job,
        probe,
        deletedOutsideWindows: afterProbe.aging,
        nullableCronEndTimeRetained: afterProbe.aging.cronRunning,
        noGoCleanupFallback: true,
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
  } finally {
    await socket.close().catch(() => {});
    if (session.retentionRunIDs.size > 0) {
      await session.remote.sql(renderRetentionCleanupSQL(plan, [...session.retentionRunIDs]));
    } else {
      await session.remote.sql(renderRetentionCleanupSQL(plan));
    }
  }
}
