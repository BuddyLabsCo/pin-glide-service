import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

const script = fileURLToPath(new URL('./migration-order.sh', import.meta.url));

function repository(t) {
  const cwd = mkdtempSync(join(tmpdir(), 'pin-glide-migration-order-'));
  t.after(() => rmSync(cwd, { recursive: true, force: true }));
  const env = { ...process.env, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' };
  for (const key of ['GIT_DIR', 'GIT_WORK_TREE', 'GIT_INDEX_FILE', 'GIT_COMMON_DIR']) delete env[key];
  const git = (...args) => {
    const result = spawnSync('git', args, { cwd, env, encoding: 'utf8' });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim();
  };
  const write = (path) => {
    mkdirSync(dirname(join(cwd, path)), { recursive: true });
    writeFileSync(join(cwd, path), 'SELECT 1;\n');
  };
  const commit = () => { git('add', '.'); git('commit', '--allow-empty', '-m', 'Add scripts'); };
  git('init', '-b', 'main');
  git('config', 'user.name', 'Migration Order Test');
  git('config', 'user.email', 'migration-order@example.invalid');
  git('config', 'commit.gpgsign', 'false');
  git('config', 'core.hooksPath', '/dev/null');
  return { cwd, git, write, commit, order: (...args) => spawnSync('bash', [script, ...args], { cwd, env, encoding: 'utf8' }) };
}

// A runtime image has no Git history: its three filtered streams must retain
// introduction order, even when filenames sort differently or overlap.
test('orders all streams by introduction and appends only local uncommitted scripts', (t) => {
  const f = repository(t);
  const first = ['db/migrations/pin_glide/issue_98_1_first.sql', 'db/migrations/pin_glide/issue_98_2_second.sql',
    'db/migrations/release/issue_98_1_first.sql', 'db/scripts/issue_98_1_first.sql'];
  for (const path of [...first].reverse()) f.write(path);
  f.commit();
  f.git('checkout', '-b', 'feature');
  const later = 'db/migrations/pin_glide/issue_7_later.sql';
  f.write(later); f.commit();
  f.git('checkout', 'main');
  f.git('merge', '--no-ff', 'feature', '-m', 'Merge scripts');
  const local = ['db/migrations/release/issue_1_local.sql', 'db/scripts/issue_2_local.sql'];
  for (const path of [...local].reverse()) f.write(path);
  const result = f.order();
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.stdout.trim().split('\n'), [...first, later, ...local]);
  const strict = f.order('HEAD');
  assert.notEqual(strict.status, 0, 'A release commit must not acquire uncommitted SQL');
});

test('rejects SQL outside the supported migration streams', (t) => {
  const f = repository(t);
  f.write('db/migrations/issue_98_flat.sql'); f.commit();
  const result = f.order('HEAD');
  assert.notEqual(result.status, 0, 'A flat migration belongs to no schema stream');
  assert.match(result.stderr, /invalid path/);
});

test('removing historical SQL does not silently change the manifest', (t) => {
  const f = repository(t);
  f.write('db/migrations/pin_glide/issue_98_first.sql'); f.commit();
  f.write('db/migrations/release/issue_1_later.sql'); f.commit();
  const result = f.order('HEAD');
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.stdout.trim().split('\n'), [
    'db/migrations/pin_glide/issue_98_first.sql', 'db/migrations/release/issue_1_later.sql',
  ]);
  f.git('rm', 'db/migrations/pin_glide/issue_98_first.sql'); f.commit();
  assert.notEqual(f.order().status, 0, 'Removing historical SQL must not silently change the manifest');
});
