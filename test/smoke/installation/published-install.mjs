#!/usr/bin/env node
import { randomBytes } from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { runProcess } from '../support/commands.mjs';
import { Evidence } from '../harness/evidence.mjs';
import { SmokeValidationError } from '../support/errors.mjs';
import { releaseVersion } from '../harness/target.mjs';
import { validatePublishedRelease } from '../environment/release.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../..');
const source = 'https://github.com/BuddyLabsCo/pin-glide-service';
const repository = 'BuddyLabsCo/pin-glide-service';
const postgresImage = 'ghcr.io/buddylabsco/pin-glide-postgres:18-1.6.8';

export function parseArguments(argv) {
  const result = { mode: '', from: '', to: '' };
  for (let index = 0; index < argv.length; index++) {
    const argument = argv[index];
    if (argument === '--check' || argument === '--integration-ready') {
      if (result.mode) throw new Error('Choose exactly one published installation mode');
      result.mode = argument.slice(2);
    } else if (argument === '--from' || argument === '--to') {
      if (index + 1 >= argv.length) throw new Error(`${argument} needs X.Y.Z`);
      const key = argument.slice(2);
      if (result[key]) throw new Error(`${argument} may be provided once`);
      result[key] = releaseVersion(argv[++index]);
    } else {
      throw new Error(`Unknown published installation argument: ${argument}`);
    }
  }
  if (!result.mode || !result.from || !result.to) {
    throw new Error('Use --check or --integration-ready with explicit --from and --to releases');
  }
  return result;
}

export function rewriteCompose(config, project) {
  if (!config?.services || typeof config.services !== 'object') {
    throw new Error('Published Compose configuration has no services');
  }
  const rewritten = { name: project, services: {}, volumes: { database: {} } };
  for (const name of ['postgres', 'db-tools', 'api']) {
    const service = config.services[name];
    if (!service) throw new Error(`Published Compose configuration is missing ${name}`);
    const copy = structuredClone(service);
    for (const key of ['build', 'ports', 'volumes', 'networks', 'container_name', 'profiles']) {
      delete copy[key];
    }
    if (copy.healthcheck) {
      Object.assign(copy.healthcheck, { interval: '1s', timeout: '3s', retries: 60 });
    }
    if (name === 'postgres') {
      if (copy.image !== postgresImage) {
        throw new Error(`Published Compose must use ${postgresImage}`);
      }
      copy.volumes = ['database:/var/lib/postgresql'];
    } else {
      copy.image = `ghcr.io/buddylabsco/pin-glide-${name}:\${RELEASE}`;
    }
    if (name === 'db-tools') copy.container_name = `${project}-db-tools`;
    rewritten.services[name] = copy;
  }
  return rewritten;
}

function cleanEnvironment() {
  return Object.fromEntries(Object.entries(process.env).filter(([key]) =>
    !/^(?:COMPOSE_|RELEASE$|PIN_GLIDE_|POSTGRES_|BAILIAN_|WECHAT_|QQ_)/.test(key)));
}

function writePrivate(filename, value) {
  fs.writeFileSync(filename, value, { mode: 0o600 });
}

async function createInstallation({ directory, project, config, release, env, run }) {
  fs.mkdirSync(directory, { recursive: true, mode: 0o700 });
  fs.copyFileSync(path.join(root, 'install.sh'), path.join(directory, 'install.sh'));
  writePrivate(path.join(directory, '.env'), [
    `COMPOSE_PROJECT_NAME=${project}`,
    `RELEASE=${release}`,
    'POSTGRES_USER=pin_glide',
    `POSTGRES_PASSWORD=${randomBytes(24).toString('hex')}`,
    '',
  ].join('\n'));
  writePrivate(path.join(directory, 'compose.yaml'), `${JSON.stringify(config, null, 2)}\n`);
  return { directory, project, env, run };
}

async function install(installation, version, action, { initial = false } = {}) {
  return installation.run('bash', [
    'install.sh',
    '--version',
    version,
    action,
    ...(initial ? ['--initial-install'] : []),
  ], { cwd: installation.directory, env: installation.env, timeoutMs: 900_000 });
}

async function compose(installation, ...args) {
  return installation.run('docker', [
    'compose',
    '--env-file',
    '.env',
    '-f',
    'compose.yaml',
    ...args,
  ], { cwd: installation.directory, env: installation.env, timeoutMs: 300_000 });
}

async function queryDatabase(installation, statement) {
  return (await compose(
    installation,
    'exec',
    '-T',
    'postgres',
    'psql',
    '-X',
    '-v',
    'ON_ERROR_STOP=1',
    '-U',
    'pin_glide',
    '-d',
    'pin_glide',
    '-Atc',
    statement,
  )).stdout.trim();
}

async function verifyReady(installation, version) {
  const health = await compose(
    installation,
    'exec',
    '-T',
    'api',
    'wget',
    '-q',
    '-O',
    '-',
    'http://127.0.0.1:8080/health',
  );
  const body = JSON.parse(health.stdout);
  if (body.status !== 'ok') throw new Error('Published installation API is not healthy');
  const release = await queryDatabase(
    installation,
    'SELECT release_version FROM release.release_deploy_history ORDER BY id DESC LIMIT 1',
  );
  if (release !== version) throw new Error(`Published installation Release is ${release}, not ${version}`);
  const images = {};
  for (const [service, reference] of [
    ['api', `ghcr.io/buddylabsco/pin-glide-api:${version}`],
    ['postgres', postgresImage],
  ]) {
    const containerID = (await compose(installation, 'ps', '-q', service)).stdout.trim();
    if (!containerID) throw new Error(`Published installation ${service} container is missing`);
    const imageID = (await installation.run(
      'docker',
      ['image', 'inspect', reference, '--format', '{{.Id}}'],
      { env: installation.env, timeoutMs: 30_000 },
    )).stdout.trim();
    const detail = (await installation.run(
      'docker',
      ['inspect', containerID, '--format', '{{.Config.Image}}|{{.Image}}'],
      { env: installation.env, timeoutMs: 30_000 },
    )).stdout.trim();
    const [runningReference, runningID] = detail.split('|');
    if (runningReference !== reference || runningID !== imageID) {
      throw new Error(`Published installation ${service} is not running ${reference}`);
    }
    images[service] = { reference, id: imageID };
  }
  return images;
}

async function verifyLabels(run, release, commit, env) {
  const images = {};
  for (const service of ['api', 'db-tools']) {
    const reference = `ghcr.io/buddylabsco/pin-glide-${service}:${release}`;
    const result = await run('docker', [
      'image',
      'inspect',
      reference,
      '--format',
      '{{json .Config.Labels}}',
    ], { env, timeoutMs: 30_000 });
    const labels = JSON.parse(result.stdout);
    if (labels?.['org.opencontainers.image.source'] !== source) {
      throw new Error(`${reference} has the wrong source label`);
    }
    if (labels['org.opencontainers.image.revision'] !== commit) {
      throw new Error(`${reference} has the wrong revision label`);
    }
    const id = (await run('docker', [
      'image',
      'inspect',
      reference,
      '--format',
      '{{.Id}}',
    ], { env, timeoutMs: 30_000 })).stdout.trim();
    images[service === 'db-tools' ? 'dbTools' : service] = { reference, id };
  }
  return images;
}

async function publishedOrigin(version, run, env) {
  const result = await run('gh', [
    'release',
    'view',
    `v${version}`,
    '--repo',
    repository,
    '--json',
    'tagName,isDraft,isPrerelease',
  ], { env, timeoutMs: 30_000 });
  let metadata;
  try {
    metadata = JSON.parse(result.stdout);
  } catch (error) {
    throw new SmokeValidationError(new Error(`Upgrade origin v${version} metadata is invalid: ${error.message}`));
  }
  if (metadata.tagName !== `v${version}` || metadata.isDraft || metadata.isPrerelease) {
    throw new SmokeValidationError(new Error(`Upgrade origin v${version} is not a published stable release`));
  }
  const commit = (await run('gh', [
    'api',
    `repos/${repository}/commits/v${version}`,
    '--jq',
    '.sha',
  ], { env, timeoutMs: 30_000 })).stdout.trim();
  return commit;
}

export async function cleanupInstallations({ installations, work, env }) {
  const errors = [];
  for (const installation of [...installations].reverse()) {
    try {
      await installation.run('docker', [
        'compose',
        '--env-file',
        '.env',
        '-f',
        'compose.yaml',
        'down',
        '--volumes',
        '--remove-orphans',
        '--timeout',
        '10',
      ], { cwd: installation.directory, env, timeoutMs: 120_000 });
    } catch (error) {
      errors.push(error);
    }
  }
  if (errors.length === 0) {
    try {
      fs.rmSync(work, { recursive: true, force: true });
    } catch (error) {
      errors.push(error);
    }
  }
  return errors;
}

async function integration({ options, release, originCommit, evidence, env, run }) {
  const work = fs.mkdtempSync(path.join(os.tmpdir(), 'pin-glide-published-install-'));
  const sourceEnv = path.join(work, 'source.env');
  writePrivate(sourceEnv, [
    `RELEASE=${options.to}`,
    'COMPOSE_PROJECT_NAME=pin-glide-published-source',
    'POSTGRES_USER=pin_glide',
    `POSTGRES_PASSWORD=${randomBytes(24).toString('hex')}`,
    'BAILIAN_API_KEY=published-install-placeholder',
    '',
  ].join('\n'));
  let base;
  let failure;
  try {
    base = JSON.parse((await run('docker', [
      'compose',
      '--env-file',
      sourceEnv,
      '-f',
      path.join(root, 'deploy/compose/docker-compose.yml'),
      'config',
      '--format',
      'json',
    ], { env, timeoutMs: 30_000 })).stdout);
  } catch (error) {
    fs.rmSync(work, { recursive: true, force: true });
    throw error;
  }
  const installations = [];

  try {
    try {
    const freshProject = `pin-glide-published-fresh-${randomBytes(6).toString('hex')}`;
    const fresh = await createInstallation({
      directory: path.join(work, freshProject),
      project: freshProject,
      config: rewriteCompose(base, freshProject),
      release: options.to,
      env,
      run,
    });
    installations.push(fresh);
    await install(fresh, options.to, '--prep', { initial: true });
    await install(fresh, options.to, '--initial-deploy');
    const freshImages = {
      ...await verifyReady(fresh, options.to),
      ...await verifyLabels(run, options.to, release.sourceCommit, env),
    };
    evidence.record({
      scenario: 'published fresh installation',
      status: 'passed',
      details: {
        release: options.to,
        sourceCommit: release.sourceCommit,
        images: freshImages,
      },
    });
    } catch (error) {
      evidence.record({
        scenario: 'published fresh installation',
        status: 'failed',
        details: {
          release: options.to,
          sourceCommit: release.sourceCommit,
          reason: error.message,
        },
      });
      evidence.record({
        scenario: `published declared upgrade ${options.from} to ${options.to}`,
        status: 'blocked',
        details: { reason: 'the fresh installation did not complete' },
      });
      throw error;
    }

    try {
    const upgradeProject = `pin-glide-published-upgrade-${randomBytes(6).toString('hex')}`;
    const upgrade = await createInstallation({
      directory: path.join(work, upgradeProject),
      project: upgradeProject,
      config: rewriteCompose(base, upgradeProject),
      release: options.from,
      env,
      run,
    });
    installations.push(upgrade);
    await install(upgrade, options.from, '--prep', { initial: true });
    await install(upgrade, options.from, '--initial-deploy');
    const originImages = {
      ...await verifyReady(upgrade, options.from),
      ...await verifyLabels(run, options.from, originCommit, env),
    };
    await install(upgrade, options.to, '--prep');
    await install(upgrade, options.to, '--full-release');
    const upgradedImages = {
      ...await verifyReady(upgrade, options.to),
      ...await verifyLabels(run, options.to, release.sourceCommit, env),
    };
    evidence.record({
      scenario: `published declared upgrade ${options.from} to ${options.to}`,
      status: 'passed',
      details: {
        origin: options.from,
        originCommit,
        release: options.to,
        sourceCommit: release.sourceCommit,
        originImages,
        upgradedImages,
      },
    });
    } catch (error) {
      evidence.record({
        scenario: `published declared upgrade ${options.from} to ${options.to}`,
        status: 'failed',
        details: {
          origin: options.from,
          originCommit,
          release: options.to,
          sourceCommit: release.sourceCommit,
          reason: error.message,
        },
      });
      throw error;
    }
  } catch (error) {
    failure = error;
  } finally {
    const cleanupErrors = await cleanupInstallations({ installations, work, env });
    if (cleanupErrors.length > 0) {
      failure = failure
        ? new AggregateError(
          [failure, ...cleanupErrors],
          `Published installation and cleanup both failed; retained ${work}`,
        )
        : new AggregateError(cleanupErrors, `Published installation cleanup failed; retained ${work}`);
    }
  }
  if (failure) throw failure;
}

async function main(argv = process.argv.slice(2)) {
  const options = parseArguments(argv);
  const env = cleanEnvironment();
  const evidence = new Evidence({
    runId: `published-install-${Date.now()}-${randomBytes(6).toString('hex')}`,
    redact: false,
    exclusions: [
      'Business API journeys after installation',
      'Bailian model issuance covered by the local runner',
      'Real QQ or WeChat authorization',
    ],
  });
  const evidencePath = process.env.PIN_GLIDE_SMOKE_EVIDENCE
    || path.join(root, 'test/smoke/evidence', `${evidence.runId}.json`);
  let prerequisitesPassed = false;
  try {
    const release = await validatePublishedRelease({
      target: { release: options.to },
      root,
      run: (command, args, runOptions = {}) => runProcess(command, args, { ...runOptions, env }),
    });
    if (!release.origins[options.to].includes(options.from)) {
      throw new SmokeValidationError(
        new Error(`${options.from} is not a declared supported origin for ${options.to}`),
      );
    }
    const originCommit = await publishedOrigin(options.from, runProcess, env);
    prerequisitesPassed = true;
    if (options.mode === 'check') {
      evidence.record({
        scenario: 'published installation prerequisites',
        status: 'passed',
        details: {
          origin: options.from,
          originCommit,
          release: options.to,
          sourceCommit: release.sourceCommit,
        },
      });
    } else {
      await integration({ options, release, originCommit, evidence, env, run: runProcess });
    }
    const report = evidence.write(evidencePath);
    console.log(JSON.stringify(report, null, 2));
    return 0;
  } catch (error) {
    const failed = prerequisitesPassed || error instanceof SmokeValidationError;
    evidence.record({
      scenario: options.mode === 'check' ? 'published installation prerequisites' : 'published installation journey',
      status: failed ? 'failed' : 'blocked',
      details: { reason: error.message },
    });
    const report = evidence.write(evidencePath);
    console.error(JSON.stringify(report, null, 2));
    return failed ? 1 : 2;
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

export { main };
