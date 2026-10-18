import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

const script = fileURLToPath(new URL('./publish-release.mjs', import.meta.url));
const versionFile = 'version.json';
const repository = 'BuddyLabsCo/pin-glide-service';
const source = `https://github.com/${repository}`;
const contractNames = ['openapi.yaml', 'asyncapi.yaml'];
const descriptions = {
  'openapi.yaml': 'openapi: 3.1.0\ninfo:\n  title: Pin Glide\n  version: 1.10.0\npaths: {}\n',
  'asyncapi.yaml': 'asyncapi: 3.0.0\ninfo:\n  title: Pin Glide\n  version: 1.10.0\nchannels: {}\n',
};
const applicationImages = ['api', 'db-tools'].map((service) => `ghcr.io/buddylabsco/pin-glide-${service}:1.10.1`);
const postgresImage = 'ghcr.io/buddylabsco/pin-glide-postgres:18-1.6.8';
const services = ['api', 'db-tools', 'postgres'];

const githubCLI = String.raw`#!/usr/bin/env node
const fs = require('node:fs');
const args = process.argv.slice(2), state = JSON.parse(fs.readFileSync(process.env.PUBLISH_TEST_STATE, 'utf8'));
const flag = name => args[args.indexOf(name) + 1];
const assetName = args[3] && args[3].split('/').at(-1);
fs.appendFileSync(process.env.PUBLISH_TEST_LOG, JSON.stringify({ args }) + '\n');
if (state.fail === args[0] + ' ' + args[1]) { console.error('simulated gh ' + state.fail + ' failure'); process.exit(42); }
if (state.fail === 'release upload ' + assetName) { console.error('simulated gh release upload failure'); process.exit(42); }
if (state.fail === 'package access' && args[0] === 'api') { console.log('HTTP/2.0 403 Forbidden\n\n' + JSON.stringify({message: 'Forbidden'})); process.exit(1); }
if (args[0] === 'api') {
  const endpoint = args.find(arg => arg.startsWith('/orgs/'));
  const name = endpoint.split('/')[5], pkg = state.packages[name];
  if (!pkg) { console.log('HTTP/2.0 404 Not Found\n\n' + JSON.stringify({message: 'Package not found.'})); process.exit(1); }
  if (endpoint.endsWith('/versions')) console.log(Object.keys(state.images).filter(image => image.startsWith('ghcr.io/buddylabsco/' + name + ':')).map(image => image.split(':').at(-1)).join('\n'));
  else console.log('HTTP/2.0 200 OK\n\n' + JSON.stringify(pkg));
} else if (args[0] === 'pr' && args[1] === 'view') {
  if (flag('--json') !== 'body' || flag('--jq') !== '.body') process.exit(91);
  console.log(state.body);
} else if (args[0] === 'release' && args[1] === 'view') {
  if (flag('--json') !== 'isDraft,assets') process.exit(95);
  if (!state.release) { console.error('release not found'); process.exit(1); }
  console.log(JSON.stringify({ isDraft: state.release.draft, assets: Object.keys(state.release.assets).map(name => ({ name })) }));
} else if (args[0] === 'release' && args[1] === 'create') {
  if (state.release || !args.includes('--verify-tag') || !args.includes('--draft') || !flag('--title') || !flag('--notes')) process.exit(92);
  state.release = { tag: args[2], draft: true, assets: {} };
} else if (args[0] === 'release' && args[1] === 'upload') {
  if (!state.release?.draft || state.release.assets[assetName] !== undefined) process.exit(96);
  state.release.assets[assetName] = fs.readFileSync(args[3], 'utf8');
} else if (args[0] === 'release' && args[1] === 'download') {
  const content = state.release?.assets[flag('--pattern')];
  if (content === undefined) process.exit(97);
  fs.writeFileSync(require('node:path').join(flag('--dir'), flag('--pattern')), content);
} else if (args[0] === 'release' && args[1] === 'edit') {
  if (!state.release?.draft || !args.includes('--draft=false')) process.exit(98);
  state.release.draft = false;
} else process.exit(90);
fs.writeFileSync(process.env.PUBLISH_TEST_STATE, JSON.stringify(state));
`;

const dockerCLI = String.raw`#!/usr/bin/env node
const fs = require('node:fs'), path = require('node:path');
const args = process.argv.slice(2), state = JSON.parse(fs.readFileSync(process.env.PUBLISH_TEST_STATE, 'utf8'));
const flag = name => args[args.indexOf(name) + 1];
const entry = { command: 'docker', args };
if (state.fail === args[0] + ' ' + args[1]) { console.error('simulated docker ' + state.fail + ' failure'); process.exit(42); }
if (args[0] === 'build') {
  const context = args.at(-1);
  entry.source = fs.readFileSync(path.join(context, 'src/app.js'), 'utf8');
  entry.version = JSON.parse(fs.readFileSync(path.join(context, 'version.json'), 'utf8')).version;
  const manifest = path.join(context, 'db/migrations/order.txt');
  entry.migrations = fs.existsSync(manifest) ? fs.readFileSync(manifest, 'utf8').split('\n').filter(Boolean) : null;
  if (!fs.existsSync(flag('--file'))) process.exit(96);
  state.built = { image: flag('--tag'), labels: Object.fromEntries(args.flatMap((arg, i) => arg === '--label' ? [args[i+1].split(/=(.*)/s).slice(0, 2)] : [])) };
} else if (args[0] === 'push') {
  if (state.images[args[1]]) { console.error('attempted image overwrite'); process.exit(97); }
  const name = args[1].split('/').at(-1).split(':')[0];
  state.packages[name] ??= { visibility: state.newVisibility ?? 'private' };
  state.images[args[1]] = state.built.labels;
} else if (args[0] === 'pull') {
  if (!state.images[args[1]]) process.exit(98);
} else if (args[0] === 'image' && args[1] === 'inspect') {
  if (!state.images[args[2]]) process.exit(99);
  console.log(JSON.stringify(state.images[args[2]]));
} else process.exit(90);
fs.appendFileSync(process.env.PUBLISH_TEST_LOG, JSON.stringify(entry) + '\n');
fs.writeFileSync(process.env.PUBLISH_TEST_STATE, JSON.stringify(state));
if (args[0] === 'push' && state.failAfterPush === args[1]) { console.error('push completed remotely but response was lost'); process.exit(42); }
`;

// All cases exercise the public CLI against real Git history and remote refs.
// Only the external GitHub and Docker boundaries are faked.
function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), 'pin-glide-publish-test-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const cwd = join(root, 'repo'), origin = join(root, 'origin.git');
  const bin = join(root, 'bin'), state = join(root, 'gh-state.json'), log = join(root, 'gh-log.jsonl');
  const exported = join(root, 'release-assets');
  for (const path of [cwd, bin, exported]) mkdirSync(path);
  const env = { ...process.env, HOME: root, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1',
    GIT_TERMINAL_PROMPT: '0', PATH: `${bin}:${process.env.PATH}`, GH_TOKEN: 'test-token',
    PUBLISH_TEST_STATE: state, PUBLISH_TEST_LOG: log, GITHUB_REPOSITORY: repository };
  for (const key of ['GIT_DIR', 'GIT_WORK_TREE', 'GIT_INDEX_FILE', 'GIT_COMMON_DIR']) delete env[key];
  const exec = (command, args) => spawnSync(command, args, { cwd, env, encoding: 'utf8', timeout: 20000 });
  const git = (...args) => {
    const result = exec('git', args);
    assert.equal(result.status, 0, `git ${args.join(' ')}: ${result.stderr || result.error}`);
    return result.stdout.trim();
  };
  const write = (path, content) => {
    mkdirSync(dirname(join(cwd, path)), { recursive: true });
    writeFileSync(join(cwd, path), content);
  };
  const versions = (version) => write(versionFile, JSON.stringify({ version }, null, 2) + '\n');
  writeFileSync(state, JSON.stringify({ body: '## Proposed release 1.10.1\n\n### Changelog\n\n- Fix request routing\n', release: null, fail: '', packages: {}, images: {} }));
  writeFileSync(log, '');
  writeFileSync(join(bin, 'gh'), githubCLI);
  chmodSync(join(bin, 'gh'), 0o755);
  writeFileSync(join(bin, 'docker'), dockerCLI);
  chmodSync(join(bin, 'docker'), 0o755);
  git('init', '--bare', origin);
  git('init', '-b', 'main');
  git('config', 'user.name', 'Publish Test');
  git('config', 'user.email', 'publish-test@example.invalid');
  git('config', 'commit.gpgsign', 'false');
  git('config', 'core.hooksPath', '/dev/null');
  git('remote', 'add', 'origin', origin);
  versions('1.10.0');
  write('src/app.js', 'export const value = 1;\n');
  write('deploy/compose/docker-compose.yml', 'name: pin-glide\nservices:\n  postgres:\n    image: ghcr.io/buddylabsco/pin-glide-postgres:18-1.6.8\n');
  for (const service of services) write(`deploy/compose/${service}.Dockerfile`, 'FROM scratch\n');
  for (const [name, content] of Object.entries(descriptions)) write(`generated/protocol/${name}`, content);
  git('add', '.'); git('commit', '-m', 'Initial implementation');
  git('push', '-u', 'origin', 'main');
  // The publish workflow exports these files at the release commit; in the test
  // they start out as the descriptions committed with the release.
  const exportAssets = () => {
    for (const name of contractNames) copyFileSync(join(cwd, 'generated/protocol', name), join(exported, name));
  };
  exportAssets();
  const calls = () => readFileSync(log, 'utf8').trim().split('\n').filter(Boolean).map(JSON.parse);
  const stateJSON = () => JSON.parse(readFileSync(state, 'utf8'));
  const configure = (patch) => writeFileSync(state, JSON.stringify({ ...stateJSON(), ...patch }));
  const propose = (version, files = {}) => {
    git('checkout', '-b', 'auto-release');
    versions(version);
    for (const [path, content] of Object.entries(files)) write(path, content);
    git('add', '.'); git('commit', '-m', `chore: propose release v${version}`);
    git('push', 'origin', 'auto-release');
    git('checkout', 'main');
  };
  const squash = (message = 'auto-release new version') => {
    git('checkout', 'main');
    git('merge', '--squash', 'auto-release');
    git('commit', '--allow-empty', '-m', message);
    git('push', 'origin', 'main');
    return git('rev-parse', 'HEAD');
  };
  const merge = (message = 'auto-release new version') => {
    git('checkout', 'main');
    git('merge', '--no-ff', 'auto-release', '-m', message);
    git('push', 'origin', 'main');
    return git('rev-parse', 'HEAD');
  };
  const commit = (path, content, title) => {
    git('checkout', 'main'); write(path, content);
    git('add', '.'); git('commit', '-m', title); return git('rev-parse', 'HEAD');
  };
  const run = (overrides = {}, success = true) => {
    const start = calls().length;
    const result = spawnSync(process.execPath, [script], { cwd, encoding: 'utf8', timeout: 20000,
      env: { ...env, PR_NUMBER: '122', PR_MERGED: 'true', PR_HEAD_REF: 'auto-release', PR_BASE_REF: 'main',
        PR_HEAD_REPOSITORY: repository, MERGE_SHA: '', OPENAPI_ASSET: join(exported, 'openapi.yaml'),
        ASYNCAPI_ASSET: join(exported, 'asyncapi.yaml'), ...overrides } });
    assert.equal(result.error, undefined, 'CLI must finish without a timeout or spawn error');
    if (success) assert.equal(result.status, 0, result.stderr);
    return { ...result, calls: calls().slice(start) };
  };
  const remoteTag = (tag) => git('ls-remote', 'origin', `refs/tags/${tag}`).split(/\s+/)[0];
  const exportedAsset = (name, content) => writeFileSync(join(exported, name), content);
  const published = (name) => stateJSON().release?.assets?.[name];
  const tamper = (name, content) => {
    const next = stateJSON();
    next.release.assets[name] = content;
    writeFileSync(state, JSON.stringify(next));
  };
  return { cwd, exported, git, write, versions, propose, squash, merge, commit, run, remoteTag, configure,
    calls, exportedAsset, published, tamper, release: () => stateJSON().release, description: (name) => descriptions[name] };
}

// Regression: publishing partial or checkout-contaminated artifacts. Seam: CLI.
// Observe the exported build context, the image identities and the Release assets.
test('squash-merged release publishes both application images, the shared postgres image and both contract assets', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.squash();
  f.write('src/app.js', 'dirty checkout must not ship');
  const result = f.run({ MERGE_SHA: mergeSha });
  assert.equal(f.remoteTag('v1.10.1'), mergeSha);
  const builds = result.calls.filter(({ command, args }) => command === 'docker' && args[0] === 'build');
  const pushes = result.calls.filter(({ command, args }) => command === 'docker' && args[0] === 'push');
  assert.deepEqual(pushes.map(({ args }) => args[1]), [...applicationImages, postgresImage]);
  assert.equal(builds.length, 3);
  for (const [index, build] of builds.entries()) {
    assert.ok(build.args[build.args.indexOf('--file') + 1].endsWith(`/deploy/compose/${services[index]}.Dockerfile`));
    assert.equal(build.source, 'export const value = 1;\n');
    assert.equal(build.version, '1.10.1');
    assert.deepEqual(build.migrations, [], 'A release without SQL migrations still carries an empty manifest');
    assert.ok(build.args.includes(`org.opencontainers.image.revision=${mergeSha}`));
    assert.ok(build.args.includes(`org.opencontainers.image.source=${source}`));
  }
  // The release is created as a draft, gains both validated descriptions and is
  // only then published.
  const index = (predicate) => result.calls.findIndex(({ args }) => predicate(args));
  const created = index((args) => args[0] === 'release' && args[1] === 'create');
  const uploaded = index((args) => args[0] === 'release' && args[1] === 'upload');
  const edited = index((args) => args[0] === 'release' && args[1] === 'edit');
  assert.ok(created >= 0 && uploaded > created && edited > uploaded);
  assert.ok(result.calls[created].args.includes('--draft'));
  assert.ok(result.calls[edited].args.includes('--draft=false'));
  for (const name of contractNames) assert.equal(f.published(name), f.description(name));
});

// Publishing an unrelated merge must not create a tag or a release.
test('closed pull request that was not merged is rejected', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  f.run({ PR_MERGED: 'false', MERGE_SHA: f.git('rev-parse', 'auto-release') }, false);
  assert.equal(f.remoteTag('v1.10.1'), '');
});

// Same CLI seam: retries must not overwrite images or mutate Release assets.
test('re-run reuses both images, the shared image, the tag, the Release and its assets', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.squash();
  f.run({ MERGE_SHA: mergeSha });
  const rerun = f.run({ MERGE_SHA: mergeSha });
  assert.equal(f.remoteTag('v1.10.1'), mergeSha);
  assert.equal(rerun.calls.filter(({ command, args }) =>
    command === 'docker' ? ['build', 'push'].includes(args[0]) : ['create', 'upload', 'edit'].includes(args[1])).length, 0);
  for (const name of contractNames) assert.equal(f.published(name), f.description(name));
});

// A release may only come from this repository's auto-release branch into main.
for (const [name, overrides, expected] of [
  ['a pull request from another branch', { PR_HEAD_REF: 'feature/release' }, /must be published from the auto-release branch/],
  ['a pull request into another branch', { PR_BASE_REF: 'develop' }, /must be merged into main/],
  ['a fork of the release branch', { PR_HEAD_REPOSITORY: 'attacker/pin-glide-service' }, /must come from BuddyLabsCo\/pin-glide-service/],
]) {
  test(`${name} is rejected`, (t) => {
    const f = fixture(t);
    f.propose('1.10.1');
    const mergeSha = f.squash();
    const result = f.run({ ...overrides, MERGE_SHA: mergeSha }, false);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, expected);
    assert.equal(f.remoteTag('v1.10.1'), '');
    assert.equal(f.calls().filter(({ args }) => args[1] === 'create').length, 0);
  });
}

// The repository disallows merge commits for release pull requests: the merged
// commit must be a squash or rebase result contained in main.
test('a merge commit is rejected', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.merge();
  const result = f.run({ MERGE_SHA: mergeSha }, false);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /squash|rebase|merge commit/i);
  assert.equal(f.remoteTag('v1.10.1'), '');
});

test('a merge commit outside origin/main is rejected', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.commit('src/app.js', 'export const value = 2;\n', 'Unpushed local work');
  const result = f.run({ MERGE_SHA: mergeSha }, false);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /not contained in origin\/main|not in this checkout/);
  assert.equal(f.remoteTag('v1.10.1'), '');
});

// An existing tag is the version ledger: it is reused only for the same commit.
test('an existing tag for another commit is rejected', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.squash();
  const other = f.git('rev-parse', 'HEAD~1');
  f.git('tag', 'v1.10.1', other); f.git('push', 'origin', 'refs/tags/v1.10.1');
  const result = f.run({ MERGE_SHA: mergeSha }, false);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /already points at/);
  assert.equal(f.remoteTag('v1.10.1'), other);
  assert.equal(f.calls().filter(({ args }) => args[1] === 'create').length, 0);
});

// The merged commit must carry the version the release publishes.
for (const [name, version] of [['a malformed version', '1.10'], ['a version that does not increase', '1.10.0']]) {
  test(`${name} is rejected`, (t) => {
    const f = fixture(t);
    f.propose(version, { 'src/app.js': 'export const value = 2;\n' });
    const mergeSha = f.squash();
    const result = f.run({ MERGE_SHA: mergeSha }, false);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /three-part release version|must be greater/);
    assert.equal(f.remoteTag(`v${version}`), '');
  });
}

// Missing or unvalidated contract assets must stop the release before any write.
test('a missing contract asset is refused before any publication', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.squash();
  const result = f.run({ MERGE_SHA: mergeSha, ASYNCAPI_ASSET: join(f.exported, 'absent.yaml') }, false);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /ASYNCAPI_ASSET must point to the exported asyncapi\.yaml description/);
  assert.equal(f.remoteTag('v1.10.1'), '');
  assert.equal(result.calls.filter(({ command, args }) => command === 'docker' || args[0] === 'release').length, 0);
});

// Regression: the workflow validated one description and exported another.
test('a contract that is not the validated description is refused', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.squash();
  f.exportedAsset('openapi.yaml', 'openapi: 3.1.0\ninfo:\n  title: Pin Glide\npaths: {}\n');
  const result = f.run({ MERGE_SHA: mergeSha }, false);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /openapi\.yaml does not match the validated description/);
  assert.equal(f.remoteTag('v1.10.1'), '');
  assert.equal(result.calls.filter(({ command, args }) => command === 'docker' || args[0] === 'release').length, 0);
});

// A failed GitHub call must fail the job, and the pushed tag must not block the retry.
test('a failed release creation fails closed and a retry publishes', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.squash();
  f.configure({ fail: 'release create' });
  const failed = f.run({ MERGE_SHA: mergeSha }, false);
  assert.notEqual(failed.status, 0);
  assert.match(failed.stderr, /simulated gh release create failure/);
  assert.equal(f.remoteTag('v1.10.1'), mergeSha, 'The tag stays published for the retry');
  assert.equal(f.release(), null);
  f.configure({ fail: '' });
  const retry = f.run({ MERGE_SHA: mergeSha });
  assert.equal(retry.calls.filter(({ args }) => args[1] === 'create').length, 1);
  assert.equal(f.release().draft, false);
});

// Regression: a timed-out push already stored an image. Observe the retry
// reusing it, while the externally stored artifact is verified, not rebuilt.
test('partial image publication resumes without rebuilding an acknowledged-lost push', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.squash();
  f.configure({ failAfterPush: applicationImages[0] });
  const failed = f.run({ MERGE_SHA: mergeSha }, false);
  assert.notEqual(failed.status, 0);
  assert.equal(failed.calls.filter(({ args }) => args[0] === 'release').length, 0);
  f.configure({ failAfterPush: '' });
  const retry = f.run({ MERGE_SHA: mergeSha });
  assert.deepEqual(retry.calls.filter(({ command, args }) => command === 'docker' && args[0] === 'push').map(({ args }) => args[1]),
    [applicationImages[1], postgresImage]);
});

// Regression: leaking private deployments or mistaking authorization failures
// for missing artifacts. The CLI must refuse before any Docker write/release.
for (const [name, configuration, expected] of [
  ['a public existing package', { packages: { 'pin-glide-api': { visibility: 'public' } } }, /must be private/],
  ['a registry permission failure', { fail: 'package access' }, /Command failed: gh/],
]) {
  test(`${name} prevents publication`, (t) => {
    const f = fixture(t);
    f.propose('1.10.1');
    const mergeSha = f.squash();
    f.configure(configuration);
    const result = f.run({ MERGE_SHA: mergeSha }, false);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, expected);
    assert.equal(result.calls.filter(({ command, args }) => command === 'docker' || args[0] === 'release').length, 0);
  });
}

// Regression: an organization default or out-of-band change exposes a newly
// created package. Only the post-push API observation can protect this path.
test('a newly published non-private package cannot complete the release', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.squash();
  f.configure({ newVisibility: 'public' });
  const result = f.run({ MERGE_SHA: mergeSha }, false);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /must be private/);
  assert.equal(result.calls.filter(({ args }) => args[0] === 'release').length, 0);
});

// Regression: same-version images were published from a different commit.
// Tag validation alone cannot detect image content; the CLI must not overwrite.
test('an existing image from another commit is rejected without overwrite', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.squash();
  f.configure({ packages: { 'pin-glide-api': { visibility: 'private' } }, images: {
    [applicationImages[0]]: { 'org.opencontainers.image.revision': f.git('rev-parse', 'HEAD^'), 'org.opencontainers.image.source': source },
  } });
  const result = f.run({ MERGE_SHA: mergeSha }, false);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /does not match release commit/);
  assert.equal(result.calls.filter(({ command, args }) => command === 'docker' ? args[0] === 'build' : args[0] === 'release').length, 0);
});

// Regression: an image tag exists but came from another repository.
test('an existing image from another repository is rejected without overwrite', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.squash();
  f.configure({ packages: { 'pin-glide-api': { visibility: 'private' } }, images: {
    [applicationImages[0]]: { 'org.opencontainers.image.revision': mergeSha, 'org.opencontainers.image.source': 'https://github.com/BuddyLabsCo/helm-gateway' },
  } });
  const result = f.run({ MERGE_SHA: mergeSha }, false);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /was not published from https:\/\/github\.com\/BuddyLabsCo\/pin-glide-service/);
  assert.equal(result.calls.filter(({ command, args }) => command === 'docker' ? args[0] === 'build' : args[0] === 'release').length, 0);
});

// Regression: upload succeeded but one image is not pullable. Existing
// push-success checks cannot establish deployability of all three images.
test('release remains unpublished until every image can be pulled', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.squash();
  f.configure({ fail: `pull ${postgresImage}` });
  const result = f.run({ MERGE_SHA: mergeSha }, false);
  assert.notEqual(result.status, 0);
  assert.equal(result.calls.filter(({ command, args }) => command === 'docker' && args[0] === 'push').length, 3);
  assert.equal(result.calls.filter(({ args }) => args[0] === 'release').length, 0);
  f.configure({ fail: '' });
  const retry = f.run({ MERGE_SHA: mergeSha });
  assert.equal(retry.calls.filter(({ command, args }) => command === 'docker' && ['build', 'push'].includes(args[0])).length, 0);
  assert.equal(f.release().draft, false);
});

// The Release is published only after both validated descriptions are attached,
// so an interrupted run leaves a draft that a retry completes.
test('an interrupted publication stays a draft and a retry attaches the missing asset', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.squash();
  f.configure({ fail: 'release upload asyncapi.yaml' });
  const failed = f.run({ MERGE_SHA: mergeSha }, false);
  assert.notEqual(failed.status, 0);
  assert.equal(f.release().draft, true, 'A partial publication must stay unpublished');
  assert.deepEqual(Object.keys(f.release().assets), ['openapi.yaml']);
  f.configure({ fail: '' });
  const retry = f.run({ MERGE_SHA: mergeSha });
  assert.deepEqual(retry.calls.filter(({ args }) => args[0] === 'release' && args[1] === 'upload').map(({ args }) => args[3].split('/').at(-1)),
    ['asyncapi.yaml']);
  assert.deepEqual(Object.keys(f.release().assets), contractNames);
  assert.equal(f.release().draft, false);
});

// A retry after the publish step failed reuses the draft and its matching assets.
test('a failed publish step is resumed from the matching draft assets', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.squash();
  f.configure({ fail: 'release edit' });
  const failed = f.run({ MERGE_SHA: mergeSha }, false);
  assert.notEqual(failed.status, 0);
  assert.equal(f.release().draft, true);
  assert.deepEqual(Object.keys(f.release().assets), contractNames);
  f.configure({ fail: '' });
  const retry = f.run({ MERGE_SHA: mergeSha });
  assert.equal(retry.calls.filter(({ args }) => args[0] === 'release' && ['create', 'upload'].includes(args[1])).length, 0);
  assert.equal(f.release().draft, false);
});

// Regression: a published Release carries a different asset than the validated
// description. Matching immutable assets are reused; conflicting ones fail.
test('an already published Release with different asset contents is rejected', (t) => {
  const f = fixture(t);
  f.propose('1.10.1');
  const mergeSha = f.squash();
  f.run({ MERGE_SHA: mergeSha });
  f.tamper('openapi.yaml', 'openapi: 3.1.0\ntampered: true\n');
  const result = f.run({ MERGE_SHA: mergeSha }, false);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /openapi\.yaml is already published with different contents/);
  assert.equal(result.calls.filter(({ args }) => args[0] === 'release' && ['create', 'upload', 'edit'].includes(args[1])).length, 0);
  assert.equal(f.published('openapi.yaml'), 'openapi: 3.1.0\ntampered: true\n');
});

// Regression: exported images lack Git history and lexical sorting can reorder
// migrations. Observe the build input through the publishing CLI, not helpers.
test('release images carry first-introduction migration order including merge commits', (t) => {
  const f = fixture(t);
  f.write('db/migrations/pin_glide/issue_98_2_second.sql', 'SELECT 2;\n');
  f.commit('db/migrations/pin_glide/issue_98_1_first.sql', 'SELECT 1;\n', 'Introduce migration batch');
  f.git('checkout', '-b', 'migration-change');
  f.write('db/migrations/pin_glide/issue_7_later.sql', 'SELECT 3;\n');
  f.git('add', '.'); f.git('commit', '-m', 'Add migration on feature branch');
  f.git('checkout', 'main');
  f.git('merge', '--no-ff', 'migration-change', '-m', 'Merge application change');
  f.git('rm', 'db/migrations/pin_glide/issue_98_1_first.sql');
  f.git('commit', '-m', 'Temporarily revert an unreleased migration');
  f.commit('db/migrations/pin_glide/issue_98_1_first.sql', 'SELECT 1;\n', 'Restore the migration');
  f.write('db/migrations/release/issue_98_1_first.sql', 'SELECT 4;\n');
  f.commit('db/scripts/issue_98_1_first.sql', 'SELECT 5;\n', 'Add release and data scripts');
  f.propose('1.10.1');
  const mergeSha = f.squash();
  f.write('db/scripts/issue_99_uncommitted.sql', 'SELECT 6;\n');
  const result = f.run({ MERGE_SHA: mergeSha });
  const builds = result.calls.filter(({ command, args }) => command === 'docker' && args[0] === 'build');
  assert.equal(builds.length, 3);
  for (const build of builds) assert.deepEqual(build.migrations, [
    'db/migrations/pin_glide/issue_98_1_first.sql', 'db/migrations/pin_glide/issue_98_2_second.sql',
    'db/migrations/pin_glide/issue_7_later.sql', 'db/migrations/release/issue_98_1_first.sql',
    'db/scripts/issue_98_1_first.sql',
  ]);
  assert.equal(existsSync(join(f.cwd, 'db/migrations/order.txt')), false, 'The generated manifest must not dirty the checkout');
});

// Regression: removed or archive-excluded SQL makes the migration manifest
// incomplete/inconsistent. The public publisher must fail before any image push.
for (const change of ['delete', 'exclude from archive']) {
  test(`an inconsistent migration manifest fails publication: ${change}`, (t) => {
    const f = fixture(t);
    f.commit('db/migrations/pin_glide/issue_98_repair.sql', 'SELECT 1;\n', 'Add an application migration');
    if (change === 'delete') {
      f.git('rm', 'db/migrations/pin_glide/issue_98_repair.sql');
      f.git('commit', '-m', 'Remove a migration');
    } else {
      f.commit('.gitattributes', 'db/migrations/pin_glide/issue_98_repair.sql export-ignore\n', 'Exclude a migration');
    }
    f.propose('1.10.1');
    const result = f.run({ MERGE_SHA: f.squash() }, false);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /migration.*manifest.*inconsistent/i);
    assert.equal(result.calls.filter(({ command, args }) => command === 'docker' || args[0] === 'release').length, 0);
  });
}
