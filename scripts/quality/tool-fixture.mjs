// Shared fixture for the shell-entrypoint tests. Child processes, temporary
// files, exit statuses and cleanup are real; only the toolchain and container
// boundaries are stubbed, and the stub models the tool contract the entrypoints
// rely on.
import { spawnSync } from 'node:child_process';
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const qualityDir = dirname(fileURLToPath(import.meta.url));

export const repoRoot = join(qualityDir, '..', '..');

// Marks the negative fixture in test/fixtures/sql/desired_schema_violations.sql,
// which is what makes the stubbed database violating once it has been loaded.
export const violationMarker = 'pin-glide negative fixture';

export const toolCLI = String.raw`#!/usr/bin/env node
const fs = require('node:fs');
const path = require('node:path');
const tool = path.basename(process.argv[1]);
const args = process.argv.slice(2);
const entry = { tool, args };
const flag = (name) => (args.find((arg) => arg.startsWith(name + '=')) ?? name + '=').slice(name.length + 1);
if (tool === 'go' && args[1] === 'psqldef' && args.includes('--config')) {
  entry.sql = fs.readFileSync(0, 'utf8');
  entry.config = fs.readFileSync(args[args.indexOf('--config') + 1], 'utf8');
}
if (tool === 'docker' && args.includes('psql')) entry.sql = fs.readFileSync(0, 'utf8');
const log = process.env.TOOL_LOG;
const logged = fs.existsSync(log) ? fs.readFileSync(log, 'utf8') : '';
const loading = (entry.sql ?? '').includes('${violationMarker}');
const violating = !process.env.TOOL_ACCEPT_FIXTURE && (logged.includes('${violationMarker}') || loading);
fs.appendFileSync(log, JSON.stringify(entry) + '\n');
const fails = (kind) => process.env.TOOL_FAIL === kind;
if (tool === 'golangci-lint') {
  if (args[0] === 'version') {
    console.log('golangci-lint has version ' + (process.env.TOOL_VERSION ?? '0.0.0') + ' built with go1.27.1 from (unknown)');
  }
  process.exit(0);
}
if (tool === 'docker') {
  if (args[0] === 'run') console.log('disposable-postgres');
  if (args[0] === 'port') console.log('127.0.0.1:15432');
  if (args.includes('psql') && violating && !loading) {
    if (process.env.TOOL_SILENT_FIXTURE_FAILURE) process.exit(1);
    console.log('primary key must be named pk_<table>');
    console.log('foreign key must be named fk_delivery_receivers_id');
    console.log('unique constraints are not allowed; use a unique index');
    console.log('index must be named idx_receiversbad_receiver_id');
    console.log('ERROR: SQL lint found 4 violation(s)');
    process.exit(1);
  }
  if (args.includes('psql') && fails('catalog')) process.exit(31);
}
if (tool === 'npx') {
  if (fails('npx')) {
    console.error('npm error could not determine executable to run');
    process.exit(1);
  }
  // The contract validators reject only the deliberately violating fixtures,
  // so a green run proves the entrypoint passed them the exported documents.
  const contractFixture = { redocly: 'violating_openapi.yaml', asyncapi: 'violating_asyncapi.yaml' }[args[1]];
  if (contractFixture && args.some((arg) => arg.endsWith(contractFixture)) && !process.env.TOOL_ACCEPT_FIXTURE) {
    if (process.env.TOOL_SILENT_FIXTURE_FAILURE) process.exit(1);
    console.error(contractFixture.includes('openapi')
      ? 'no-unresolved-refs: $ref points to a schema that does not exist'
      : 'asyncapi-document-resolved: the document is missing a required field');
    process.exit(1);
  }
  if (args.includes('--version')) console.log(process.env.TOOL_VERSION ?? '0.0.0');
  else if (violating) {
    if (process.env.TOOL_SILENT_FIXTURE_FAILURE) process.exit(1);
    console.error('pin_glide.receiversbad: error pin-glide-object-naming : name must match');
    console.error('pin_glide.receiversbad: error pin-glide-comments : object must have a non-empty COMMENT ON');
    process.exit(1);
  } else if (fails('schemalint')) process.exit(32);
}
if (tool === 'uvx') {
  if (violating) {
    if (process.env.TOOL_SILENT_FIXTURE_FAILURE) process.exit(1);
    console.log('L:   4 | P:   1 | CP01 | Keywords must be upper case.');
    console.log('L:  14 | P:   1 | LT02 | Expected indent of 4 spaces.');
    process.exit(1);
  }
  if (fails('sqlfluff')) process.exit(33);
}
// The jet invocation writes <path>/<database>/<schema> only for the schemas
// listed in TOOL_JET_SCHEMAS, the way Jet succeeds without output when a
// schema has no tables, views or enums.
if (tool === 'go' && args[1] === 'jet') {
  if (fails('jet')) process.exit(41);
  const schema = flag('-schema');
  if ((process.env.TOOL_JET_SCHEMAS ?? '').split(',').includes(schema)) {
    const table = path.join(flag('-path'), process.env.PGDATABASE ?? 'pin_glide', schema, 'table');
    fs.mkdirSync(table, { recursive: true });
    fs.writeFileSync(path.join(table, 'receivers.go'), 'package table\n');
  }
}
// The offline export modes write the requested description to stdout.
if (tool === 'go' && args[0] === 'run') {
  if (fails('export')) process.exit(42);
  console.log('# stub description for ' + args.slice(1).join(' '));
}
if (tool === 'go' && args[1] === 'psqldef' && fails('apply')) process.exit(43);
`;

export function toolFixture(t, { copy = [], files = {}, tools = [] } = {}) {
  const cwd = mkdtempSync(join(tmpdir(), 'pin-glide-tool-fixture-'));
  t.after(() => rmSync(cwd, { recursive: true, force: true }));

  const write = (relative, contents) => {
    mkdirSync(dirname(join(cwd, relative)), { recursive: true });
    writeFileSync(join(cwd, relative), contents);
  };

  // Each test copies the real scripts it exercises, so the fixture cannot lean
  // on something the entrypoint does not actually call.
  for (const relative of copy) {
    mkdirSync(dirname(join(cwd, relative)), { recursive: true });
    copyFileSync(join(repoRoot, relative), join(cwd, relative));
  }
  for (const [relative, contents] of Object.entries(files)) write(relative, contents);
  for (const tool of tools) {
    write(`bin/${tool}`, toolCLI);
    chmodSync(join(cwd, `bin/${tool}`), 0o755);
  }
  write('commands.jsonl', '');

  const env = { ...process.env, PATH: `${join(cwd, 'bin')}:${process.env.PATH}`, TOOL_LOG: join(cwd, 'commands.jsonl') };
  return {
    cwd,
    run: (entry, args = [], extra = {}) => spawnSync(entry, args, {
      cwd, env: { ...env, ...extra }, encoding: 'utf8', timeout: 15000,
    }),
    calls: (tool) => readFileSync(join(cwd, 'commands.jsonl'), 'utf8')
      .trim().split('\n').filter(Boolean).map(JSON.parse)
      .filter((call) => !tool || call.tool === tool),
  };
}
