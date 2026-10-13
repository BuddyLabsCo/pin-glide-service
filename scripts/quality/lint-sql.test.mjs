import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

// Exercise the shell entry point. Only toolchain/container boundaries are faked;
// all child processes, temporary files, exit statuses, and cleanup are real.
const toolCLI = String.raw`#!/usr/bin/env node
const fs = require('node:fs'), path = require('node:path');
const tool = path.basename(process.argv[1]), args = process.argv.slice(2);
const entry = { tool, args };
if (tool === 'go' && args[1] === 'psqldef') {
  entry.sql = fs.readFileSync(0, 'utf8');
  entry.config = fs.readFileSync(args[args.indexOf('--config') + 1], 'utf8');
}
if (tool === 'docker' && args[0] === 'exec' && args.includes('psql')) {
  entry.sql = fs.readFileSync(0, 'utf8');
}
fs.appendFileSync(process.env.TOOL_LOG, JSON.stringify(entry) + '\n');
if (tool === 'docker') {
  if (args[0] === 'run') console.log('disposable-postgres');
  if (args[0] === 'port') console.log('127.0.0.1:15432');
  if (args.includes('psql') && process.env.TOOL_FAIL === 'catalog') process.exit(31);
}
if (tool === 'npx' && process.env.TOOL_FAIL === 'schemalint') process.exit(32);
if (tool === 'uvx' && process.env.TOOL_FAIL === 'sqlfluff') process.exit(33);
`;

function fixture(t) {
  const cwd = mkdtempSync(join(tmpdir(), 'pin-glide-lint-sql-'));
  t.after(() => rmSync(cwd, { recursive: true, force: true }));
  const write = (path, contents) => {
    mkdirSync(dirname(join(cwd, path)), { recursive: true });
    writeFileSync(join(cwd, path), contents);
  };
  for (const name of ['quality/lint-sql.sh', 'quality/sql-lint.sql', 'quality/sql-lint-ignore.sql', 'db/psqldef.sh', 'db/postgres.sh']) {
    write(`scripts/${name}`, '');
    copyFileSync(fileURLToPath(new URL(`../${name}`, import.meta.url)), join(cwd, 'scripts', name));
  }
  write('db/schema/release/00_extensions.sql', 'CREATE SCHEMA release;\n');
  write('db/schema/pin_glide/00_extensions.sql', 'CREATE SCHEMA pin_glide;\n');
  write('commands.jsonl', '');
  for (const tool of ['docker', 'go', 'npx', 'uvx']) {
    write(`bin/${tool}`, toolCLI);
    chmodSync(join(cwd, 'bin', tool), 0o755);
  }
  const env = { ...process.env, PATH: `${join(cwd, 'bin')}:${process.env.PATH}`, TOOL_LOG: join(cwd, 'commands.jsonl') };
  return {
    cwd,
    run: (extra = {}) => spawnSync('bash', ['scripts/quality/lint-sql.sh'], {
      cwd, env: { ...env, ...extra }, encoding: 'utf8', timeout: 15000,
    }),
    calls: () => readFileSync(join(cwd, 'commands.jsonl'), 'utf8').trim().split('\n').filter(Boolean).map(JSON.parse),
  };
}

test('SQL checks load both schemas, then run the catalog, Schemalint and pinned SQLFluff checks', (t) => {
  const f = fixture(t), result = f.run();
  assert.equal(result.status, 0, result.stderr);
  const calls = f.calls();
  const psql = calls.find((call) => call.tool === 'docker' && call.args.includes('psql'));
  assert.ok(psql, result.stderr);
  assert.ok(psql.args.includes('pin_glide'));
  assert.ok(psql.args.includes('ON_ERROR_STOP=1'));
  assert.match(psql.sql, /lint_ignore_object/);
  assert.match(psql.sql, /'pin_glide', 'release'/);
  const schemalint = calls.find((call) => call.tool === 'npx');
  assert.deepEqual(schemalint.args, ['--no-install', 'schemalint']);
  const sqlfluff = calls.find((call) => call.tool === 'uvx');
  assert.deepEqual(sqlfluff.args, [
    '--from', 'sqlfluff==4.3.0', 'sqlfluff', 'lint',
    'db/schema/release', 'db/schema/pin_glide', 'db/migrations', 'db/scripts',
  ]);
  assert.ok(calls.indexOf(psql) < calls.indexOf(schemalint), result.stderr);
  assert.ok(calls.indexOf(schemalint) < calls.indexOf(sqlfluff), result.stderr);
  assert.deepEqual(calls.at(-1), { tool: 'docker', args: ['rm', '-f', 'disposable-postgres'] });
});

for (const [failure, code, stopped] of [['catalog', 31, 'npx'], ['schemalint', 32, 'uvx'], ['sqlfluff', 33, null]]) {
  test(`a ${failure} failure fails the SQL check, stops later checks and removes the disposable database`, (t) => {
    const f = fixture(t), result = f.run({ TOOL_FAIL: failure });
    assert.equal(result.status, code, result.stderr);
    if (stopped) assert.ok(!f.calls().some((call) => call.tool === stopped), result.stderr);
    assert.deepEqual(f.calls().at(-1), { tool: 'docker', args: ['rm', '-f', 'disposable-postgres'] });
  });
}
