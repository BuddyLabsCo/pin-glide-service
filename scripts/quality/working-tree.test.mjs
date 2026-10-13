import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, unlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

// Moving scripts without staging must not make checks read deleted index paths
// or silently skip new files. Exercise the real shell entrypoints and Git index.
test('quality checks inspect existing tracked and untracked working-tree files', (t) => {
  const cwd = mkdtempSync(join(tmpdir(), 'pin-glide-quality-'));
  t.after(() => rmSync(cwd, { recursive: true, force: true }));
  for (const dir of ['scripts/quality', 'scripts/dev']) mkdirSync(join(cwd, dir), { recursive: true });
  const env = { ...process.env };
  for (const key of ['GIT_DIR', 'GIT_WORK_TREE', 'GIT_INDEX_FILE', 'GIT_COMMON_DIR']) delete env[key];
  const run = (command, args) => spawnSync(command, args, { cwd, env, encoding: 'utf8' });
  assert.equal(run('git', ['init', '--quiet']).status, 0);
  writeFileSync(join(cwd, 'removed.go'), 'package removed\n');
  assert.equal(run('git', ['add', 'removed.go']).status, 0);
  unlinkSync(join(cwd, 'removed.go'));
  for (const name of ['format.sh', 'check-file-size.sh']) {
    copyFileSync(fileURLToPath(new URL(`./${name}`, import.meta.url)), join(cwd, 'scripts/quality', name));
  }
  writeFileSync(join(cwd, 'new.go'), 'package example\nfunc value( )int{return 1}\n');
  const unformatted = run('bash', ['scripts/quality/format.sh', '--check']);
  assert.equal(unformatted.status, 1, unformatted.stderr);
  assert.match(unformatted.stderr, /new\.go/);
  assert.doesNotMatch(unformatted.stderr, /removed\.go/);
  const formatted = run('bash', ['scripts/quality/format.sh']);
  assert.equal(formatted.status, 0, formatted.stderr);
  assert.equal(run('bash', ['scripts/quality/format.sh', '--check']).status, 0);
  writeFileSync(join(cwd, 'scripts/dev/oversized.sh'), '# line\n'.repeat(501));
  const oversized = run('bash', ['scripts/quality/check-file-size.sh']);
  assert.equal(oversized.status, 1, oversized.stderr);
  assert.match(oversized.stderr, /scripts\/dev\/oversized\.sh: 501 physical lines/);
  unlinkSync(join(cwd, 'scripts/dev/oversized.sh'));
  const valid = run('bash', ['scripts/quality/check-file-size.sh']);
  assert.equal(valid.status, 0, valid.stderr);
});
