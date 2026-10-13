import assert from 'node:assert/strict';
import test from 'node:test';
import { toolFixture } from './tool-fixture.mjs';

// The wrapper must refuse any golangci-lint that does not match the pinned
// version, so a local or CI installation cannot run a different linter set.
const fixture = (t) => toolFixture(t, { copy: ['scripts/quality/golangci-lint.sh'], tools: ['golangci-lint'] });

test('the wrapper rejects a golangci-lint that does not match the pinned version', (t) => {
  const f = fixture(t), result = f.run('bash', ['scripts/quality/golangci-lint.sh'], { TOOL_VERSION: '2.13.0' });
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /golangci-lint 2\.14\.0 is required; found 2\.13\.0\./);
  assert.deepEqual(f.calls('golangci-lint').map((call) => call.args), [['version']]);
});

test('the wrapper reports a missing golangci-lint and refuses to lint', (t) => {
  const f = fixture(t), result = f.run('bash', ['scripts/quality/golangci-lint.sh'], { GOLANGCI_LINT_BIN: 'golangci-lint-absent' });
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /golangci-lint 2\.14\.0 is required but was not found on PATH\./);
  assert.deepEqual(f.calls('golangci-lint'), []);
});

test('the wrapper runs the pinned golangci-lint over every package', (t) => {
  const f = fixture(t), result = f.run('bash', ['scripts/quality/golangci-lint.sh'], { TOOL_VERSION: '2.14.0' });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(f.calls('golangci-lint').map((call) => call.args), [['version'], ['run', './...']]);
});
