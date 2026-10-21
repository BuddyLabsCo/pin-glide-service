#!/usr/bin/env node
import { createHash, randomBytes } from 'node:crypto';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { validateDatabaseFacts } from './lib/database.mjs';
import { SmokeValidationError } from './lib/errors.mjs';
import { Evidence } from './lib/evidence.mjs';
import { createFixturePlan } from './lib/fixtures.mjs';
import { SmokeBlocked, SmokeHttp, SmokeProblem } from './lib/http.mjs';
import { summarizeCompose } from './lib/installation.mjs';
import { modelConfiguration } from './lib/configuration.mjs';
import { runModelScenarios } from './lib/model.mjs';
import { contractBytes, scriptDigests, validatePublishedRelease } from './lib/release.mjs';
import { acquireEnvironmentLock } from './lib/remote_lock.mjs';
import { RemoteEnvironment, repository } from './lib/remote.mjs';
import { recoveryRestoreScenario, verifyRecovery } from './lib/recovery.mjs';
import { recoverySnapshot, verifySnapshotLocations } from './lib/restore.mjs';
import { SmokeSession } from './lib/session.mjs';
import { runSenderCredentialScenarios } from './lib/session_credentials.mjs';
import { runDeliveryScenarios } from './lib/session_delivery.mjs';
import { recoverDeliveryStorageFault } from './lib/delivery_faults.mjs';
import { runPairingScenarios } from './lib/session_pairing.mjs';
import { runReceiverScenarios } from './lib/session_receiver.mjs';
import { runRetentionScenarios } from './lib/session_retention.mjs';
import { targetFromOptions } from './lib/target.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const exclusions = [
  'Real QQ or WeChat authorization and provider identity verification',
  'Owner-proof Sender security revocation',
  'Owner-proof recover_receiver authorization and credential replacement',
];
function usage() {
  return [
    'Usage: node test/smoke/runner.mjs --check --release X.Y.Z',
    '       node test/smoke/runner.mjs --integration-ready --release X.Y.Z',
    '',
    '--check verifies the published release, deployed Compose identity, health,',
    'schema/data ledgers, contracts and snapshot inventory without writes. It',
    'always exits 2 after prerequisites pass because it does not execute restore.',
    '--integration-ready additionally writes and cleans up the guarded SQL fixture,',
    'exercises seeded Sender credential/session, Pairing, Delivery, Receiver and real',
    'Bailian model journeys plus privacy and retention cleanup through the deployed',
    'service, then creates a',
    'stopped-writer snapshot and restores it into a disposable database.',
    'It requires PIN_GLIDE_SMOKE_WRITE_AUTHORIZATION=pin-glide-dev.',
  ].join('\n');
}

function argumentsFrom(argv) {
  const result = { mode: '', release: process.env.PIN_GLIDE_SMOKE_RELEASE || '' };
  for (let index = 0; index < argv.length; index++) {
    const argument = argv[index];
    if (argument === '--help' || argument === '-h') {
      result.help = true;
    } else if (['--check', '--integration-ready'].includes(argument)) {
      if (result.mode) throw new Error('Choose exactly one smoke run mode');
      result.mode = argument.slice(2);
    } else if (argument === '--release') {
      if (index + 1 >= argv.length) throw new Error('--release needs X.Y.Z');
      result.release = argv[++index];
    } else {
      throw new Error(`Unknown smoke runner argument: ${argument}`);
    }
  }
  if (!result.help && !result.mode) throw new Error('Use --check or explicitly authorize --integration-ready');
  return result;
}

function targetFor(options) {
  return targetFromOptions(options);
}

export function checkExitCode(mode) {
  return mode === 'check' ? 2 : 0;
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

async function check({ target, evidence, onPreflight = () => {}, allowMissingSnapshot = false }) {
  const release = await validatePublishedRelease({ target, root });
  target.sourceCommit = release.sourceCommit;
  evidence.record({
    scenario: 'published release and contracts',
    status: 'passed',
    details: {
      tag: release.tag,
      sourceCommit: release.sourceCommit,
      upgradeOrigin: release.upgradeOrigin,
      assets: release.assets,
    },
  });

  const remote = new RemoteEnvironment({ target });
  const deployment = await remote.verifyDeployment();
  await verifySnapshotLocations(remote);
  onPreflight();
  const snapshot = await recoverySnapshot(remote, { allowMissing: true });
  try {
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
      details: { ...compose, deployment },
    });
    if (snapshot || !allowMissingSnapshot) {
      evidence.record({
        scenario: 'server and workstation snapshot inventory',
        status: snapshot ? 'passed' : 'blocked',
        details: snapshot || { reason: 'no completed development snapshot exists yet' },
      });
    }
    if (!allowMissingSnapshot) {
      evidence.record({
        scenario: recoveryRestoreScenario,
        status: 'blocked',
        details: {
          reason: snapshot
            ? '--check is read-only and does not execute the restore'
            : '--check is read-only and no completed snapshot is available',
        },
      });
    }

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
      scenario: 'published LAN port',
      status: 'passed',
      details: { bindings },
    });

    evidence.record({
      scenario: 'published contract identities',
      status: 'passed',
      details: Object.fromEntries(Object.entries(release.assets).map(([name, asset]) => [name, asset.digest])),
    });
    const local = await contractBytes(root);
    for (const name of ['openapi.yaml', 'asyncapi.yaml']) {
      const digest = createHash('sha256').update(local[name]).digest('hex');
      if (digest !== release.assets[name].digest) {
        throw new Error(`Checked-in ${name} is not the published release asset`);
      }
    }

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
      snapshot,
      model,
      postgresImage: running.postgres.reference,
    };
  } catch (error) {
    if (error instanceof SmokeProblem || error instanceof SmokeValidationError) throw error;
    throw new SmokeValidationError(error);
  }
}

async function runSeededJourneys({ session, plan }) {
  const pairing = await runPairingScenarios(session);
  const { context, ...pairingEvidence } = pairing;
  return {
    credentials: await runSenderCredentialScenarios({ session, plan }),
    pairing: pairingEvidence,
    delivery: await runDeliveryScenarios(session, context),
    receiver: await runReceiverScenarios(session, { root }),
    retention: await runRetentionScenarios(session, { root }),
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
    });
    const model = await runModel({ session, model: checkResult.model });
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
  const evidence = new Evidence({ runId, exclusions });
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
      allowMissingSnapshot: options.mode === 'integration-ready',
    });
    const exitCode = checkExitCode(options.mode);
    if (exitCode !== 0) {
      const report = evidence.write(evidencePath);
      console.error(JSON.stringify(report, null, 2));
      return exitCode;
    }
    if (options.mode === 'integration-ready') {
      failureStatus = 'failed';
      await verifyRecovery({ checkResult: result, evidence });
      await integration({ target, evidence, checkResult: result });
    }
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
      scenario: options.mode === 'integration-ready' ? 'seeded smoke journey' : 'read-only deployment check',
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
