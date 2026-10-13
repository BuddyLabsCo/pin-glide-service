import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import { repoRoot, toolFixture } from './tool-fixture.mjs';

const pinned = JSON.parse(readFileSync(join(repoRoot, 'package.json'), 'utf8')).devDependencies;

const fixture = (t) => toolFixture(t, {
  copy: [
    'package.json',
    'scripts/quality/check-contracts.sh',
    'test/fixtures/contract/violating_openapi.yaml',
    'test/fixtures/contract/violating_asyncapi.yaml',
  ],
  files: {
    'node_modules/@redocly/cli/package.json': JSON.stringify({ name: '@redocly/cli', version: pinned['@redocly/cli'] }),
    'node_modules/@asyncapi/cli/package.json': JSON.stringify({ name: '@asyncapi/cli', version: pinned['@asyncapi/cli'] }),
  },
  tools: ['go', 'npx'],
});

const run = (f, extra = {}) => f.run('bash', ['scripts/quality/check-contracts.sh'], extra);

test('the contract check lints both offline exports and proves each validator rejects its violating fixture', (t) => {
  const f = fixture(t), result = run(f);
  assert.equal(result.status, 0, result.stderr);

  const exports = f.calls('go').filter((call) => call.args[0] === 'run');
  assert.deepEqual(exports.map((call) => call.args), [
    ['run', './cmd/pin-glide-api', '-openapi'],
    ['run', './cmd/pin-glide-api', '-asyncapi'],
  ], result.stderr);

  const validators = f.calls('npx').map((call) => call.args);
  assert.deepEqual(validators.map((args) => args.slice(0, 3)), [
    ['--no-install', 'redocly', 'lint'],
    ['--no-install', 'asyncapi', 'validate'],
    ['--no-install', 'redocly', 'lint'],
    ['--no-install', 'asyncapi', 'validate'],
  ], result.stderr);
  assert.match(validators[0].at(-1), /openapi\.yaml$/, result.stderr);
  assert.ok(!validators[0].at(-1).includes('fixtures'), result.stderr);
  assert.match(validators[1].find((arg) => arg.endsWith('.yaml')), /asyncapi\.yaml$/, result.stderr);
  assert.ok(validators[1].includes('--fail-severity=error'), result.stderr);
  assert.match(validators[2].at(-1), /violating_openapi\.yaml$/, result.stderr);
  assert.match(validators[3].find((arg) => arg.endsWith('.yaml')), /violating_asyncapi\.yaml$/, result.stderr);

  const contractTests = f.calls('go').find((call) => call.args[0] === 'test');
  assert.deepEqual(contractTests.args, ['test', './internal/api/wss/...', './test/contract/...'], result.stderr);
});

test('the contract check fails when a validator accepts the violating fixture', (t) => {
  const f = fixture(t), result = run(f, { TOOL_ACCEPT_FIXTURE: '1' });
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /the OpenAPI validator accepted the violating fixture\./);
});

test('the contract check fails when a validator fails without reporting the violation', (t) => {
  const f = fixture(t), result = run(f, { TOOL_SILENT_FIXTURE_FAILURE: '1' });
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /the OpenAPI validator failed without reporting no-unresolved-refs\./);
});

test('the contract check stops on a validator that does not match the pin', (t) => {
  const f = fixture(t);
  f.run('bash', ['-c', "printf '{\"name\":\"@redocly/cli\",\"version\":\"0.0.1\"}' > node_modules/@redocly/cli/package.json"]);

  const result = run(f);
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, new RegExp(`@redocly/cli ${pinned['@redocly/cli']} is required; found 0\\.0\\.1\\.`));
  assert.equal(f.calls('go').length, 0, result.stderr);
});
