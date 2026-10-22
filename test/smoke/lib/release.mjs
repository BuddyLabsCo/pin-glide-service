import { createHash } from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';

import { runProcess } from './commands.mjs';
import { SmokeValidationError } from './errors.mjs';
import { validateReleaseMetadata } from './installation.mjs';
import { repository } from './remote.mjs';

function versionParts(value) {
  if (typeof value !== 'string' || !/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.test(value)) {
    return null;
  }
  return value.split('.').map(part => BigInt(part));
}

function compareVersions(left, right) {
  const a = versionParts(left);
  const b = versionParts(right);
  if (!a || !b) return 0;
  for (let index = 0; index < 3; index++) {
    if (a[index] !== b[index]) return a[index] > b[index] ? 1 : -1;
  }
  return 0;
}

async function checksum(filename) {
  return createHash('sha256').update(await fs.promises.readFile(filename)).digest('hex');
}

export async function scriptDigests(root, run = runProcess) {
  const result = await run('bash', ['scripts/release/migration-order.sh'], { cwd: root });
  const manifest = result.stdout.trim().split('\n').filter(Boolean);
  if (manifest.length === 0 || new Set(manifest).size !== manifest.length) {
    throw new Error('Release SQL order manifest is empty or contains duplicate paths');
  }
  const data = {};
  const migrations = {};
  for (const relative of manifest) {
    const match = relative.match(/^db\/(scripts\/[^/]+\.sql|migrations\/(release|pin_glide)\/[^/]+\.sql)$/);
    if (!match) throw new Error(`Release SQL order manifest contains invalid path ${relative}`);
    if (relative.startsWith('db/scripts/')) {
      const filename = path.basename(relative);
      data[filename] = await checksum(path.join(root, relative));
    } else {
      const schema = match[2];
      migrations[`${schema}/${path.basename(relative)}`] = await checksum(path.join(root, relative));
    }
  }
  return { data, migrations, manifest };
}

export async function contractBytes(root) {
  const result = {};
  for (const name of ['openapi.yaml', 'asyncapi.yaml']) {
    result[name] = await fs.promises.readFile(path.join(root, 'generated/protocol', name));
  }
  return result;
}

export function parseReleaseSupport(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error('Release support declaration must be an object');
  }
  if (!value.origins || typeof value.origins !== 'object' || Array.isArray(value.origins)) {
    throw new Error('Release support declaration must contain an origins object');
  }
  const origins = {};
  for (const [target, candidates] of Object.entries(value.origins)) {
    if (!versionParts(target)) throw new Error(`Release support target ${target} is not X.Y.Z`);
    if (!Array.isArray(candidates)) {
      throw new Error(`Release support origins for ${target} must be an array`);
    }
    const seen = new Set();
    for (const candidate of candidates) {
      if (!versionParts(candidate) || compareVersions(candidate, target) >= 0) {
        throw new Error(`Release support origin ${candidate} for ${target} is not an earlier X.Y.Z release`);
      }
      if (seen.has(candidate)) {
        throw new Error(`Release support origin ${candidate} for ${target} is duplicated`);
      }
      seen.add(candidate);
    }
    origins[target] = [...candidates];
  }
  return { ...value, origins };
}

export async function releaseSupport(root) {
  let value;
  try {
    value = JSON.parse(await fs.promises.readFile(path.join(root, 'test/smoke/release-support.json'), 'utf8'));
  } catch (error) {
    throw new SmokeValidationError(new Error(`Release support declaration is invalid: ${error.message}`));
  }
  try {
    return parseReleaseSupport(value);
  } catch (error) {
    throw new SmokeValidationError(error);
  }
}

export async function publishedUpgradeOrigin({ version, releaseRepo, allowedOrigins, run = runProcess }) {
  if (!Array.isArray(allowedOrigins)) {
    throw new SmokeValidationError(new Error('Declared release origins must be an array'));
  }
  if (allowedOrigins.length === 0) return null;
  const result = await run('gh', [
    'release',
    'list',
    '--repo',
    releaseRepo,
    '--limit',
    '100',
    '--json',
    'tagName,isDraft,isPrerelease',
  ]);
  let metadata;
  try {
    metadata = JSON.parse(result.stdout);
  } catch (error) {
    throw new SmokeValidationError(new Error(`Published release list is not valid JSON: ${error.message}`));
  }
  if (!Array.isArray(metadata)) throw new SmokeValidationError(new Error('Published release list is not an array'));
  let origin = null;
  for (const release of metadata) {
    if (release?.isDraft || release?.isPrerelease || typeof release?.tagName !== 'string') continue;
    if (!release.tagName.startsWith('v')) continue;
    const candidate = release.tagName.slice(1);
    if (!versionParts(candidate) || compareVersions(candidate, version) >= 0) continue;
    if (!allowedOrigins.includes(candidate)) continue;
    if (origin === null || compareVersions(candidate, origin) > 0) origin = candidate;
  }
  return origin;
}

export async function validatePublishedRelease({
  target,
  root,
  run = runProcess,
  requireClean = true,
  runContracts = true,
}) {
  const head = (await run('git', ['rev-parse', 'HEAD'], { cwd: root })).stdout.trim();
  if (!/^[a-f0-9]{40}$/.test(head)) {
    throw new SmokeValidationError(new Error('Could not resolve the checked-out source commit'));
  }
  if (requireClean) {
    const status = (await run('git', ['status', '--porcelain'], { cwd: root })).stdout;
    if (status.trim()) {
      throw new SmokeValidationError(new Error('The smoke source worktree must be clean'));
    }
  }
  const releaseRepo = repository;
  const commit = (await run('gh', [
    'api',
    `repos/${releaseRepo}/commits/v${target.release}`,
    '--jq',
    '.sha',
  ], { cwd: root })).stdout.trim();
  const metadataResult = await run('gh', [
    'release',
    'view',
    `v${target.release}`,
    '--repo',
    releaseRepo,
    '--json',
    'tagName,isDraft,isPrerelease,targetCommitish,assets',
  ], { cwd: root });
  let metadata;
  try {
    metadata = JSON.parse(metadataResult.stdout);
  } catch (error) {
    throw new SmokeValidationError(new Error(`Published release metadata is not valid JSON: ${error.message}`));
  }
  const support = await releaseSupport(root);
  if (!Object.hasOwn(support.origins, target.release)) {
    throw new SmokeValidationError(new Error(`Release ${target.release} has no declared upgrade support`));
  }
  const declaredOrigins = support.origins[target.release];
  let upgradeOrigin = null;
  if (declaredOrigins.length > 0) {
    upgradeOrigin = await publishedUpgradeOrigin({
      version: target.release,
      releaseRepo,
      allowedOrigins: declaredOrigins,
      run,
    });
    if (declaredOrigins.length > 0 && (!upgradeOrigin || !declaredOrigins.includes(upgradeOrigin))) {
      throw new SmokeValidationError(new Error(`Release ${target.release} has no published declared upgrade origin`));
    }
  }
  let validated;
  try {
    validated = validateReleaseMetadata(metadata, {
      version: target.release,
      sourceCommit: commit,
      assetBytes: await contractBytes(root),
    });
  } catch (error) {
    throw new SmokeValidationError(error);
  }
  if (runContracts) {
    try {
      await run('bash', ['scripts/quality/check-contracts.sh'], { cwd: root, timeoutMs: 300_000 });
    } catch (error) {
      throw new SmokeValidationError(new Error(`Published release contract validation failed: ${error.message}`));
    }
  }
  return {
    ...validated,
    checkedOutCommit: head,
    tag: metadata.tagName,
    publishedAt: metadata.publishedAt || '',
    upgradeOrigin,
    origins: support.origins,
  };
}
