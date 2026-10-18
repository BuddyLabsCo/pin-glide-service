// Publish the GitHub Release for a merged auto-release pull request, run by the
// publish-release workflow after it re-runs the CI gates on the merged commit.
//
//   PR_NUMBER=<n> PR_MERGED=true PR_HEAD_REF=auto-release PR_BASE_REF=main \
//   PR_HEAD_REPOSITORY=<owner/repo> GITHUB_REPOSITORY=<owner/repo> MERGE_SHA=<sha> \
//   OPENAPI_ASSET=<path> ASYNCAPI_ASSET=<path> GH_TOKEN=<token> \
//   node scripts/release/publish-release.mjs
//
// Requires git, gh, Docker (authenticated to GHCR), and both descriptions
// exported from the release source commit. Tests:
//   node --test scripts/release/publish-release.test.mjs
//
// Only a merged pull request from the auto-release branch of this repository may
// publish, and it must have been squashed or rebased into main: a merge commit is
// rejected. Tag, images and Release assets are idempotent, so a re-run reuses the
// artifacts published from the same commit and refuses a conflicting one. The
// Release stays a draft until both validated descriptions are attached, so a
// partial failure can never publish an incomplete Release.
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const releaseBranch = 'auto-release';
const targetBranch = 'main';
const versionFile = 'version.json';
const packageScript = fileURLToPath(new URL('./package-release.sh', import.meta.url));
const source = 'https://github.com/BuddyLabsCo/pin-glide-service';
const applications = ['api', 'db-tools'];
const contracts = [
  { name: 'openapi.yaml', environment: 'OPENAPI_ASSET' },
  { name: 'asyncapi.yaml', environment: 'ASYNCAPI_ASSET' },
];

const run = (command, ...args) => execFileSync(command, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'] });
const git = (...args) => run('git', ...args);
const gh = (...args) => run('gh', ...args).trim();
const readGitFile = (ref, path) => git('show', `${ref}:${path}`);
const packageEndpoint = (service) => `/orgs/buddylabsco/packages/container/pin-glide-${service}`;

function requireEnvironment(name) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} must be set`);
  return value;
}

function validateVersion(version) {
  if (typeof version !== 'string' || !/^\d+\.\d+\.\d+$/.test(version)) {
    throw new Error(`Expected a three-part release version, found ${version}`);
  }
  return version;
}

function compareVersions(left, right) {
  const [a, b] = [left, right].map((version) => version.split('.').map(BigInt));
  for (let part = 0; part < 3; part += 1) {
    if (a[part] !== b[part]) return a[part] < b[part] ? -1 : 1;
  }
  return 0;
}

function spawnStatus(command, args) {
  try {
    execFileSync(command, args, { stdio: 'ignore' });
    return 0;
  } catch (error) {
    return error.status ?? 1;
  }
}

function isContained(commit) {
  return spawnStatus('git', ['merge-base', '--is-ancestor', commit, `origin/${targetBranch}`]) === 0;
}

// The workflow trigger already filters on the auto-release branch; this is the
// authority that rejects anything else that reaches the job.
function validateMergedPullRequest() {
  const repository = requireEnvironment('GITHUB_REPOSITORY');
  if (requireEnvironment('PR_MERGED') !== 'true') throw new Error('Only a merged pull request can publish a release');
  if (requireEnvironment('PR_HEAD_REF') !== releaseBranch) {
    throw new Error(`A release must be published from the ${releaseBranch} branch`);
  }
  if (requireEnvironment('PR_BASE_REF') !== targetBranch) {
    throw new Error(`A release must be merged into ${targetBranch}`);
  }
  if (requireEnvironment('PR_HEAD_REPOSITORY') !== repository) {
    throw new Error(`A release must come from ${repository}, not ${process.env.PR_HEAD_REPOSITORY}`);
  }
  requireEnvironment('PR_NUMBER');
  const merge = requireEnvironment('MERGE_SHA');
  if (!/^[a-f0-9]{40}$/.test(merge)) throw new Error(`MERGE_SHA must be a commit SHA, found ${merge}`);
  git('fetch', '--quiet', 'origin', `+refs/heads/${targetBranch}:refs/remotes/origin/${targetBranch}`);
  const [commit, ...parents] = git('rev-list', '--parents', '-n', '1', merge).trim().split(' ');
  if (commit !== merge) throw new Error(`Merge commit ${merge} is not in this checkout`);
  if (parents.length !== 1) {
    throw new Error(`Release pull requests must be squashed or rebased into ${targetBranch}, not merged with a merge commit`);
  }
  if (!isContained(merge)) throw new Error(`Merge commit ${merge} is not contained in origin/${targetBranch}`);
  return merge;
}

function releaseVersion(merge) {
  const version = validateVersion(JSON.parse(readGitFile(merge, versionFile)).version);
  const parentVersion = validateVersion(JSON.parse(readGitFile(`${merge}^`, versionFile)).version);
  if (compareVersions(version, parentVersion) <= 0) {
    throw new Error(`Release version ${version} must be greater than the merged parent version ${parentVersion}`);
  }
  return version;
}

// A real 404 means a new package (GHCR creates it private). Other failures must
// never be treated as permission to overwrite an existing version.
function packageInfo(service) {
  let response;
  try {
    response = gh('api', '--include', packageEndpoint(service));
  } catch (error) {
    if (/^HTTP\/[\d.]+ 404\b/.test(error.stdout ?? '')) return null;
    throw error;
  }
  const metadata = JSON.parse(response.split(/\r?\n\r?\n/).slice(1).join('\n\n'));
  if (metadata.visibility !== 'private') throw new Error(`pin-glide-${service} must be private before publishing`);
  return metadata;
}

function referenceTag(reference) {
  return reference.slice(reference.lastIndexOf(':') + 1);
}

// The Compose file is the authority for the infrastructure distribution, so a
// release publishes exactly the image deployments pull. Its tag is independent
// of the application version: an existing one is reused, never rebuilt.
function infrastructureTarget(merge) {
  const compose = readGitFile(merge, 'deploy/compose/docker-compose.yml');
  const match = compose.match(/^\s*image:\s*(ghcr\.io\/buddylabsco\/(pin-glide-postgres:[0-9][\w.-]*))\s*$/m);
  if (!match) throw new Error('deploy/compose/docker-compose.yml must reference the pin-glide-postgres image');
  return { service: 'postgres', reference: `ghcr.io/buddylabsco/${match[2]}`, exact: false };
}

function verifyImage({ reference, exact }, merge) {
  run('docker', 'pull', reference);
  const labels = JSON.parse(run('docker', 'image', 'inspect', reference, '--format', '{{json .Config.Labels}}'));
  if (labels?.['org.opencontainers.image.source'] !== source) {
    throw new Error(`${reference} was not published from ${source}; refusing to use it`);
  }
  if (exact && labels['org.opencontainers.image.revision'] !== merge) {
    throw new Error(`${reference} does not match release commit ${merge}; refusing to overwrite it`);
  }
}

// Both application images come from the exact release commit; the shared
// PostgreSQL/pg_cron image keeps its own tag across ordinary releases.
function publishImages(merge, version) {
  const targets = [
    ...applications.map((service) => ({ service, exact: true, reference: `ghcr.io/buddylabsco/pin-glide-${service}:${version}` })),
    infrastructureTarget(merge),
  ];
  const images = targets.map((target) => {
    const metadata = packageInfo(target.service);
    const tags = metadata ? gh('api', `${packageEndpoint(target.service)}/versions`, '--paginate', '--jq', '.[].metadata.container.tags[]').split('\n') : [];
    return { ...target, exists: tags.includes(referenceTag(target.reference)) };
  });
  let context;
  try {
    for (const image of images) {
      if (!image.exists) {
        context ??= run(packageScript, merge, version).trim();
        run('docker', 'build', '--file', join(context, 'deploy/compose', `${image.service}.Dockerfile`),
          '--label', `org.opencontainers.image.revision=${merge}`, '--label', `org.opencontainers.image.source=${source}`,
          '--tag', image.reference, context);
        run('docker', 'push', image.reference);
      }
      if (!packageInfo(image.service)) throw new Error(`${image.reference} is not available as a private package`);
      verifyImage(image, merge);
    }
  } finally {
    if (context) rmSync(context, { recursive: true, force: true });
  }
}

function ensureTag(tag, merge) {
  const peeled = git('ls-remote', 'origin', `refs/tags/${tag}^{}`).trim();
  const direct = git('ls-remote', 'origin', `refs/tags/${tag}`).trim();
  const existing = (peeled || direct).split(/\s+/)[0];
  if (existing) {
    if (existing !== merge) throw new Error(`Tag ${tag} already points at ${existing}, not ${merge}`);
    console.log(`Tag ${tag} already points at ${merge}.`);
    return;
  }
  git('tag', tag, merge);
  git('push', 'origin', `refs/tags/${tag}`);
}

// The release chain generated and validated these files at the release source
// commit. Re-checking their identity here is what makes the attached bytes the
// validated description rather than whatever the workflow happened to pass.
function contractAssets(merge) {
  return contracts.map(({ name, environment }) => {
    const path = requireEnvironment(environment);
    let content;
    try {
      content = readFileSync(path, 'utf8');
    } catch {
      throw new Error(`${environment} must point to the exported ${name} description`);
    }
    if (content !== readGitFile(merge, `generated/protocol/${name}`)) {
      throw new Error(`${name} does not match the validated description at ${merge}`);
    }
    return { name, path, content };
  });
}

function releaseState(tag) {
  const view = ['release', 'view', tag, '--json', 'isDraft,assets'];
  if (spawnStatus('gh', view) !== 0) return null;
  return JSON.parse(gh(...view));
}

function assertMatchingAsset(tag, asset, directory) {
  run('gh', 'release', 'download', tag, '--pattern', asset.name, '--dir', directory, '--clobber');
  if (readFileSync(join(directory, asset.name), 'utf8') !== asset.content) {
    throw new Error(`${asset.name} is already published with different contents; corrections use a new release`);
  }
}

function publishRelease(tag, notes, assets) {
  const state = releaseState(tag);
  const directory = mkdtempSync(join(tmpdir(), 'pin-glide-db-tools-assets-'));
  try {
    if (state && !state.isDraft) {
      for (const asset of assets) assertMatchingAsset(tag, asset, directory);
      console.log(`Release ${tag} is already published.`);
      return;
    }
    if (!state) gh('release', 'create', tag, '--title', tag, '--notes', notes, '--verify-tag', '--draft');
    const published = new Set((state?.assets ?? []).map(({ name }) => name));
    for (const asset of assets) {
      if (published.has(asset.name)) assertMatchingAsset(tag, asset, directory);
      else gh('release', 'upload', tag, asset.path);
    }
    gh('release', 'edit', tag, '--draft=false');
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
}

function main() {
  const merge = validateMergedPullRequest();
  const version = releaseVersion(merge);
  const tag = `v${version}`;
  const assets = contractAssets(merge);
  ensureTag(tag, merge);
  publishImages(merge, version);
  const notes = gh('pr', 'view', requireEnvironment('PR_NUMBER'), '--json', 'body', '--jq', '.body');
  publishRelease(tag, notes, assets);
  console.log(`Published ${tag} from ${merge}.`);
}

try {
  main();
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
}
