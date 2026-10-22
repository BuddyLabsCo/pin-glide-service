#!/usr/bin/env node
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../..');

function fixture(t, cleanup = true) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'pin-glide-dev-test-'));
  if (cleanup) t.after(() => {
    spawnSync('make', ['dev-stop'], { cwd: dir, timeout: 5000,
      env: { ...process.env, TMPDIR: `${dir}/tmp`, PATH: `${dir}/bin:${process.env.PATH}` } });
    fs.rmSync(dir, { recursive: true, force: true });
  });
  fs.mkdirSync(path.join(dir, 'scripts'), { recursive: true });
  fs.cpSync(path.join(root, 'scripts/dev'), path.join(dir, 'scripts/dev'), { recursive: true });
  fs.copyFileSync(path.join(root, 'Makefile'), path.join(dir, 'Makefile'));
  fs.copyFileSync(path.join(root, 'version.json'), path.join(dir, 'version.json'));
  fs.mkdirSync(path.join(dir, 'dev/anon'), { recursive: true });
  fs.mkdirSync(path.join(dir, 'bin'));
  fs.mkdirSync(path.join(dir, 'tmp'));
  const log = path.join(dir, 'commands.log');
  fs.writeFileSync(path.join(dir, 'bin/docker'), `#!/bin/sh\nprintf '%s\\n' "$*" >> '${log}'\nexit 81\n`, { mode: 0o755 });
  return {
    dir, log,
    run(target, extra = {}) {
      return spawnSync('make', [target], {
        cwd: dir, encoding: 'utf8', timeout: 5000,
        env: { ...process.env, TMPDIR: `${dir}/tmp`, PATH: `${dir}/bin:${process.env.PATH}`, ...extra },
      });
    },
  };
}

function commandFakes(f) {
  const source = `#!/usr/bin/env node
const fs = require('node:fs');
const path = require('node:path');
const name = path.basename(process.argv[1]);
const args = process.argv.slice(2);
const input = fs.readFileSync(0, 'utf8');
fs.appendFileSync(${JSON.stringify(f.log)}, JSON.stringify({ name, args, input }) + '\\n');
if (name === 'docker' && args.some(arg => arg.includes('to_regclass'))) console.log('f');
if (name === 'go' && args[0] === 'build') {
  const output = args[args.indexOf('-o') + 1];
  fs.writeFileSync(output, '#!/bin/sh\\nexit 0\\n', { mode: 0o755 });
}
if (process.env.FAIL_AT && (args.includes(process.env.FAIL_AT) || input.includes(process.env.FAIL_AT))) process.exit(82);
`;
  for (const name of ['docker', 'go']) fs.writeFileSync(path.join(f.dir, 'bin', name), source, { mode: 0o755 });
  f.events = () => fs.readFileSync(f.log, 'utf8').trim().split('\n').map(JSON.parse);
}

// Regression: a typo in the snapshot directory must never reset local data.
// Seam: public make command, with Docker replaced only at its external boundary.
test('dev rejects missing and non-directory snapshot paths before touching Docker', t => {
  const f = fixture(t);
  const file = path.join(f.dir, 'file');
  fs.writeFileSync(file, 'not a directory');
  for (const target of ['dev']) {
    for (const snapshot of ['', path.join(f.dir, 'missing'), file]) {
      const result = f.run(target, { PIN_GLIDE_SNAPSHOT_PATH: snapshot });
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /PIN_GLIDE_SNAPSHOT_PATH.*director/i);
      assert(!fs.existsSync(f.log), 'Docker ran before validating PIN_GLIDE_SNAPSHOT_PATH');
    }
  }
  const unreadable = path.join(f.dir, 'unreadable');
  fs.mkdirSync(unreadable);
  fs.chmodSync(unreadable, 0o000);
  let result;
  try {
    result = f.run('dev', { PIN_GLIDE_SNAPSHOT_PATH: unreadable });
  } finally {
    fs.chmodSync(unreadable, 0o700);
  }
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /PIN_GLIDE_SNAPSHOT_PATH.*director/i);
  assert(!fs.existsSync(f.log), 'Docker ran before validating PIN_GLIDE_SNAPSHOT_PATH');

  const snapshots = path.join(f.dir, 'snapshots');
  const blocked = path.join(snapshots, 'blocked.dump');
  fs.mkdirSync(snapshots);
  fs.writeFileSync(blocked, 'dump');
  fs.chmodSync(blocked, 0o000);
  try {
    result = f.run('dev', { PIN_GLIDE_SNAPSHOT_PATH: snapshots });
  } finally {
    fs.chmodSync(blocked, 0o600);
  }
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /Snapshot file is not readable/);
  assert(!fs.existsSync(f.log), 'Docker ran before validating the selected snapshot');
});

// Regression: stale snapshots or apps starting before masking expose unsafe data.
// Seam: make dev, observing external Docker/Go operations in order.
test('dev resets isolated infrastructure and restores the latest dump before migration and masking', t => {
  const f = fixture(t);
  commandFakes(f);
  const snapshots = path.join(f.dir, 'snapshots with spaces');
  fs.mkdirSync(snapshots);
  fs.writeFileSync(path.join(snapshots, 'older.dump'), 'old dump');
  fs.writeFileSync(path.join(snapshots, 'newer.dump'), 'new dump');
  fs.writeFileSync(path.join(snapshots, 'zzz.dump'), 'tie dump');
  fs.writeFileSync(path.join(snapshots, 'source'), 'symlink target');
  fs.symlinkSync(path.join(snapshots, 'source'), path.join(snapshots, 'linked.dump'));
  fs.utimesSync(path.join(snapshots, 'older.dump'), 1, 1);
  const tied = new Date(2_000_000);
  fs.utimesSync(path.join(snapshots, 'newer.dump'), tied, tied);
  fs.utimesSync(path.join(snapshots, 'zzz.dump'), tied, tied);
  fs.utimesSync(path.join(snapshots, 'source'), 3_000_000, 3_000_000);
  fs.writeFileSync(path.join(f.dir, 'dev/anon/fixture.sql'), '-- fixture label');
  const result = f.run('dev', { PIN_GLIDE_SNAPSHOT_PATH: snapshots, DLV_BIN: '/does/not/exist' });
  assert.equal(result.status, 0, result.stderr);
  const events = f.events();
  const reset = events.findIndex(e => e.name === 'docker' && e.args.includes('down'));
  const postgresUp = events.find(e => e.name === 'docker' && e.args.includes('up')
    && e.args.includes('postgres'));
  const restore = events.findIndex(e => e.args.includes('pg_restore'));
  const migrate = events.findIndex(e => e.name === 'go' && e.args.join(' ') === 'run ./cmd/pin-glide-db-tools migrate');
  const mask = events.findIndex(e => e.input.includes('SELECT anon.anonymize_database();'));
  const app = events.findIndex(e => e.name === 'go' && e.args.includes('./cmd/pin-glide-api'));
  assert(reset >= 0 && restore > reset && migrate > restore && mask > migrate && app > mask,
    'Required reset → restore → migrate → mask → apps sequence missing');
  assert(postgresUp?.args.includes('--build'), 'Development PostgreSQL was not rebuilt from source');
  assert.equal(events[restore].input, 'tie dump');
  assert.match(events[mask].input, /SET LOCAL anon.nosuperuser = false;/);
  assert.match(events[mask].input, /-- fixture label/);
  assert(events[reset].args.includes('--volumes'));
  const project = events[reset].args[events[reset].args.indexOf('-p') + 1];
  assert.match(project, /^pin-glide-dev-[a-f0-9]+$/);
  for (const event of events.filter(e => e.name === 'docker')) {
    assert.equal(event.args[event.args.indexOf('-p') + 1], project);
    assert(!event.args.some(arg => arg.includes('deploy/compose')));
  }
  assert(!events.some(e => e.name === 'docker' && e.args.includes('stop')), 'Application exit stopped infrastructure');
});

// Regression: code-only and stop must work without access to the snapshot source.
test('code-only preserves infrastructure and dev-stop removes only its local stack', t => {
  const f = fixture(t);
  commandFakes(f);
  const snapshots = path.join(f.dir, 'empty');
  fs.mkdirSync(snapshots);
  let result = f.run('dev', { PIN_GLIDE_SNAPSHOT_PATH: snapshots, DLV_BIN: '/does/not/exist' });
  assert.equal(result.status, 0, result.stderr);
  assert(!f.events().some(e => e.args.includes('pg_restore')), 'Empty directory restored a dump');
  fs.writeFileSync(f.log, '');
  result = f.run('dev-code-only', { PIN_GLIDE_SNAPSHOT_PATH: '', DLV_BIN: '/does/not/exist' });
  assert.equal(result.status, 0, result.stderr);
  assert(f.events().some(e => e.name === 'go' && e.args.includes('./cmd/pin-glide-api')));
  assert(!f.events().some(e => e.name === 'docker' || e.args.includes('migrate')));
  fs.writeFileSync(f.log, '');
  result = f.run('dev-stop', { PIN_GLIDE_SNAPSHOT_PATH: '' });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(f.events().length, 1);
  assert(f.events()[0].args.includes('down'));
  assert(f.events()[0].args.includes('--volumes'));
  result = f.run('dev-code-only', { PIN_GLIDE_SNAPSHOT_PATH: '' });
  assert.notEqual(result.status, 0, 'Stopped stack accepted code-only startup');
});

// Regression: a PID file with a reused process ID must not signal that process.
test('stale application PID does not stop an unrelated process', t => {
  const f = fixture(t);
  commandFakes(f);
  const project = `pin-glide-dev-${createHash('sha256').update(fs.realpathSync(f.dir)).digest('hex').slice(0, 16)}`;
  const state = path.join(f.dir, 'tmp', project);
  fs.mkdirSync(state, { recursive: true });
  const unrelated = spawn('sleep', ['30'], { stdio: 'ignore' });
  t.after(() => { try { unrelated.kill(); } catch {} });
  fs.writeFileSync(path.join(state, 'apps.pid'), `${unrelated.pid} 0\n`);
  const result = f.run('dev-stop', { PIN_GLIDE_SNAPSHOT_PATH: '' });
  assert.equal(result.status, 0, result.stderr);
  assert.doesNotThrow(() => process.kill(unrelated.pid, 0), 'stale PID killed an unrelated process');
  assert(!fs.existsSync(path.join(state, 'apps.pid')));
});

test('malformed application PID state does not block recovery', t => {
  const f = fixture(t);
  commandFakes(f);
  const project = `pin-glide-dev-${createHash('sha256').update(fs.realpathSync(f.dir)).digest('hex').slice(0, 16)}`;
  const state = path.join(f.dir, 'tmp', project);
  fs.mkdirSync(state, { recursive: true });
  for (const contents of ['', '123\n']) {
    fs.writeFileSync(path.join(state, 'apps.pid'), contents);
    const result = f.run('dev-stop', { PIN_GLIDE_SNAPSHOT_PATH: '' });
    assert.equal(result.status, 0, result.stderr);
    assert(!fs.existsSync(path.join(state, 'apps.pid')));
  }
});

// Regression: restore, migration or masking failure must never expose a raw dump.
test('failed database preparation leaves application processes gated', t => {
  const f = fixture(t);
  commandFakes(f);
  const snapshots = path.join(f.dir, 'snapshots');
  fs.mkdirSync(snapshots);
  fs.writeFileSync(path.join(snapshots, 'fixture.dump'), 'dump');
  fs.writeFileSync(path.join(f.dir, 'dev/anon/fixture.sql'),
    "SECURITY LABEL FOR anon ON COLUMN fixture.config.secret IS 'MASKED WITH VALUE NULL';");
  for (const failure of ['pg_restore', 'migrate', 'SELECT anon.init();', 'SECURITY LABEL FOR anon',
    'SELECT anon.anonymize_database();']) {
    const result = f.run('dev', { PIN_GLIDE_SNAPSHOT_PATH: snapshots, FAIL_AT: failure });
    assert.notEqual(result.status, 0, `${failure} unexpectedly succeeded`);
    assert(!f.events().some(e => e.args.includes('./cmd/pin-glide-api')));
    const retry = f.run('dev-code-only', { PIN_GLIDE_SNAPSHOT_PATH: '' });
    assert.notEqual(retry.status, 0, 'Failed preparation marked database ready');
    fs.writeFileSync(f.log, '');
  }
});

function live(f, target, extra = {}) {
  const child = spawn('make', [target], {
    cwd: f.dir, detached: true, stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...process.env, TMPDIR: `${f.dir}/tmp`, PATH: `${f.dir}/bin:${process.env.PATH}`,
      DLV_BIN: '/does/not/exist', ...extra },
  });
  const exited = new Promise(resolve => child.once('exit', (code, signal) => resolve({ code, signal })));
  const ready = new Promise((resolve, reject) => {
    let output = '';
    child.stdout.on('data', data => { output += data; if (output.includes('APP_READY')) resolve(); });
    child.stderr.on('data', data => { output += data; });
    child.once('exit', () => reject(new Error(`Exited before apps ready: ${output}`)));
  });
  return { child, exited, ready };
}

// Regression: Ctrl-C must stop app descendants while retaining PostgreSQL.
test('Ctrl-C stops apps and leaves infrastructure available for code-only restart', { timeout: 10000 }, async t => {
  const f = fixture(t);
  commandFakes(f);
  // Keep only external app processes alive; all infrastructure commands stay fake.
  const go = path.join(f.dir, 'bin/go');
  fs.appendFileSync(go, `\nif (args[0] === 'build') fs.writeFileSync(args[args.indexOf('-o') + 1], '#!/usr/bin/env node\\nconsole.log("APP_READY"); setInterval(() => {}, 1000);\\n', { mode: 0o755 });\n`);
  const snapshots = path.join(f.dir, 'empty');
  fs.mkdirSync(snapshots);
  const first = live(f, 'dev', { PIN_GLIDE_SNAPSHOT_PATH: snapshots });
  t.after(() => { try { process.kill(-first.child.pid, 'SIGKILL'); } catch {} });
  await first.ready;
  process.kill(-first.child.pid, 'SIGINT');
  await first.exited;
  assert(!f.events().some(e => e.name === 'docker' && e.args.includes('stop')));
  assert.equal(f.events().filter(e => e.name === 'docker' && e.args.includes('down')).length, 1);
  fs.writeFileSync(f.log, '');
  const second = live(f, 'dev-code-only', { PIN_GLIDE_SNAPSHOT_PATH: '' });
  t.after(() => { try { process.kill(-second.child.pid, 'SIGKILL'); } catch {} });
  await second.ready;
  assert(!f.events().some(e => e.name === 'docker' || e.args.includes('migrate')));
  const replacement = live(f, 'dev-code-only', { PIN_GLIDE_SNAPSHOT_PATH: '' });
  t.after(() => { try { process.kill(-replacement.child.pid, 'SIGKILL'); } catch {} });
  await replacement.ready;
  await second.exited;
  const stopped = f.run('dev-stop', { PIN_GLIDE_SNAPSHOT_PATH: '' });
  assert.equal(stopped.status, 0, stopped.stderr);
  await replacement.exited;
});

// Regression: replacing a supervisor during a slow compile must cancel that build.
test('code-only can replace an in-progress compilation', { timeout: 15000 }, async t => {
  const f = fixture(t);
  commandFakes(f);
  fs.appendFileSync(path.join(f.dir, 'bin/go'), `
if (args[0] === 'build' && !fs.existsSync(${JSON.stringify(path.join(f.dir, 'built-once'))})) {
  fs.writeFileSync(${JSON.stringify(path.join(f.dir, 'built-once'))}, '');
  console.log('APP_READY'); setInterval(() => {}, 1000);
}
if (args[0] === 'build') fs.writeFileSync(args[args.indexOf('-o') + 1], '#!/usr/bin/env node\\nconsole.log("APP_READY"); setInterval(() => {}, 1000);\\n', { mode: 0o755 });
`);
  const snapshots = path.join(f.dir, 'empty');
  fs.mkdirSync(snapshots);
  const first = live(f, 'dev', { PIN_GLIDE_SNAPSHOT_PATH: snapshots });
  t.after(() => { try { process.kill(-first.child.pid, 'SIGKILL'); } catch {} });
  await first.ready;
  const replacement = live(f, 'dev-code-only', { PIN_GLIDE_SNAPSHOT_PATH: '' });
  t.after(() => { try { process.kill(-replacement.child.pid, 'SIGKILL'); } catch {} });
  await replacement.ready;
  await first.exited;
  assert.equal(f.run('dev-stop', { PIN_GLIDE_SNAPSHOT_PATH: '' }).status, 0);
  await replacement.exited;
});

// Real Docker covers pg_restore compatibility, actual Pin Glide masking,
// inherited cron suppression and the API-only code restart path.
test('Docker snapshot restore migrates and masks before apps; code-only retains local edits', {
  skip: !process.argv.includes('--docker'), timeout: 900000,
}, async t => {
  const f = fixture(t, false);
  commandFakes(f);
  fs.unlinkSync(path.join(f.dir, 'bin/docker'));
  const project = `pin-glide-dev-${createHash('sha256').update(f.dir).digest('hex').slice(0, 16)}`;
  const composeArgs = ['compose', '--env-file', '/dev/null', '-p', project,
    '-f', path.join(f.dir, 'scripts/dev/docker-compose.yml')];
  function run(command, args, options = {}) {
    const result = spawnSync(command, args, { encoding: 'utf8', timeout: 600000,
      env: process.env, maxBuffer: 16 * 1024 * 1024, ...options });
    assert.equal(result.status, 0, `${command} ${args.join(' ')}: ${result.error || result.stderr || result.stdout}`);
    return result.stdout;
  }
  const compose = (...args) => run('docker', [...composeArgs, ...args]);
  const sql = statement => compose('exec', '-T', 'postgres', 'psql', '-X', '-U', 'pin_glide', '-d', 'pin_glide',
    '-v', 'ON_ERROR_STOP=1', '-Atc', statement).trim();
  const go = run('sh', ['-c', 'command -v go']).trim();
  const image = `${project}-db-tools`;
  const dockerQuery = statement => JSON.stringify([
    ...composeArgs, 'exec', '-T', 'postgres', 'psql', '-X', '-U', 'pin_glide', '-d', 'pin_glide', '-Atc', statement,
  ]);
  const gate = `const cp = require('node:child_process');
const config = cp.spawnSync('docker', ${dockerQuery("SELECT enabled::text || '|' || workspace_id || '|' || model FROM pin_glide.model_configurations WHERE id = 'default'")}, { encoding: 'utf8' });
const cron = cp.spawnSync('docker', ${dockerQuery("SELECT (SELECT count(*) FROM cron.job WHERE active) || '|' || (SELECT count(*) FROM cron.job_run_details)")}, { encoding: 'utf8' });
if (config.status !== 0 || !/^false\\|(?:local-workspace|local-edit)\\|qwen3$/.test(config.stdout.trim())) process.exit(96);
const [active, runs] = cron.stdout.trim().split('|').map(Number);
if (cron.status !== 0 || !(active > 0 && runs === 0)) process.exit(97);
if (process.env.PIN_GLIDE_WECHAT_APP_ID || process.env.PIN_GLIDE_WECHAT_APP_SECRET ||
    process.env.PIN_GLIDE_QQ_APP_ID || process.env.PIN_GLIDE_QQ_APP_KEY ||
    process.env.PIN_GLIDE_BAILIAN_API_KEY) process.exit(98);
console.log('MASKED_APP_START');
console.log('APP_READY');
setInterval(() => {}, 1000);`;
  fs.appendFileSync(path.join(f.dir, 'bin/go'), `\nif (args[0] === 'run') {
  const result = require('node:child_process').spawnSync(${JSON.stringify(go)}, args, { cwd: ${JSON.stringify(root)}, stdio: 'inherit' });
  process.exit(result.status ?? 1);
}
if (args[0] === 'build') fs.writeFileSync(args[args.indexOf('-o') + 1], ${JSON.stringify(`#!/usr/bin/env node\n${gate}\n`)}, { mode: 0o755 });\n`);
  const snapshots = path.join(f.dir, 'snapshots');
  fs.mkdirSync(snapshots);
  const providerSentinels = {
    PIN_GLIDE_WECHAT_APP_ID: 'deployed-wechat-app',
    PIN_GLIDE_WECHAT_APP_SECRET: 'deployed-wechat-secret',
    PIN_GLIDE_QQ_APP_ID: 'deployed-qq-app',
    PIN_GLIDE_QQ_APP_KEY: 'deployed-qq-key',
    PIN_GLIDE_BAILIAN_API_KEY: 'deployed-bailian-key',
  };
  let active;
  let imageBuilt = false;
  t.after(() => {
    if (active) { try { process.kill(-active.child.pid, 'SIGTERM'); } catch {} }
    try {
      compose('down', '--volumes', '--remove-orphans', '--timeout', '5');
      if (imageBuilt) run('docker', ['image', 'rm', image]);
    } finally { fs.rmSync(f.dir, { recursive: true, force: true }); }
  });
  run('docker', ['build', '-f', path.join(root, 'deploy/compose/db-tools.Dockerfile'), '-t', image, root]);
  imageBuilt = true;
  compose('up', '--detach', '--wait', '--build', 'postgres');
  run(go, ['run', './cmd/pin-glide-db-tools', 'migrate'], { cwd: root, env: {
    ...process.env, PIN_GLIDE_ENV: 'development', PIN_GLIDE_DATABASE_URL: 'postgres://pin_glide:pin_glide@127.0.0.1:55432/pin_glide?sslmode=disable',
    PIN_GLIDE_RELEASE_VERSION: JSON.parse(fs.readFileSync(path.join(root, 'version.json'))).version,
    PIN_GLIDE_RELEASE_INITIAL: 'true', PIN_GLIDE_RELEASE_INITIAL_RETRY: 'false',
  } });
  sql(`
    INSERT INTO pin_glide.model_configurations (id, workspace_id, model, enabled)
    VALUES ('default', 'deployed-workspace', 'qwen3', true)
    ON CONFLICT (id) DO UPDATE SET workspace_id = excluded.workspace_id, model = excluded.model, enabled = true;
    INSERT INTO pin_glide.identity_accounts (id, provider, app_id, open_id)
    VALUES ('account-local', 'wechat', 'app-local', 'open-local');
    INSERT INTO pin_glide.identity_sender_registrations (id, account_id)
    VALUES ('sender-local', 'account-local');
    INSERT INTO pin_glide.identity_receiver_registrations (id, account_id, state)
    VALUES ('receiver-local', 'account-local', 'active');
    INSERT INTO pin_glide.pairing_pairings (id, sender_id, receiver_id, alias, state, activated_at)
    VALUES ('pairing-local', 'sender-local', 'receiver-local', 'local alias', 'active', now());
    INSERT INTO pin_glide.delivery_deliveries (id, sender_id, message_id, receiver_id, pairing_id)
    VALUES ('delivery-local', 'sender-local', 'message-local', 'receiver-local', 'pairing-local');
    CREATE SCHEMA fixture;
    CREATE TABLE fixture.cron_calls (called_at timestamptz NOT NULL DEFAULT now());
    SELECT cron.schedule('pin_glide_dev_canary', '1 second', 'INSERT INTO fixture.cron_calls DEFAULT VALUES');
  `);
  const labels = fs.readFileSync(path.join(root, 'dev/anon/model_configurations.sql'), 'utf8');
  sql(`CREATE EXTENSION anon; SELECT anon.init(); ${labels}`);
  const sourceConfig = JSON.parse(compose('config', '--format', 'json'));
  sourceConfig.services['db-tools'] = { image, environment: {
    PIN_GLIDE_DATABASE_URL: 'postgres://pin_glide:pin_glide@postgres:5432/pin_glide?sslmode=disable',
  } };
  fs.writeFileSync(path.join(f.dir, 'compose.yaml'), JSON.stringify(sourceConfig));
  fs.copyFileSync(path.join(root, 'install.sh'), path.join(f.dir, 'install.sh'));
  fs.writeFileSync(path.join(f.dir, '.env'), `COMPOSE_PROJECT_NAME=${project}\nPIN_GLIDE_SNAPSHOT_PATH=${snapshots}\nPIN_GLIDE_SNAPSHOT_KEEP=2\n`);
  const snapshot = () => run('bash', ['install.sh', '--make-snapshot'], { cwd: f.dir });
  snapshot();
  const dumps = () => fs.readdirSync(snapshots).filter(name => name.endsWith('.dump')).sort();
  const older = dumps()[0];
  const owner = fs.statSync(path.join(snapshots, older));
  assert.equal(owner.uid, process.getuid(), 'Snapshot is not owned by the invoking operator');
  assert.equal(owner.gid, process.getgid());
  assert.equal(owner.mode & 0o777, 0o600, 'Snapshot permissions must remain private');
  fs.utimesSync(path.join(snapshots, older), 1, 1);
  sql("UPDATE pin_glide.model_configurations SET workspace_id = 'newer-workspace'");
  assert.equal(sql("SELECT workspace_id FROM pin_glide.model_configurations WHERE id = 'default'"), 'newer-workspace');
  snapshot();
  assert.equal(dumps().length, 2);
  const newer = dumps().at(-1);
  const dumpRun = (...args) => run('docker', ['run', '--rm', '--user', `${process.getuid()}:${process.getgid()}`,
    '--entrypoint', 'pg_restore', '-v', `${snapshots}:/snapshots:ro`, image, ...args, `/snapshots/${newer}`]);
  const contents = dumpRun('--list');
  assert(!contents.includes('SECURITY LABEL'), 'Source masking labels escaped into the archive');
  assert(!contents.includes('EXTENSION - anon'), 'Source anon extension escaped into the archive');
  const rawData = dumpRun('--data-only', '--file=-');
  assert.match(rawData, /newer-workspace/, 'Snapshot data was masked at the producer');
  assert.match(rawData, /pin_glide_dev_canary/, 'Snapshot did not retain the active cron fixture');
  fs.copyFileSync(path.join(root, 'dev/anon/model_configurations.sql'), path.join(f.dir, 'dev/anon/model_configurations.sql'));
  active = live(f, 'dev', { PIN_GLIDE_SNAPSHOT_PATH: snapshots, ...providerSentinels });
  await active.ready;
  assert.equal(sql("SELECT enabled::text || '|' || workspace_id || '|' || model FROM pin_glide.model_configurations WHERE id = 'default'"),
    'false|local-workspace|qwen3');
  assert.equal(sql("SELECT alias || '|' || state FROM pin_glide.pairing_pairings WHERE id = 'pairing-local'"),
    'local alias|active');
  assert.equal(sql("SELECT count(*) FROM pin_glide.delivery_deliveries WHERE id = 'delivery-local'"), '1');
  assert.equal(sql("SELECT extversion FROM pg_extension WHERE extname = 'anon'"), '3.2.3');
  assert.equal(sql("SELECT extversion FROM pg_extension WHERE extname = 'pg_cron'"), '1.6');
  assert(Number(sql("SELECT count(*) FROM cron.job WHERE active")) > 0);
  assert.equal(sql("SELECT count(*) FROM cron.job_run_details"), '0');
  const database = compose('ps', '-q', 'postgres');
  const history = sql('SELECT count(*) FROM release.release_deploy_history');
  process.kill(-active.child.pid, 'SIGINT');
  await active.exited;
  sql("UPDATE pin_glide.model_configurations SET workspace_id = 'local-edit'");
  active = live(f, 'dev-code-only', { PIN_GLIDE_SNAPSHOT_PATH: '', ...providerSentinels });
  await active.ready;
  assert.equal(sql("SELECT workspace_id FROM pin_glide.model_configurations WHERE id = 'default'"), 'local-edit');
  assert.equal(sql('SELECT count(*) FROM release.release_deploy_history'), history);
  assert.equal(compose('ps', '-q', 'postgres'), database);
  run('make', ['dev-stop'], { cwd: f.dir, env: { ...process.env, TMPDIR: `${f.dir}/tmp`, PIN_GLIDE_SNAPSHOT_PATH: '' } });
  await active.exited;
  active = undefined;
  for (const kind of ['container', 'volume', 'network']) {
    assert.equal(run('docker', [kind, 'ls', kind === 'container' ? '-aq' : '-q', '--filter',
      `label=com.docker.compose.project=${project}`]).trim(), '', `Leaked ${kind}`);
  }
});
