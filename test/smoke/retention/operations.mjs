import fs from 'node:fs/promises';
import path from 'node:path';

import { retention } from '../environment/database.mjs';
import {
  cleanupCallSQL,
  renderConfigurationDeleteProbeSQL,
  renderDeliveryAgeSQL,
  renderRetentionInterruptedSQL,
} from './checks.mjs';
import {
  renderRetentionBoundarySQL,
  retentionMarker,
} from './fixtures.mjs';
import {
  jsonFromOutput,
  retentionState,
  runScheduledCleanupProbe,
} from './runtime.mjs';
import { assertRetentionCounts } from './runtime.mjs';
import {
  deliveryItem,
    deliveryRowQuery,
  expectMetadata,
  expectNoMessage,
  expectResult,
  metadata,
  nextDelivery,
  openReceiver,
  sendReceipt,
  upload,
} from '../delivery/support.mjs';

export async function loadArtifacts(root) {
  if (!root) return {};
  const artifacts = {};
  for (const relative of ['generated/protocol/openapi.yaml', 'generated/protocol/asyncapi.yaml']) {
    artifacts[relative] = await fs.readFile(path.join(root, relative), 'utf8');
  }
  for (const relative of await sqlFiles(path.join(root, 'db'), 'db')) {
    artifacts[relative] = await fs.readFile(path.join(root, relative), 'utf8');
  }
  return artifacts;
}

async function sqlFiles(directory, relativeDirectory) {
  const entries = await fs.readdir(directory, { withFileTypes: true });
  const found = [];
  for (const entry of entries.sort((left, right) => left.name.localeCompare(right.name))) {
    const relative = `${relativeDirectory}/${entry.name}`;
    if (entry.isDirectory()) found.push(...await sqlFiles(path.join(directory, entry.name), relative));
    else if (entry.name.endsWith('.sql')) found.push(relative);
  }
  return found;
}

export function assertDurableBaseline(state) {
  if (
    state.durable.accounts !== 2
    || state.durable.receiver !== 1
    || state.durable.receiverCredential !== 1
    || state.durable.pairing !== 1
    || state.durable.model !== 1
    || state.durable.retentionPolicies !== retention.size
    || state.durable.retentionJob !== 1
  ) {
    throw new Error('The run-owned durable authority or configuration is incomplete');
  }
}

export function assertDurableState(before, after) {
  if (JSON.stringify(before.durable) !== JSON.stringify(after.durable)) {
    throw new Error('Scheduled cleanup changed durable configuration, authority or release-ledger state');
  }
  assertDurableBaseline(after);
}

export async function interruptedCleanup(session, plan, fixture) {
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
  return { interruptedExitCode: result.code, partialRowsDeleted: 0 };
}

export async function boundaryCleanup(session, plan, fixture) {
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

export async function proveSlidingFamily(session, plan, fixture) {
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

export async function deliveryLifetime(session, plan, fixture, pairing, socket) {
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
  const aged = await session.remote.sqlJSON(deliveryRowQuery(session, 'pair2', message));
  const lookup = expectMetadata(await metadata(session, 'pair2', message), [{
    receiverID: pairing.receiverID,
    status: 'received',
    createdAt: aged.createdAt,
    completedAt: aged.completedAt,
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
    lookupDidNotExtendLifetime: Date.parse(lookup.createdAt) === Date.parse(aged.createdAt)
      && Date.parse(lookup.completedAt) === Date.parse(aged.completedAt),
    scheduledProbe: probe,
    purged,
    acceptedRepeat: repeatedView.status,
    noPayloadFields: !/ciphertext|nonce|clientKey|apiKey/i.test(JSON.stringify(repeatedView)),
  };
}

export async function restartWithoutPayload(session, receiverID, receiverToken, message) {
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

export async function configurationDeletionProbe(session, plan, fixture, expectedDurable) {
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
    || durable.retentionPolicies !== expectedDurable.retentionPolicies
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
