// Run in a disposable checkout: PUSH_BEFORE=<pre-push SHA> node scripts/release/auto-release-pr.mjs
// Requires git, gh, and GH_TOKEN. Tests: node --test scripts/release/auto-release-pr.test.mjs
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const releaseBranch = 'auto-release';
const releaseTitle = 'auto-release new version';
const versionFile = 'version.json';
const run = (command, ...args) => execFileSync(command, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'] });
const git = (...args) => run('git', ...args);
const gh = (...args) => run('gh', ...args).trim();
const readJSON = (path) => JSON.parse(readFileSync(path, 'utf8'));

function validateVersion(version) {
  if (typeof version !== 'string' || !/^\d+\.\d+\.\d+$/.test(version)) {
    throw new Error(`Expected a three-part release version, found ${version}`);
  }
  return version;
}

function main() {
  const before = process.env.PUSH_BEFORE;
  if (!before || !/^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(before)) {
    throw new Error('PUSH_BEFORE must be the push event before commit SHA');
  }
  const paths = (/^0+$/.test(before)
    ? git('ls-tree', '-r', '--name-only', '-z', 'HEAD')
    : git('diff', '--no-renames', '--name-only', '-z', before, 'HEAD')
  ).split('\0').filter(Boolean);
  // A push that only carries the version file is the merge of a release proposal
  // and must not start the next one; documentation and workflow files never ship.
  const eligible = paths.some((path) =>
    path !== versionFile && !path.endsWith('.md') && !path.startsWith('.github/'));
  const existingPR = gh('pr', 'list', '--base', 'main', '--head', releaseBranch, '--state', 'open', '--json', 'number', '--jq', '.[0].number // empty');

  // Ignored paths prevent creation, not updates to an open release proposal.
  if (!eligible && !existingPR) {
    console.log('No release-eligible changes; skipping release preparation.');
    return;
  }

  const currentVersion = validateVersion(readJSON(versionFile).version);
  let nextVersion;
  if (existingPR) {
    git('fetch', '--quiet', 'origin', `+refs/heads/${releaseBranch}:refs/remotes/origin/${releaseBranch}`);
    nextVersion = validateVersion(JSON.parse(git('show', `origin/${releaseBranch}:${versionFile}`)).version);
  } else {
    const [major, minor, patch] = currentVersion.split('.');
    nextVersion = `${major}.${minor}.${BigInt(patch) + 1n}`;
  }

  const previousTag = git('tag', '--list', 'v*', '--sort=-v:refname').trim().split('\n')[0];
  const changelog = git('log', '--no-merges', '--pretty=format:- %s', previousTag ? `${previousTag}..HEAD` : 'HEAD');
  const body = [
    `## Proposed release ${nextVersion}`, '',
    `Changelog base: ${previousTag || 'the repository root'}.`, '',
    '### Changelog', '', changelog, '', '---', '',
    'This pull request is created for release-eligible pushes to `main` and refreshed for all pushes while open.',
    'Merging it publishes the application images, the PostgreSQL/pg_cron image and both validated contract assets; it never creates the next proposal.',
    'Maintainer-proposed versions are preserved.',
    'Merge it with squash or rebase: the publish workflow rejects a merge commit, and only this branch publishes.', '',
  ].join('\n');

  if (eligible) {
    const versionJSON = readJSON(versionFile);
    versionJSON.version = nextVersion;
    git('config', 'user.name', 'github-actions[bot]');
    git('config', 'user.email', '41898282+github-actions[bot]@users.noreply.github.com');
    git('checkout', '-B', releaseBranch);
    writeFileSync(versionFile, `${JSON.stringify(versionJSON, null, 2)}\n`);
    git('add', versionFile);
    if (git('diff', '--cached', '--name-only').trim()) {
      git('commit', '-m', `chore: propose release v${nextVersion}`);
      git('push', '--force', 'origin', releaseBranch);
    }
  }

  const temporary = mkdtempSync(join(tmpdir(), 'pin-glide-db-tools-pr-'));
  try {
    const bodyFile = join(temporary, 'body.md');
    writeFileSync(bodyFile, body);
    if (existingPR) {
      gh('pr', 'edit', existingPR, '--title', releaseTitle, '--body-file', bodyFile);
    } else {
      gh('pr', 'create', '--base', 'main', '--head', releaseBranch, '--title', releaseTitle, '--body-file', bodyFile);
    }
  } finally {
    rmSync(temporary, { recursive: true, force: true });
  }
}

try {
  main();
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
}
