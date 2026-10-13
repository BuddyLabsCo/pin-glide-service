import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import { repoRoot, toolFixture, violationMarker } from './tool-fixture.mjs';

const pinnedSchemalint = JSON.parse(readFileSync(join(repoRoot, 'package.json'), 'utf8')).devDependencies.schemalint;

const fixture = (t) => toolFixture(t, {
  copy: [
    'package.json',
    'scripts/db/psqldef.sh',
    'scripts/db/postgres.sh',
    'scripts/quality/lint-sql.sh',
    'scripts/quality/sqlfluff.sh',
    'scripts/quality/sql-lint.sql',
    'scripts/quality/sql-lint-ignore.sql',
    'test/fixtures/sql/desired_schema_violations.sql',
  ],
  files: {
    'db/schema/release/00_extensions.sql': 'CREATE SCHEMA release;\n',
    'db/schema/pin_glide/00_extensions.sql': 'CREATE SCHEMA pin_glide;\n',
  },
  tools: ['docker', 'go', 'npx', 'uvx'],
});

const run = (f, extra = {}) => f.run('bash', ['scripts/quality/lint-sql.sh'], {
  TOOL_VERSION: pinnedSchemalint, ...extra,
});

test('SQL checks pass the desired schemas before proving each rule family rejects the violating fixture', (t) => {
  const f = fixture(t), result = run(f);
  assert.equal(result.status, 0, result.stderr);

  const catalog = f.calls('docker').filter((call) => call.args.includes('psql'));
  assert.equal(catalog.length, 3, result.stderr);
  assert.match(catalog[0].sql, /lint_ignore_object/);
  assert.ok(catalog[1].sql.includes(violationMarker), result.stderr);
  assert.match(catalog[2].sql, /lint_ignore_object/);

  const [version, ...schemalint] = f.calls('npx');
  assert.deepEqual(version.args, ['--no-install', 'schemalint', '--version']);
  assert.deepEqual(schemalint.map((call) => call.args), [
    ['--no-install', 'schemalint'],
    ['--no-install', 'schemalint'],
  ]);

  const sqlfluff = f.calls('uvx');
  assert.equal(sqlfluff.length, 2, result.stderr);
  const pins = sqlfluff.map((call) => call.args[1]);
  assert.match(pins[0], /^sqlfluff==/, result.stderr);
  assert.ok(pins.every((pin) => pin === pins[0]), result.stderr);
  assert.deepEqual(sqlfluff.map((call) => call.args.at(-1).replace(`${f.cwd}/`, '')), [
    'db/scripts',
    'test/fixtures/sql/desired_schema_violations.sql',
  ]);

  assert.deepEqual(f.calls('docker').at(-1).args, ['rm', '-f', 'disposable-postgres']);
});

test('the SQL checks stop on an installed Schemalint that does not match the pin', (t) => {
  const f = fixture(t), result = run(f, { TOOL_VERSION: '0.0.1' });
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, new RegExp(`schemalint ${pinnedSchemalint} is required; found 0\\.0\\.1\\.`));
  assert.equal(f.calls('uvx').length, 0, result.stderr);
  assert.deepEqual(f.calls('docker').at(-1).args, ['rm', '-f', 'disposable-postgres']);
});

for (const [failure, code, stopped] of [['catalog', 31, 'uvx'], ['schemalint', 32, 'uvx'], ['sqlfluff', 33, null]]) {
  test(`a ${failure} failure fails the SQL check, stops later checks and removes the disposable database`, (t) => {
    const f = fixture(t), result = run(f, { TOOL_FAIL: failure });
    assert.equal(result.status, code, result.stderr);
    if (stopped) assert.equal(f.calls(stopped).length, 0, result.stderr);
    assert.deepEqual(f.calls('docker').at(-1).args, ['rm', '-f', 'disposable-postgres']);
  });
}

test('the SQL checks fail when a checker accepts the violating fixture', (t) => {
  const f = fixture(t), result = run(f, { TOOL_ACCEPT_FIXTURE: '1' });
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /the catalog naming check accepted the violating fixture\./);
  assert.deepEqual(f.calls('docker').at(-1).args, ['rm', '-f', 'disposable-postgres']);
});

test('the SQL checks fail when a checker fails without reporting the violation', (t) => {
  const f = fixture(t), result = run(f, { TOOL_SILENT_FIXTURE_FAILURE: '1' });
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /the catalog naming check failed without reporting SQL lint found\./);
  assert.deepEqual(f.calls('docker').at(-1).args, ['rm', '-f', 'disposable-postgres']);
});
