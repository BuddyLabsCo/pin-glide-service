import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import { toolFixture } from '../quality/tool-fixture.mjs';

const fixture = (t) => toolFixture(t, {
  copy: ['scripts/codegen/generate.sh', 'scripts/db/psqldef.sh', 'scripts/db/postgres.sh', 'scripts/db/postgres-image.sh'],
  files: {
    'db/schema/release/00_extensions.sql': 'CREATE SCHEMA release;\n',
    'db/schema/pin_glide/00_extensions.sql': 'CREATE SCHEMA pin_glide;\n',
  },
  tools: ['docker', 'go'],
});

const jetgen = (f, schema) => join(f.cwd, 'internal/db/jetgen', schema);

test('generation installs the schemas Jet produced and publishes both offline descriptions', (t) => {
  const f = fixture(t), result = f.run('bash', ['scripts/codegen/generate.sh'], { TOOL_JET_SCHEMAS: 'release,pin_glide' });
  assert.equal(result.status, 0, result.stderr);

  assert.match(readFileSync(join(jetgen(f, 'release'), 'table/receivers.go'), 'utf8'), /package table/);
  assert.match(readFileSync(join(jetgen(f, 'pin_glide'), 'table/receivers.go'), 'utf8'), /package table/);

  const exports = f.calls('go').filter((call) => call.args[0] === 'run');
  assert.deepEqual(exports.map((call) => call.args), [
    ['run', './cmd/pin-glide-api', '-openapi'],
    ['run', './cmd/pin-glide-api', '-asyncapi'],
  ], result.stderr);
  for (const mode of ['openapi', 'asyncapi']) {
    assert.match(readFileSync(join(f.cwd, 'generated/protocol', `${mode}.yaml`), 'utf8'), new RegExp(`-${mode}`));
  }

  const jet = f.calls('go').filter((call) => call.args[1] === 'jet');
  assert.deepEqual(jet.map((call) => call.args.at(-2)), ['-schema=release', '-schema=pin_glide']);
  assert.deepEqual(f.calls('docker').at(-1).args, ['rm', '-f', 'disposable-postgres']);
});

test('generation follows Jet between empty and populated schemas without retaining stale output', (t) => {
  const f = fixture(t);

  let result = f.run('bash', ['scripts/codegen/generate.sh']);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(existsSync(jetgen(f, 'release')), false, 'an empty schema must not be installed');

  result = f.run('bash', ['scripts/codegen/generate.sh'], { TOOL_JET_SCHEMAS: 'release,pin_glide' });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(existsSync(join(jetgen(f, 'release'), 'table/receivers.go')), true);
  assert.equal(existsSync(join(jetgen(f, 'pin_glide'), 'table/receivers.go')), true);

  result = f.run('bash', ['scripts/codegen/generate.sh']);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(existsSync(jetgen(f, 'release')), false, 'a schema that became empty must lose its stale output');
  assert.equal(existsSync(jetgen(f, 'pin_glide')), false, 'a schema that became empty must lose its stale output');
});

for (const [step, code] of [['jet', 41], ['export', 42]]) {
  test(`a failing ${step} fails generation without publishing a description`, (t) => {
    const f = fixture(t);
    const result = f.run('bash', ['scripts/codegen/generate.sh'], { TOOL_FAIL: step });
    assert.equal(result.status, code, result.stderr);
    assert.equal(existsSync(join(f.cwd, 'generated/protocol/openapi.yaml')), false, result.stderr);
    assert.deepEqual(f.calls('docker').at(-1).args, ['rm', '-f', 'disposable-postgres']);
  });
}
