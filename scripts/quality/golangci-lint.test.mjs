import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

// The wrapper must refuse any golangci-lint that does not match the pinned
// version, so a local or CI installation cannot run a different linter set.
const lintCLI = String.raw`#!/usr/bin/env node
const fs = require('node:fs');
const args = process.argv.slice(2);
fs.appendFileSync(process.env.TOOL_LOG, JSON.stringify(args) + '\n');
if (args[0] === 'version') {
  console.log('golangci-lint has version ' + (process.env.TOOL_VERSION ?? '2.14.0') + ' built with go1.27.1 from (unknown)');
}
`;

function fixture(t) {
  const cwd = mkdtempSync(join(tmpdir(), 'pin-glide-golangci-'));
  t.after(() => rmSync(cwd, { recursive: true, force: true }));
  const script = join(cwd, 'scripts/quality/golangci-lint.sh');
  mkdirSync(dirname(script), { recursive: true });
  copyFileSync(fileURLToPath(new URL('./golangci-lint.sh', import.meta.url)), script);
  mkdirSync(join(cwd, 'bin'), { recursive: true });
  writeFileSync(join(cwd, 'bin/golangci-lint'), lintCLI);
  chmodSync(join(cwd, 'bin/golangci-lint'), 0o755);
  writeFileSync(join(cwd, 'commands.jsonl'), '');
  const env = { ...process.env, PATH: `${join(cwd, 'bin')}:${process.env.PATH}`, TOOL_LOG: join(cwd, 'commands.jsonl') };
  return {
    cwd,
    run: (extra = {}) => spawnSync('bash', ['scripts/quality/golangci-lint.sh'], {
      cwd, env: { ...env, ...extra }, encoding: 'utf8', timeout: 15000,
    }),
    calls: () => readFileSync(join(cwd, 'commands.jsonl'), 'utf8').trim().split('\n').filter(Boolean).map(JSON.parse),
  };
}

test('the wrapper rejects a golangci-lint that does not match the pinned version', (t) => {
  const f = fixture(t), result = f.run({ TOOL_VERSION: '2.13.0' });
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /golangci-lint 2\.14\.0 is required; found 2\.13\.0\./);
  assert.deepEqual(f.calls(), [['version']]);
});

test('the wrapper reports a missing golangci-lint and refuses to lint', (t) => {
  const f = fixture(t), result = f.run({ GOLANGCI_LINT_BIN: 'golangci-lint-absent' });
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /golangci-lint 2\.14\.0 is required but was not found on PATH\./);
  assert.deepEqual(f.calls(), []);
});

test('the wrapper runs the pinned golangci-lint over every package', (t) => {
  const f = fixture(t), result = f.run();
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(f.calls(), [['version'], ['run', './...']]);
});
