import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

const script = fileURLToPath(new URL('./auto-release-pr.mjs', import.meta.url));
const versionFile = 'version.json';

// All cases exercise the public CLI, real Git history and remote refs. Only the
// external GitHub boundary is faked; no existing test covers this release flow.
function fixture(t, tagged = true) {
  const root = mkdtempSync(join(tmpdir(), 'pin-glide-db-tools-test-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const cwd = join(root, 'repo'), origin = join(root, 'origin.git');
  const bin = join(root, 'bin'), state = join(root, 'gh-state.json'), log = join(root, 'gh-log.jsonl');
  for (const path of [cwd, bin]) mkdirSync(path);
  const env = { ...process.env, HOME: root, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1',
    GIT_TERMINAL_PROMPT: '0', PATH: `${bin}:${process.env.PATH}`, GH_TOKEN: 'test-token',
    RELEASE_TEST_STATE: state, RELEASE_TEST_LOG: log };
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
  writeFileSync(state, JSON.stringify({ open: '', fail: '' }));
  writeFileSync(log, '');
  writeFileSync(join(bin, 'gh'), String.raw`#!/usr/bin/env node
const fs = require('node:fs');
const args = process.argv.slice(2), state = JSON.parse(fs.readFileSync(process.env.RELEASE_TEST_STATE, 'utf8'));
const flag = name => args[args.indexOf(name) + 1];
const body = args.includes('--body-file') ? fs.readFileSync(flag('--body-file'), 'utf8') : null;
fs.appendFileSync(process.env.RELEASE_TEST_LOG, JSON.stringify({ args, body }) + '\n');
if (state.fail === args[1]) { console.error('simulated gh ' + args[1] + ' failure'); process.exit(42); }
if (args[0] !== 'pr') process.exit(90);
if (args[1] === 'list') {
  if (flag('--base') !== 'main' || flag('--head') !== 'auto-release' || flag('--state') !== 'open' || flag('--json') !== 'number' || !args.includes('--jq')) process.exit(91);
  console.log(state.open);
} else if (args[1] === 'create') {
  if (state.open || flag('--base') !== 'main' || flag('--head') !== 'auto-release' || body === null) process.exit(92);
  state.open = '121'; fs.writeFileSync(process.env.RELEASE_TEST_STATE, JSON.stringify(state));
  console.log('https://github.com/BuddyLabsCo/pin-glide-service/pull/121');
} else if (args[1] === 'edit') {
  if (!state.open || args[2] !== state.open || body === null) process.exit(93);
} else process.exit(94);
`);
  chmodSync(join(bin, 'gh'), 0o755);
  git('init', '--bare', origin);
  git('init', '-b', 'main');
  git('config', 'user.name', 'Release Test');
  git('config', 'user.email', 'release-test@example.invalid');
  git('config', 'commit.gpgsign', 'false');
  git('config', 'core.hooksPath', '/dev/null');
  git('remote', 'add', 'origin', origin);
  versions('1.10.0');
  write('src/app.js', 'export const value = 1;\n');
  git('add', '.'); git('commit', '-m', 'Initial implementation');
  if (tagged) {
    git('tag', 'v1.9.9');
    write('README.md', 'Released baseline\n');
    git('add', '.'); git('commit', '-m', 'Released baseline'); git('tag', 'v1.10.0');
  }
  git('push', '-u', 'origin', 'main', '--tags');
  const calls = () => readFileSync(log, 'utf8').trim().split('\n').filter(Boolean).map(JSON.parse);
  const configure = (patch) => writeFileSync(state, JSON.stringify({ ...JSON.parse(readFileSync(state, 'utf8')), ...patch }));
  const commit = (path, content, title) => {
    git('checkout', 'main'); const before = git('rev-parse', 'HEAD');
    write(path, content); git('add', '.'); git('commit', '-m', title); git('push', 'origin', 'main');
    return before;
  };
  const run = (before, success = true) => {
    const start = calls().length;
    const result = spawnSync(process.execPath, [script], { cwd, env: { ...env, PUSH_BEFORE: before }, encoding: 'utf8', timeout: 20000 });
    const invoked = calls().slice(start);
    assert.ok(!calls().some(({ args }) => args[0] === 'release'), 'Must never invoke gh release');
    assert.equal(result.error, undefined, 'CLI must finish without a timeout or spawn error');
    if (success) assert.equal(result.status, 0, result.stderr);
    return { ...result, calls: invoked };
  };
  const remote = () => git('ls-remote', '--heads', 'origin', 'auto-release');
  const proposed = (version) => {
    git('checkout', '-b', 'auto-release'); versions(version);
    git('add', '.'); git('commit', '--allow-empty', '-m', 'Maintainer release proposal');
    git('push', 'origin', 'auto-release'); git('checkout', 'main'); configure({ open: '121' });
  };
  const checkVersion = (version) => {
    git('fetch', 'origin', 'auto-release');
    assert.equal(JSON.parse(git('show', `origin/auto-release:${versionFile}`)).version, version);
  };
  return { git, write, versions, configure, commit, run, remote, proposed, checkVersion, calls };
}

function mutation(result, action, titles) {
  const writes = result.calls.filter(({ args }) => ['create', 'edit'].includes(args[1]));
  assert.equal(writes.length, 1, 'Exactly one PR mutation is required');
  assert.equal(writes[0].args[1], action);
  for (const title of titles) assert.ok(writes[0].body.includes(title), `Changelog must include: ${title}`);
  return writes[0].body;
}

// Catches the original skip bug and phantom releases through successive pushes.
test('tagged release -> docs skip -> code creates -> docs refresh existing PR', (t) => {
  const f = fixture(t);
  for (const [path, title] of [['guide.md', 'Document setup'], ['.github/check.yml', 'Tune checks']]) {
    const result = f.run(f.commit(path, title, title));
    assert.equal(result.calls.filter(({ args }) => ['create', 'edit'].includes(args[1])).length, 0);
    assert.equal(f.remote(), '');
    assert.equal(f.git('branch', '--list', 'auto-release'), '');
  }
  const before = f.commit('src/app.js', 'export const value = 2;\n', 'Fix request routing');
  const main = f.git('rev-parse', 'HEAD');
  const body = mutation(f.run(before), 'create', ['Document setup', 'Tune checks', 'Fix request routing']);
  assert.ok(!body.includes('Initial implementation') && !body.includes('Released baseline'), 'Use the highest version tag, not lexical order');
  assert.match(body, /squash or rebase/, 'The proposal must state the accepted merge method for publishing');
  assert.match(body, /both validated contract assets/, 'The proposal must name what merging publishes');
  f.checkVersion('1.10.1');
  assert.equal(f.git('rev-parse', 'origin/main'), main);
  assert.deepEqual(f.git('diff', '--name-only', 'origin/main', 'origin/auto-release').split('\n'), [versionFile]);
  for (const [path, title] of [['guide.md', 'Document the routing fix'], ['.github/check.yml', 'Document CI behavior']]) {
    mutation(f.run(f.commit(path, title, title)), 'edit', ['Fix request routing', title]);
    f.checkVersion('1.10.1');
  }
  assert.equal(f.calls().filter(({ args }) => args[1] === 'create').length, 1);
});

// The merge of a release proposal is not release-eligible either: that push
// carries only the version file, so it must not propose the next release.
test('a push that only changes the release version file proposes nothing', (t) => {
  const f = fixture(t);
  const before = f.commit(versionFile, '{\n  "version": "1.10.1"\n}\n', 'chore: propose release v1.10.1');
  const result = f.run(before);
  assert.equal(result.calls.filter(({ args }) => ['create', 'edit'].includes(args[1])).length, 0);
  assert.equal(f.remote(), '');
  assert.equal(f.git('branch', '--list', 'auto-release'), '');
});

// A push is a range, not just its final (possibly documentation-only) commit.
test('multi-commit push considers earlier code changes', (t) => {
  const f = fixture(t);
  const before = f.commit('src/app.js', 'export const value = 3;\n', 'Fix first commit');
  f.commit('guide.md', 'Explain fix', 'Explain last commit');
  mutation(f.run(before), 'create', ['Fix first commit', 'Explain last commit']);
  f.checkVersion('1.10.1');
});

// New branches have no before object; untagged history must still be complete.
test('zero before and no tags use the full history, excluding merge titles', (t) => {
  const f = fixture(t, false);
  f.git('checkout', '-b', 'topic'); f.write('feature.js', 'export const feature = true;\n');
  f.git('add', '.'); f.git('commit', '-m', 'Add topic feature'); f.git('checkout', 'main');
  f.git('merge', '--no-ff', 'topic', '-m', 'Merge title must not appear'); f.git('push', 'origin', 'main');
  const body = mutation(f.run('0'.repeat(40)), 'create', ['Initial implementation', 'Add topic feature']);
  assert.ok(!body.includes('Merge title must not appear'));
  f.checkVersion('1.10.1');
});

// Rename detection must not hide a deleted code path behind a markdown suffix.
test('renaming code to markdown is release-eligible', (t) => {
  const f = fixture(t), before = f.git('rev-parse', 'HEAD');
  f.git('mv', 'src/app.js', 'retired.md'); f.git('commit', '-m', 'Retire executable code'); f.git('push', 'origin', 'main');
  mutation(f.run(before), 'create', ['Retire executable code']); f.checkVersion('1.10.1');
});

// Protect maintainer intent and PR updates independent of staged version diffs.
for (const version of ['2.0.0', '1.11.0', '1.10.0']) {
  test(`existing PR preserves ${version} and refreshes even without version changes`, (t) => {
    const f = fixture(t); f.proposed(version);
    mutation(f.run(f.commit('fix.js', 'export const fixed = true;\n', 'Fix after proposal')), 'edit', ['Fix after proposal']);
    f.checkVersion(version);
    mutation(f.run(f.commit('guide.md', 'Updated guide', 'Explain after proposal')), 'edit', ['Fix after proposal', 'Explain after proposal']);
    f.checkVersion(version);
    assert.equal(f.calls().filter(({ args }) => args[1] === 'create').length, 0);
  });
}

// Fail closed: malformed versions, missing remote proposals and GitHub failures
// must surface as errors rather than silently inventing a replacement PR.
for (const fault of ['current version', 'proposed version', 'missing branch', 'gh list', 'gh create', 'gh edit']) {
  test(`explicit failure for ${fault}, without a replacement PR`, (t) => {
    const f = fixture(t);
    if (fault === 'proposed version') f.proposed('2.invalid.0');
    if (fault === 'gh edit') f.proposed('2.0.0');
    if (fault === 'missing branch') f.configure({ open: '121' });
    if (fault.startsWith('gh ')) f.configure({ fail: fault.slice(3) });
    const before = f.git('rev-parse', 'HEAD');
    if (fault === 'current version') f.versions('1.10');
    f.commit('fix.js', 'export const fixed = true;\n', 'Trigger guarded release');
    const remoteBefore = f.remote(), result = f.run(before, false);
    assert.notEqual(result.status, 0, 'Invalid release state must fail explicitly');
    const expected = fault.startsWith('gh ') ? new RegExp(`simulated ${fault} failure`)
      : fault === 'missing branch' ? /couldn't find remote ref|missing.*auto-release|not found/i
        : /three-part release version|invalid.*version|malformed.*version/i;
    assert.match(result.stderr, expected);
    assert.equal(result.calls.filter(({ args }) => args[1] === 'create').length, fault === 'gh create' ? 1 : 0);
    if (!fault.startsWith('gh ') || fault === 'gh list') assert.equal(f.remote(), remoteBefore);
  });
}
