#!/usr/bin/env node
import { createHash, randomBytes } from 'node:crypto';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { validateDatabaseFacts } from './environment/database.mjs';
import { SmokeValidationError } from './support/errors.mjs';
import { Evidence } from './harness/evidence.mjs';
import { runExcludedProviderEvidence } from './harness/excluded-provider.mjs';
import { createFixturePlan } from './harness/fixtures.mjs';
import { SmokeBlocked, SmokeHttp, SmokeProblem } from './harness/http.mjs';
import { summarizeCompose } from './installation/installation.mjs';
import { modelConfiguration } from './environment/configuration.mjs';
import { runModelScenarios } from './model/index.mjs';
import { scriptDigests, validatePublishedRelease } from './environment/release.mjs';
import { acquireEnvironmentLock } from './environment/local-lock.mjs';
import { LocalEnvironment, repository } from './environment/local.mjs';
import { ensureSmokeDeployment } from './environment/provision.mjs';
import { verifyRecovery } from './environment/recovery.mjs';
import { verifySnapshotLocations } from './environment/restore.mjs';
import { SmokeSession } from './harness/session.mjs';
import { runSenderCredentialScenarios } from './credentials/index.mjs';
import { runDeliveryScenarios } from './delivery/index.mjs';
import { recoverDeliveryStorageFault } from './delivery/faults.mjs';
import { runPairingScenarios } from './pairing/index.mjs';
import { runReceiverScenarios } from './receiver/index.mjs';
import { runRetentionScenarios } from './retention/index.mjs';
import {
  retentionDrainProbeSchedule,
  startRunningRetentionProbe,
} from './retention/runtime.mjs';
import { renderRetentionCleanupSQL } from './retention/checks.mjs';
import { targetFromOptions } from './harness/target.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const exclusions = [
  'Real QQ or WeChat authorization and provider identity verification',
  'Owner-proof Sender or Receiver device-security revocation',
  'Owner-proof recover_receiver authorization and credential replacement',
];
function usage() {
  return [
    'Usage: node test/smoke/runner.mjs --integration-ready --release X.Y.Z',
    '',
    'The runner creates or updates /srv/pin-glide-smoke and its smoke .env, then',
    'writes and cleans up the guarded SQL fixture,',
    'exercises seeded Sender credential/session, Pairing, Delivery, Receiver and real',
    'Bailian model journeys plus privacy and retention cleanup through the local',
    'service, then creates a',
    'stopped-writer snapshot and restores it into a disposable database.',
    'It requires PIN_GLIDE_SMOKE_WRITE_AUTHORIZATION=pin-glide-smoke.',
  ].join('\n');
}

function argumentsFrom(argv) {
  const result = { mode: '', release: process.env.PIN_GLIDE_SMOKE_RELEASE || '' };
  for (let index = 0; index < argv.length; index++) {
    const argument = argv[index];
    if (argument === '--help' || argument === '-h') {
      result.help = true;
    } else if (argument === '--integration-ready') {
      if (result.mode) throw new Error('Choose exactly one smoke run mode');
      result.mode = argument.slice(2);
    } else if (argument === '--release') {
      if (index + 1 >= argv.length) throw new Error('--release needs X.Y.Z');
      result.release = argv[++index];
    } else {
      throw new Error(`Unknown smoke runner argument: ${argument}`);
    }
  }
  if (!result.help && !result.mode) throw new Error('Use --integration-ready to authorize the smoke run');
  return result;
}

function targetFor(options) {
  return targetFromOptions(options);
}

function imageFacts(result, label, reference) {
  const [rawLabels, rawDigests, id] = result.stdout.trim().split('|');
  let labels;
  let digests;
  try {
    labels = JSON.parse(rawLabels);
    digests = JSON.parse(rawDigests);
  } catch {
    throw new Error(`${label} image metadata is not valid JSON`);
  }
  if (!/^sha256:[0-9a-f]{64}$/.test(id || '')) {
    throw new Error(`${label} image has no immutable local ID`);
  }
  if (!Array.isArray(digests) || digests.length === 0) {
    throw new Error(`${label} image has no registry digest`);
  }
  const repository = reference.slice(0, reference.lastIndexOf(':'));
  if (!digests.some(digest => digest.startsWith(`${repository}@sha256:`))) {
    throw new Error(`${label} image digest does not identify ${repository}`);
  }
  return { labels: labels || {}, digests, id };
}

async function inspectImages(remote, target) {
  const expected = {
    api: `ghcr.io/buddylabsco/pin-glide-api:${target.release}`,
    dbTools: `ghcr.io/buddylabsco/pin-glide-db-tools:${target.release}`,
    postgres: 'ghcr.io/buddylabsco/pin-glide-postgres:18-1.6.8',
  };
  const images = {};
  for (const [name, reference] of Object.entries(expected)) {
    images[name] = { reference, ...imageFacts(await remote.image(reference), name, reference) };
  }
  for (const name of ['api', 'dbTools']) {
    const labels = images[name].labels;
    if (labels['org.opencontainers.image.source'] !== `https://github.com/${repository}`) {
      throw new Error(`${name} image source label does not identify the Pin Glide repository`);
    }
    if (labels['org.opencontainers.image.revision'] !== target.sourceCommit) {
      throw new Error(`${name} image revision does not match the published release commit`);
    }
  }
  return images;
}

function runningImage(result, label) {
  const [reference, rawLabels, id] = result.stdout.trim().split('|');
  let labels;
  try {
    labels = JSON.parse(rawLabels);
  } catch {
    throw new Error(`${label} container image metadata is not valid JSON`);
  }
  if (!/^sha256:[0-9a-f]{64}$/.test(id || '')) {
    throw new Error(`${label} container has no immutable image ID`);
  }
  return { reference, labels: labels || {}, id };
}

async function verifyPort(remote, target) {
  const result = await remote.apiContainerPort();
  const bindings = result.stdout.split('\n').map(line => line.trim()).filter(Boolean);
  if (!bindings.some(binding => binding.endsWith(`:${target.hostPort}`))) {
    throw new Error(`Running API does not publish the adopted host port ${target.hostPort}`);
  }
  return bindings;
}

async function check({ target, evidence, onPreflight = () => {} }) {
  const release = await validatePublishedRelease({ target, root });
  target.sourceCommit = release.sourceCommit;
  evidence.record({
    scenario: 'published release and contracts',
    status: 'passed',
    details: {
      tag: release.tag,
      sourceCommit: release.sourceCommit,
      checkedOutCommit: release.checkedOutCommit,
      upgradeOrigin: release.upgradeOrigin,
    },
  });

  const remote = new LocalEnvironment({ target });
  try {
    const installation = await ensureSmokeDeployment(remote, {
      root,
      release: target.release,
      origins: release.origins[target.release],
    });
    const deployment = await remote.verifyDeployment();
    await verifySnapshotLocations(remote);
    onPreflight();
    const config = await remote.composeConfig();
    const apiEnvironment = config.services?.api?.environment || {};
    const postgresEnvironment = config.services?.postgres?.environment || {};
    for (const secret of [
      apiEnvironment.PIN_GLIDE_DATABASE_URL,
      apiEnvironment.PIN_GLIDE_WECHAT_APP_SECRET,
      apiEnvironment.PIN_GLIDE_QQ_APP_KEY,
      apiEnvironment.PIN_GLIDE_BAILIAN_API_KEY,
      postgresEnvironment.POSTGRES_PASSWORD,
    ]) {
      evidence.addSecret(secret);
    }
    const compose = summarizeCompose(config, target, target.release);
    evidence.record({
      scenario: 'target and Compose identity',
      status: 'passed',
      details: { ...compose, deployment, installation },
    });
    const images = await inspectImages(remote, target);
    const running = {
      api: runningImage(await remote.deployedImage('api'), 'API'),
      postgres: runningImage(await remote.deployedImage('postgres'), 'PostgreSQL'),
    };
    for (const [name, image] of Object.entries(running)) {
      if (image.reference !== images[name].reference) {
        throw new Error(`Running ${name} container does not use ${images[name].reference}`);
      }
      if (image.id !== images[name].id) {
        throw new Error(`Running ${name} container image ID does not match the inspected release image`);
      }
    }
    evidence.record({
      scenario: 'immutable images',
      status: 'passed',
      details: Object.fromEntries(Object.entries(images).map(([name, image]) => [name, {
        reference: image.reference,
        id: image.id,
        digests: image.digests,
      }]).concat([['running', {
        api: running.api.id,
        postgres: running.postgres.id,
      }]])),
    });
    const scripts = await scriptDigests(root);
    const packagedManifest = (await remote.scriptManifest(images.dbTools.reference)).stdout;
    if (packagedManifest !== `${scripts.manifest.join('\n')}\n`) {
      throw new Error('Packaged SQL order manifest does not match the selected source manifest');
    }
    evidence.record({
      scenario: 'packaged SQL order manifest',
      status: 'passed',
      details: {
        image: images.dbTools.reference,
        digest: createHash('sha256').update(packagedManifest).digest('hex'),
        entries: scripts.manifest.length,
      },
    });
    const bindings = await verifyPort(remote, target);
    evidence.record({
      scenario: 'published local port',
      status: 'passed',
      details: { bindings },
    });

    const http = new SmokeHttp({ origin: target.httpOrigin });
    const health = await http.call({ path: '/health' });
    if (health.body?.status !== 'ok') throw new Error('API health did not report ok');
    evidence.record({
      scenario: 'HTTP health',
      status: 'passed',
      requestId: health.requestId,
      httpStatus: health.status,
    });

    const facts = await remote.schemaAudits();
    const model = await modelConfiguration(root);
    const database = validateDatabaseFacts(facts, {
      release: target.release,
      scripts,
      origins: release.origins,
      upgradeOrigin: release.upgradeOrigin,
      model,
    });
    evidence.record({
      scenario: database.installation === 'fresh' ? 'fresh installation' : 'declared supported upgrade',
      status: 'passed',
      details: {
        installation: database.installation,
        upgradeOrigin: database.upgradeOrigin,
        history: database.history,
      },
    });
    evidence.record({
      scenario: 'Database Release and converged configuration',
      status: 'passed',
      details: database,
    });
    return {
      remote,
      http,
      database,
      release,
      model,
      postgresImage: running.postgres.reference,
    };
  } catch (error) {
    if (error instanceof SmokeProblem || error instanceof SmokeValidationError) throw error;
    throw new SmokeValidationError(error);
  }
}

async function runSeededJourneys({ session, plan }) {
  const groups = session.diagnosticGroups;
  let start = session.requestIds.length;
  const pairing = await runPairingScenarios(session);
  groups.pairing = session.requestIds.slice(start);
  const { context, ...pairingEvidence } = pairing;
  start = session.requestIds.length;
  const credentials = await runSenderCredentialScenarios(session);
  groups.credentials = session.requestIds.slice(start);
  start = session.requestIds.length;
  const delivery = await runDeliveryScenarios(session, context);
  groups.delivery = session.requestIds.slice(start);
  start = session.requestIds.length;
  const receiver = await runReceiverScenarios(session, { root });
  groups.receiver = session.requestIds.slice(start);
  const retention = await runRetentionScenarios(session, { root });
  const excludedProvider = await runExcludedProviderEvidence({ session, root });
  return {
    credentials,
    pairing: pairingEvidence,
    delivery,
    receiver,
    retention,
    excludedProvider,
  };
}

async function integration({
  target,
  evidence,
  checkResult,
  createSession = SmokeSession.create,
  runLifecycle = runSeededJourneys,
  runModel = runModelScenarios,
  plan = createFixturePlan(),
  repairDeliveryStorage = recoverDeliveryStorageFault,
}) {
  let session;
  let cleaned = false;
  try {
    // Fixture bootstrap deletes run-owned Delivery rows, so a table a crashed
    // run left renamed must be repaired before that cleanup runs.
    if (checkResult.remote) await repairDeliveryStorage(checkResult.remote);
    session = await createSession({
      target,
      remote: checkResult.remote,
      http: checkResult.http,
      evidence,
      plan,
      retentionCleanup: (fixturePlan, runIDs) => checkResult.remote.sql(
        renderRetentionCleanupSQL(fixturePlan, runIDs),
      ),
    });
    session.diagnosticGroups = {};
    const modelStart = session.requestIds.length;
    const model = await runModel({ session, model: checkResult.model });
    session.diagnosticGroups.model = session.requestIds.slice(modelStart);
    const lifecycle = await runLifecycle({ session, plan });
    evidence.record({
      scenario: 'seeded Sender, Pairing, Delivery, Receiver, retention and real Bailian smoke journey',
      status: 'passed',
      details: {
        ...lifecycle,
        model,
        requestCount: session.requestIds.length,
      },
    });
    await session.close();
    cleaned = true;
    evidence.record({
      scenario: 'run-owned fixture cleanup',
      status: 'passed',
      details: { accounts: plan.accounts.length, senders: plan.senders.length },
    });
    return plan;
  } catch (error) {
    if (session && !cleaned) {
      try {
        await session.close();
        evidence.record({
          scenario: 'run-owned fixture cleanup',
          status: 'passed',
          details: { accounts: plan.accounts.length, senders: plan.senders.length },
        });
      } catch (cleanupError) {
        evidence.record({
          scenario: 'run-owned fixture cleanup',
          status: 'failed',
          details: { reason: cleanupError.message },
        });
      }
    }
    throw error;
  }
}

async function main(argv = process.argv.slice(2)) {
  const options = argumentsFrom(argv);
  if (options.help) {
    console.log(usage());
    return 0;
  }
  const target = targetFor(options);
  if (options.mode === 'integration-ready' && process.env.PIN_GLIDE_SMOKE_WRITE_AUTHORIZATION !== target.project) {
    throw new Error(`--integration-ready requires PIN_GLIDE_SMOKE_WRITE_AUTHORIZATION=${target.project}`);
  }
  const runId = `smoke-${Date.now()}-${randomBytes(6).toString('hex')}`;
  const evidence = new Evidence({ runId, exclusions, redact: false });
  const evidencePath = process.env.PIN_GLIDE_SMOKE_EVIDENCE
    || path.join(root, 'test/smoke/evidence', `${runId}.json`);
  let lock;
  let failureStatus = 'blocked';
  try {
    lock = await acquireEnvironmentLock({ target });
    const result = await check({
      target,
      evidence,
      onPreflight: () => {
        failureStatus = 'failed';
      },
    });
    failureStatus = 'failed';
    await verifyRecovery({
      checkResult: result,
      evidence,
      startDrainProbe: startRunningRetentionProbe,
      drainProbeSchedule: retentionDrainProbeSchedule,
    });
    await integration({ target, evidence, checkResult: result });
    const report = evidence.write(evidencePath);
    console.log(JSON.stringify(report, null, 2));
    return 0;
  } catch (error) {
    if (error instanceof SmokeBlocked) {
      failureStatus = 'blocked';
    } else if (error instanceof SmokeProblem || error instanceof SmokeValidationError) {
      failureStatus = 'failed';
    }
    evidence.record({
      scenario: 'seeded smoke journey',
      status: failureStatus,
      code: error instanceof SmokeProblem ? error.code : '',
      requestId: error instanceof SmokeProblem ? error.requestId : '',
      details: { reason: error.message },
    });
    const report = evidence.write(evidencePath);
    console.error(JSON.stringify(report, null, 2));
    return failureStatus === 'blocked' ? 2 : 1;
  } finally {
    if (lock) await lock.release();
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().then(code => {
    process.exitCode = code;
  }, error => {
    console.error(error.message);
    process.exitCode = 1;
  });
}

export { check, integration, main, targetFor };
