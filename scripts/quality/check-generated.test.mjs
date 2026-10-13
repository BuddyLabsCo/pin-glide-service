import assert from 'node:assert/strict';
import { appendFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import { toolFixture } from './tool-fixture.mjs';

// The drift check runs against a real Git repository, so a clean checkout,
// a changed artifact and an uncommitted artifact are the actual conditions the
// check has to tell apart.
const fixture = (t) => {
  const f = toolFixture(t, {
    copy: ['scripts/quality/check-generated.sh'],
    files: { 'generated/protocol/openapi.yaml': '# committed description\n' },
  });
  for (const args of [
    ['init', '-q'],
    ['config', 'user.email', 'fixture@example.com'],
    ['config', 'user.name', 'Fixture'],
    ['add', '.'],
    ['commit', '-qm', 'committed artifacts'],
  ]) {
    const result = f.run('git', args);
    assert.equal(result.status, 0, result.stderr);
  }
  return f;
};

const run = (f) => f.run('bash', ['scripts/quality/check-generated.sh']);

test('the drift check passes a working tree that still matches the commit', (t) => {
  const result = run(fixture(t));
  assert.equal(result.status, 0, result.stderr);
});

test('the drift check fails when regeneration changed a committed artifact', (t) => {
  const f = fixture(t);
  appendFileSync(join(f.cwd, 'generated/protocol/openapi.yaml'), '# stale\n');

  const result = run(f);
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /generated artifacts drifted from the committed output:/);
  assert.match(result.stderr, /generated\/protocol\/openapi\.yaml/);
});

test('the drift check fails when regeneration produced an uncommitted artifact', (t) => {
  const f = fixture(t);
  writeFileSync(join(f.cwd, 'generated/protocol/asyncapi.yaml'), '# uncommitted\n');

  const result = run(f);
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /generation produced untracked output:/);
  assert.match(result.stderr, /generated\/protocol\/asyncapi\.yaml/);
});
