import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import { once } from 'node:events';

const script = fileURLToPath(new URL('../../install.sh', import.meta.url));

// Exercise the copied installer through its CLI/menu. Only Docker and sleeping
// are simulated; test/smoke exercises real PostgreSQL dumps and restores
// separately.
function installation(t, release = '1.9.0') {
  const root = mkdtempSync(join(tmpdir(), 'pin-glide-install-test-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const bin = join(root, 'bin'), log = join(root, 'docker.log'), state = join(root, 'docker.json');
  mkdirSync(bin);
  writeFileSync(join(root, '.env'), `# Preserve this file exactly\nRELEASE=${release}\nSECRET='$(touch hacked)'\n`);
  writeFileSync(join(root, 'compose.yaml'), 'name: install-test\nservices: {}\n');
  writeFileSync(state, JSON.stringify({ container: null, running: false, databaseVersion: release, cron: null }));
  writeFileSync(log, '');
  // The drain poll is bounded by wall-clock seconds in production; the test
  // keeps the poll count honest without waiting a minute for a blocked drain.
  writeFileSync(join(bin, 'sleep'), '#!/bin/sh\nexit 0\n');
  chmodSync(join(bin, 'sleep'), 0o755);
  writeFileSync(join(bin, 'docker'), `#!/usr/bin/env node
const fs = require('node:fs');
const args = process.argv.slice(2), path = process.env.DOCKER_STATE;
const s = JSON.parse(fs.readFileSync(path, 'utf8'));
fs.appendFileSync(process.env.DOCKER_LOG, JSON.stringify({args, release: process.env.RELEASE ?? null}) + '\\n');
const jobs = () => (s.cron ?? {}).jobs ?? {};
if (args.includes('config')) console.log(JSON.stringify({name:'install-test'}));
else if (args[0] === 'container' && args[1] === 'ls') { if(s.container) console.log('install-test-db-tools'); }
else if(args[0] === 'container' && args[1] === 'inspect') console.log(JSON.stringify([s.container]));
else if(args[0] === 'logs') { if(s.fail === 'logs') process.exit(42); console.log(s.container.logs ?? ''); }
else if(args.includes('ps')) { if(s.running) console.log('api'); }
else if(args.includes('stop')) { s.running=false; }
else if(args.includes('up') && args.includes('api')) { s.running=true; }
else if(args[0] === 'container' && args[1] === 'rm') s.container=null;
else if(args.includes('run')) {
  const version=args[args.indexOf('--label')+1].split('=')[1];
  s.container={State:{Status:'exited',ExitCode:s.migrateExit ?? 0},Config:{Labels:{'pin-glide.release':version}},logs:s.migrateLogs ?? ''};
  fs.writeFileSync(path, JSON.stringify(s)); if(s.interrupt) process.kill(process.ppid, 'SIGTERM'); process.exit(s.migrateExit ?? 0);
} else if(args.includes('exec')) {
  const command=args.at(-1);
  if(command.includes('SELECT release_version')) { if(s.fail === 'query') process.exit(42); console.log(s.databaseVersion ?? ''); }
  if(command.includes('pg_dump')) { if(s.fail === 'dump') process.exit(42); console.log('complete-original-database'); }
  if(command.includes('pg_restore') && s.fail === 'restore') process.exit(42);
  if(command.includes("pg_extension WHERE extname = 'pg_cron'")) console.log(s.cron ? '1' : '0');
  if(command.includes('json_object_agg')) console.log(JSON.stringify(jobs()));
  if(command.includes('FROM cron.job WHERE active')) console.log(String(Object.values(jobs()).filter(Boolean).length));
  if(command.includes('cron.job_run_details')) console.log(String((s.cron ?? {}).running ?? 0));
  if(command === 'UPDATE cron.job SET active = false') for (const id of Object.keys(jobs())) s.cron.jobs[id] = false;
  if(command.includes('WHERE jobid IN (')) {
    const ids=command.match(/WHERE jobid IN \\(([^)]*)\\)/)[1].split(',').filter(Boolean).map(Number);
    for(const id of ids) s.cron.jobs[id]=command.includes('SET active = true');
  }
}
if(s.hold && args.includes('pull')) { console.log('holding'); setTimeout(() => {}, 1500); }
if(s.fail && args.includes(s.fail)) process.exit(42);
fs.writeFileSync(path, JSON.stringify(s));
`);
  chmodSync(join(bin, 'docker'), 0o755);
  writeFileSync(join(bin, 'mv'), `#!/usr/bin/env node
const fs=require('node:fs'), cp=require('node:child_process');
const args=process.argv.slice(2), s=JSON.parse(fs.readFileSync(process.env.DOCKER_STATE,'utf8'));
if(s.failJSONAfterMigrate && args.at(-1).startsWith('attempt-') && s.container?.State.ExitCode === 0) process.exit(42);
const result=cp.spawnSync('/usr/bin/mv',args,{stdio:'inherit'});
if(s.interruptPrep && args.at(-1) === '.env.bak' && result.status === 0) process.kill(process.ppid,'SIGTERM');
process.exit(result.status ?? 1);
`);
  chmodSync(join(bin, 'mv'), 0o755);
  const env = { ...process.env, PATH: `${bin}:${process.env.PATH}`, DOCKER_STATE: state, DOCKER_LOG: log, RELEASE: '99.0.0' };
  const run = (args, input) => spawnSync('bash', [script, ...args], { cwd: root, env, input, encoding: 'utf8', timeout: 15000 });
  const read = name => readFileSync(join(root, name), 'utf8');
  return { root, env, run, read, calls: () => read('docker.log').trim().split('\n').filter(Boolean).map(JSON.parse),
    configure: patch => writeFileSync(state, JSON.stringify({ ...JSON.parse(read('docker.json')), ...patch })),
    files: () => readdirSync(root) };
}

const active = (f, version = '1.10.0') => JSON.parse(f.read(`attempt-${version}.json`));
const archived = (f, version = '1.10.0') => f.files().filter(name => new RegExp(`^attempt-${version.replaceAll('.', '\\.')}-.+\\.json$`).test(name));

test('prep preserves the full environment, compares numeric versions, and never overwrites an active attempt', t => {
  const f = installation(t), original = f.read('.env');
  const prep = f.run(['--version', '1.10.0', '--prep']);
  assert.equal(prep.status, 0, prep.stderr);
  assert.equal(f.read('.env.bak'), original);
  assert.match(f.read('.env'), /^RELEASE=1\.10\.0$/m);
  assert.equal(active(f).phase, 'prepared');
  assert.notEqual(f.run(['--version', '1.11.0', '--prep']).status, 0);
  assert.equal(f.read('.env.bak'), original);
  assert.equal(f.files().includes('hacked'), false);
  assert.ok(f.calls().every(call => call.release === null));
});

test('ordinary start accepts preparing or failed migration attempts and archives only successful starts', t => {
  for (const migration of ['not_started', 'failed']) {
    const f = installation(t);
    assert.notEqual(f.run(['--version', '1.9.0', '--start-api']).status, 0);
    writeFileSync(join(f.root, 'attempt-1.9.0.json'), JSON.stringify({ phase: 'preparing', migration: { status: migration } }));
    f.configure({ fail: 'up' });
    assert.notEqual(f.run(['--version', '1.9.0', '--start-api']).status, 0);
    assert.ok(f.files().includes('attempt-1.9.0.json'));
    f.configure({ fail: null });
    const result = f.run(['--version', '1.9.0', '--start-api']);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(f.files().includes('attempt-1.9.0.json'), false);
    assert.equal(archived(f, '1.9.0').length, 1);
    assert.equal(f.files().includes('.env.bak'), false);
  }
});

test('menu entry never prepares automatically and shares the CLI start exception', t => {
  const f = installation(t);
  const result = f.run(['--version', '1.9.0'], '6\n');
  assert.notEqual(result.status, 0);
  assert.equal(f.files().includes('.env.bak'), false);
  assert.equal(f.files().includes('attempt-1.9.0.json'), false);
  writeFileSync(join(f.root, 'attempt-1.9.0.json'), '{}');
  const started = f.run(['--version', '1.9.0'], '6\n');
  assert.equal(started.status, 0, started.stderr);
  assert.equal(f.files().includes('attempt-1.9.0.json'), false);
});

test('prep rejects downgrades and residual migration containers before changing files', t => {
  const f = installation(t), original = f.read('.env');
  assert.notEqual(f.run(['--version', '1.8.99', '--prep']).status, 0);
  f.configure({ container: { State: { Status: 'exited', ExitCode: 1 } } });
  assert.notEqual(f.run(['--version', '1.9.0', '--prep']).status, 0);
  assert.equal(f.read('.env'), original);
  assert.equal(f.files().includes('.env.bak'), false);
  f.configure({ container: null });
  const result = f.run(['--version', '1.9.0', '--prep']);
  assert.equal(result.status, 0, result.stderr);
});

test('full release orders pull, stop, backup, migrate, readiness; dump failure never migrates or starts', t => {
  for (const fail of ['dump', null]) {
    const f = installation(t);
    assert.equal(f.run(['--version', '1.10.0', '--prep']).status, 0);
    f.configure({ running: true, fail });
    const result = f.run(['--version', '1.10.0', '--full-release']);
    const actions = f.calls().map(c => c.args).flatMap(args => {
      if (args.includes('pull')) return ['pull'];
      if (args.includes('stop')) return ['stop'];
      if (args.at(-1).includes('pg_dump')) return ['backup'];
      if (args.includes('run')) return ['migrate'];
      if (args.includes('up') && args.includes('api')) return ['start'];
      return [];
    });
    assert.equal(result.status === 0, !fail, result.stderr);
    assert.deepEqual(actions, fail ? ['pull', 'stop', 'backup'] : ['pull', 'stop', 'backup', 'migrate', 'start']);
    assert.equal(f.files().includes('attempt-1.10.0.json'), !!fail);
  }
});

test('failed migration freezes the stopped backup across retries; failed restore stays stopped and unarchived', t => {
  const f = installation(t), original = f.read('.env');
  assert.equal(f.run(['--version', '1.10.0', '--prep']).status, 0);
  f.configure({ migrateExit: 7, running: true });
  assert.notEqual(f.run(['--version', '1.10.0', '--full-release']).status, 0);
  const dump = f.read('backup-1.10.0.dump');
  assert.equal(active(f).migration.status, 'failed');
  assert.notEqual(f.run(['--version', '1.10.0', '--backup']).status, 0);
  // Simulate the operator explicitly removing this attempt's failed container.
  f.configure({ container: null });
  assert.notEqual(f.run(['--version', '1.10.0', '--full-release']).status, 0);
  assert.equal(f.read('backup-1.10.0.dump'), dump);
  assert.equal(f.calls().filter(c => c.args.at(-1).includes('pg_dump')).length, 1);
  f.configure({ fail: 'restore' });
  assert.notEqual(f.run(['--version', '1.10.0', '--cancel']).status, 0);
  assert.equal(JSON.parse(f.read('docker.json')).running, false);
  assert.ok(f.files().includes('attempt-1.10.0.json'));
  assert.equal(f.read('backup-1.10.0.dump'), dump);
  f.configure({ fail: null });
  const cancel = f.run(['--version', '1.10.0', '--cancel']);
  assert.equal(cancel.status, 0, cancel.stderr);
  assert.equal(f.read('.env'), original);
  assert.equal(archived(f).length, 1);
  assert.ok(JSON.parse(f.read('docker.json')).container, 'cancel must retain failed migration logs');
  assert.notEqual(f.run(['--version', '1.10.0', '--prep']).status, 0);
});

test('exited success is reconciled before cancel, while running or missing outcomes never restore', t => {
  for (const state of ['exited', 'running', 'missing']) {
    const f = installation(t);
    assert.equal(f.run(['--version', '1.10.0', '--prep']).status, 0);
    const attempt = active(f);
    attempt.migration.status = 'running';
    writeFileSync(join(f.root, 'attempt-1.10.0.json'), JSON.stringify(attempt));
    f.configure({ container: state === 'missing' ? null : { State: { Status: state, ExitCode: 0 }, Config: { Labels: { 'pin-glide.release': '1.10.0' } } } });
    const cancel = f.run(['--version', '1.10.0', '--cancel']);
    assert.notEqual(cancel.status, 0);
    assert.equal(f.calls().some(c => c.args.includes('pull')), false);
    assert.match(f.read('.env'), /^RELEASE=1\.10\.0$/m);
    if (state === 'exited') {
      assert.equal(active(f).migration.status, 'succeeded');
      assert.equal(JSON.parse(f.read('docker.json')).container, null);
    }
  }
});

test('initial deployment starts infrastructure without dumping or deleting existing data; no-baseline cancel cannot restore', t => {
  const f = installation(t);
  assert.equal(f.run(['--version', '1.10.0', '--prep', '--initial-install']).status, 0);
  f.configure({ migrateExit: 5 });
  assert.notEqual(f.run(['--version', '1.10.0', '--initial-deploy']).status, 0);
  assert.notEqual(f.run(['--version', '1.10.0', '--cancel']).status, 0);
  const calls = f.calls().map(c => c.args);
  assert.ok(calls.some(args => args.includes('up') && args.includes('postgres')));
  assert.equal(calls.some(args => args.at(-1).includes('pg_dump') || args.at(-1).includes('dropdb')), false);
  assert.ok(f.files().includes('attempt-1.10.0.json'));
});

test('interrupted prep preserves its original backup and valid preparing JSON without rewriting the environment', t => {
  const f = installation(t), original = f.read('.env');
  f.configure({ interruptPrep: true });
  assert.notEqual(f.run(['--version', '1.10.0', '--prep']).status, 0);
  assert.equal(f.read('.env'), original);
  assert.equal(f.read('.env.bak'), original);
  assert.equal(active(f).phase, 'preparing');
  f.configure({ interruptPrep: false });
  assert.notEqual(f.run(['--version', '1.11.0', '--prep']).status, 0);
  assert.equal(f.read('.env.bak'), original);
});

test('JSON replacement failure retains successful container; cancel reconciles success instead of restoring', t => {
  const f = installation(t);
  assert.equal(f.run(['--version', '1.10.0', '--prep', '--initial-install']).status, 0);
  f.configure({ failJSONAfterMigrate: true, migrateLogs: 'pin-glide-db-tools migrate: initial database accepted' });
  assert.notEqual(f.run(['--version', '1.10.0', '--initial-deploy']).status, 0);
  assert.equal(active(f).migration.status, 'running');
  assert.equal(JSON.parse(f.read('docker.json')).container.State.ExitCode, 0);
  assert.notEqual(f.run(['--version', '1.10.0', '--cancel']).status, 0);
  f.configure({ failJSONAfterMigrate: false });
  assert.notEqual(f.run(['--version', '1.10.0', '--cancel']).status, 0);
  assert.equal(active(f).migration.status, 'succeeded');
  assert.equal(JSON.parse(f.read('docker.json')).container, null);
  assert.match(f.read('.env'), /^RELEASE=1\.10\.0$/m);
  assert.equal(f.calls().some(c => c.args.at(-1).includes('dropdb')), false);
});

test('signal after container success resumes without rerunning migration and archives after readiness', t => {
  const f = installation(t);
  assert.equal(f.run(['--version', '1.10.0', '--prep', '--initial-install']).status, 0);
  f.configure({ interrupt: true });
  assert.notEqual(f.run(['--version', '1.10.0', '--initial-deploy']).status, 0);
  assert.equal(active(f).migration.status, 'running');
  f.configure({ interrupt: false });
  const resumed = f.run(['--version', '1.10.0', '--initial-deploy']);
  assert.equal(resumed.status, 0, resumed.stderr);
  assert.equal(f.calls().filter(c => c.args.includes('run')).length, 1);
  assert.equal(f.files().includes('attempt-1.10.0.json'), false);
});

test('all nine menu choices execute the same public operations as their CLI equivalents', t => {
  const actions = ['prep', 'backup', 'pull', 'stop-api', 'migrate', 'start-api', 'full-release', 'cancel', 'initial-deploy'];
  for (const [index, action] of actions.entries()) {
    const traces = [];
    for (const menu of [false, true]) {
      const f = installation(t);
      if (action !== 'prep') assert.equal(f.run(['--version', '1.10.0', '--prep', ...(action === 'initial-deploy' ? ['--initial-install'] : [])]).status, 0);
      if (action === 'migrate') {
        assert.equal(f.run(['--version', '1.10.0', '--stop-api']).status, 0);
        assert.equal(f.run(['--version', '1.10.0', '--backup']).status, 0);
      }
      const result = f.run(['--version', '1.10.0', ...(menu ? [] : [`--${action}`])], menu ? `${index + 1}\n` : undefined);
      assert.equal(result.status, 0, `${action}: ${result.stderr}`);
      traces.push(f.calls().map(c => c.args));
    }
    assert.deepEqual(traces[0], traces[1], action);
  }
});

test('online backup is not a recovery baseline even after the applications are subsequently stopped', t => {
  const f = installation(t);
  assert.equal(f.run(['--version', '1.10.0', '--prep']).status, 0);
  f.configure({ running: true });
  assert.equal(f.run(['--version', '1.10.0', '--backup']).status, 0);
  assert.equal(active(f).backup.stopped, false);
  assert.equal(f.run(['--version', '1.10.0', '--stop-api']).status, 0);
  assert.notEqual(f.run(['--version', '1.10.0', '--migrate']).status, 0);
  assert.equal(f.calls().some(c => c.args.includes('run')), false);
  assert.equal(f.run(['--version', '1.10.0', '--backup']).status, 0);
  const migrate = f.run(['--version', '1.10.0', '--migrate']);
  assert.equal(migrate.status, 0, migrate.stderr);
});

test('archive timestamp collisions never overwrite history and the same version can be prepared again', t => {
  const f = installation(t);
  writeFileSync(join(f.root, 'bin/date'), '#!/bin/sh\nprintf 20260928T120000\n');
  chmodSync(join(f.root, 'bin/date'), 0o755);
  for (let i = 0; i < 2; i++) {
    const prepared = f.run(['--version', '1.9.0', '--prep']);
    assert.equal(prepared.status, 0, prepared.stderr);
    assert.equal(f.run(['--version', '1.9.0', '--start-api']).status, 0);
  }
  assert.ok(f.files().includes('attempt-1.9.0-20260928T120000.json'));
  assert.ok(f.files().includes('attempt-1.9.0-20260928T120000-1.json'));
});

test('directory lock survives an interrupted caller while its Docker operation is still active', async t => {
  const f = installation(t);
  assert.equal(f.run(['--version', '1.10.0', '--prep']).status, 0);
  f.configure({ hold: true });
  const first = spawn('bash', [script, '--version', '1.10.0', '--pull'], { cwd: f.root, env: f.env });
  const closed = once(first, 'close');
  await once(first.stdout, 'data');
  first.kill('SIGKILL');
  const second = f.run(['--version', '1.10.0', '--stop-api']);
  await closed;
  assert.notEqual(second.status, 0);
  assert.match(second.stderr, /another action is running/);
});

test('same-version migration retry invalidates an earlier partial-cancel restore checkpoint', t => {
  const f = installation(t);
  assert.equal(f.run(['--version', '1.9.0', '--prep']).status, 0);
  f.configure({ migrateExit: 7 });
  assert.notEqual(f.run(['--version', '1.9.0', '--full-release']).status, 0);
  // The second start fails after the restore checkpoint is durable.
  f.configure({ fail: 'up' });
  assert.notEqual(f.run(['--version', '1.9.0', '--cancel']).status, 0);
  assert.equal(f.calls().filter(c => c.args.at(-1).includes('dropdb')).length, 1);
  f.configure({ fail: null, container: null });
  assert.notEqual(f.run(['--version', '1.9.0', '--migrate']).status, 0);
  const cancelled = f.run(['--version', '1.9.0', '--cancel']);
  assert.equal(cancelled.status, 0, cancelled.stderr);
  assert.equal(f.calls().filter(c => c.args.at(-1).includes('dropdb')).length, 2,
    'a later partial migration requires a fresh restore from the original frozen backup');
});

test('upgrade prep compares Database Release, not .env, and preserves the original environment for cancellation', t => {
  for (const [environment, database, target, allowed] of [
    ['9.0.0', '1.9.0', '1.10.0', true],
    ['1.0.0', '1.10.0', '1.9.0', false],
    ['1.0.0', '1.10.0', '1.10.0', true],
  ]) {
    const f = installation(t, environment), original = f.read('.env');
    f.configure({ databaseVersion: database });
    const result = f.run(['--version', target, '--prep']);
    assert.equal(result.status === 0, allowed, result.stderr);
    const query = f.calls().find(c => c.args.at(-1).includes('SELECT release_version'));
    assert.ok(query);
    assert.match(query.args.at(-1), /FROM release\.release_deploy_history ORDER BY id DESC LIMIT 1/);
    if (allowed) {
      const attempt = active(f, target);
      assert.equal(attempt.initial, false);
      assert.equal(attempt.previous, environment);
      assert.equal(attempt.database_version, database);
      assert.equal(f.read('.env.bak'), original);
    } else {
      assert.equal(f.read('.env'), original);
      assert.equal(f.files().includes('.env.bak'), false);
      assert.equal(f.files().some(n => n.startsWith('attempt-')), false);
    }
  }
});

test('failed, missing, or malformed Database Release refuses prep before any deployment-file writes', t => {
  for (const patch of [{ fail: 'query' }, { databaseVersion: null }, { databaseVersion: '' },
    { databaseVersion: 'v1.9.0' }, { databaseVersion: '1.9.0\n1.8.0' }]) {
    const f = installation(t), original = f.read('.env');
    writeFileSync(join(f.root, '.env.bak'), 'previous recovery environment');
    f.configure(patch);
    const result = f.run(['--version', '1.10.0', '--prep']);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /Database Release/);
    assert.equal(f.read('.env'), original);
    assert.equal(f.read('.env.bak'), 'previous recovery environment');
    assert.equal(f.files().some(n => n.startsWith('attempt-') || n.startsWith('.pin-glide-tmp.')), false);
    assert.equal(f.calls().some(c => c.args.includes('up')), false);
  }
});

test('explicit initial prep persists mode immediately and skips querying or starting PostgreSQL, including menu choice', t => {
  for (const menu of [false, true]) {
    const f = installation(t);
    f.configure({ fail: 'query', databaseVersion: null, interruptPrep: true });
    const result = f.run(['--version', '1.10.0', ...(menu ? [] : ['--prep', '--initial-install'])], menu ? '1\ny\n' : undefined);
    assert.notEqual(result.status, 0);
    const attempt = active(f);
    assert.equal(attempt.initial, true);
    assert.equal(attempt.phase, 'preparing');
    assert.equal(attempt.database_version, null);
    assert.equal(f.calls().some(c => c.args.includes('exec') || c.args.includes('up')), false);
  }
});

test('initial deployment cannot change a prepared upgrade into an initial attempt', t => {
  const f = installation(t);
  assert.equal(f.run(['--version', '1.10.0', '--prep']).status, 0);
  const original = f.read('attempt-1.10.0.json');
  const result = f.run(['--version', '1.10.0', '--initial-deploy']);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /requires --prep --initial-install/);
  assert.equal(f.read('attempt-1.10.0.json'), original);
  assert.equal(f.calls().some(c => c.args.includes('pull') || c.args.includes('up') || c.args.includes('run')), false);
  assert.notEqual(f.run(['--version', '1.10.0', '--migrate', '--initial-install']).status, 0);
});

test('only the exact initial acceptance log permits retry; a rejected initial claim cannot gain permission', t => {
  const accepted = 'pin-glide-db-tools migrate: initial database accepted';
  for (const [logs, verified] of [['already deployed at this version', false], [`prefix ${accepted}`, false],
    [`${accepted} suffix`, false], [`before\n${accepted}\nafter`, true]]) {
    const f = installation(t);
    assert.equal(f.run(['--version', '1.10.0', '--prep', '--initial-install']).status, 0);
    f.configure({ migrateExit: 7, migrateLogs: logs });
    assert.notEqual(f.run(['--version', '1.10.0', '--initial-deploy']).status, 0);
    assert.equal(active(f).migration.initial_verified === true, verified);
    f.configure({ container: null, migrateLogs: '' });
    assert.notEqual(f.run(['--version', '1.10.0', '--migrate']).status, 0);
    const runs = f.calls().filter(c => c.args.includes('run'));
    assert.equal(runs.length, 2);
    for (const call of runs) {
      assert.ok(call.args.includes('PIN_GLIDE_RELEASE_VERSION=1.10.0'));
      assert.ok(call.args.includes('PIN_GLIDE_RELEASE_INITIAL=true'));
    }
    assert.ok(runs[0].args.includes('PIN_GLIDE_RELEASE_INITIAL_RETRY=false'));
    assert.ok(runs[1].args.includes(`PIN_GLIDE_RELEASE_INITIAL_RETRY=${verified}`));
    assert.equal(active(f).migration.initial_verified === true, verified);
  }
});

test('reconciliation persists accepted initial state after interruption, before retry or successful-container removal', t => {
  for (const migrateExit of [0, 7]) {
    const f = installation(t);
    assert.equal(f.run(['--version', '1.10.0', '--prep', '--initial-install']).status, 0);
    f.configure({ interrupt: true, migrateExit, migrateLogs: 'pin-glide-db-tools migrate: initial database accepted' });
    assert.notEqual(f.run(['--version', '1.10.0', '--initial-deploy']).status, 0);
    assert.equal(active(f).migration.initial_verified, undefined);
    f.configure({ interrupt: false });
    const reconciled = f.run(['--version', '1.10.0', '--migrate']);
    assert.equal(reconciled.status === 0, migrateExit === 0, reconciled.stderr);
    assert.equal(active(f).migration.initial_verified, true);
    if (migrateExit) {
      f.configure({ container: null, migrateExit: 0 });
      const retry = f.run(['--version', '1.10.0', '--migrate']);
      assert.equal(retry.status, 0, retry.stderr);
      assert.ok(f.calls().filter(c => c.args.includes('run')).at(-1).args.includes('PIN_GLIDE_RELEASE_INITIAL_RETRY=true'));
    }
    assert.equal(JSON.parse(f.read('docker.json')).container, null);
    assert.equal(active(f).migration.initial_verified, true);
  }
});

test('upgrades explicitly pass their target and false initial/retry flags to migrate', t => {
  const f = installation(t);
  assert.equal(f.run(['--version', '1.10.0', '--prep']).status, 0);
  const result = f.run(['--version', '1.10.0', '--full-release']);
  assert.equal(result.status, 0, result.stderr);
  const args = f.calls().find(c => c.args.includes('run')).args;
  assert.deepEqual(args.slice(-2), ['db-tools', 'migrate']);
  assert.ok(args.includes('PIN_GLIDE_RELEASE_VERSION=1.10.0'));
  assert.ok(args.includes('PIN_GLIDE_RELEASE_INITIAL=false'));
  assert.ok(args.includes('PIN_GLIDE_RELEASE_INITIAL_RETRY=false'));
});

test('scheduled jobs are held and drained before the backup and restored after a successful start', t => {
  const f = installation(t);
  f.configure({ cron: { jobs: { '1': true, '2': false }, running: 0 } });
  assert.equal(f.run(['--version', '1.10.0', '--prep']).status, 0);
  const result = f.run(['--version', '1.10.0', '--full-release']);
  assert.equal(result.status, 0, result.stderr);
  const commands = f.calls().map(c => c.args.at(-1) ?? '');
  const held = commands.indexOf('UPDATE cron.job SET active = false');
  assert.ok(held >= 0, 'the release must deactivate scheduled jobs');
  assert.ok(held < commands.findIndex(command => command.includes('pg_dump')), 'jobs must be held before the backup');
  assert.deepEqual(JSON.parse(f.read('docker.json')).cron.jobs, { '1': true, '2': false },
    'the successful start must restore the intended job state');
  const attempt = JSON.parse(f.read(archived(f)[0]));
  assert.equal(attempt.cron.status, 'restored');
  assert.deepEqual(attempt.cron.jobs, { '1': true, '2': false });
});

test('scheduled SQL that never exits cannot authorize migration and blocks the stopped-writer baseline', t => {
  const f = installation(t);
  f.configure({ cron: { jobs: { '1': true }, running: 1 } });
  assert.equal(f.run(['--version', '1.10.0', '--prep']).status, 0);
  const stop = f.run(['--version', '1.10.0', '--stop-api']);
  assert.notEqual(stop.status, 0);
  assert.match(stop.stderr, /scheduled SQL has not exited/);
  assert.equal(f.run(['--version', '1.10.0', '--backup']).status, 0);
  assert.equal(active(f).backup.stopped, false, 'an undrained writer is not a stopped baseline');
  assert.notEqual(f.run(['--version', '1.10.0', '--migrate']).status, 0);
  assert.equal(f.calls().some(c => c.args.includes('run')), false);
});

test('pg_cron appearing after scheduling control blocks the recovery baseline instead of guessing', t => {
  const f = installation(t);
  assert.equal(f.run(['--version', '1.10.0', '--prep']).status, 0);
  assert.equal(f.run(['--version', '1.10.0', '--stop-api']).status, 0);
  f.configure({ cron: { jobs: { '1': true }, running: 0 } });
  const backup = f.run(['--version', '1.10.0', '--backup']);
  assert.notEqual(backup.status, 0);
  assert.match(backup.stderr, /pg_cron state is unknown/);
  assert.equal(JSON.parse(f.read('docker.json')).cron.jobs['1'], true, 'unknown state must not be silently paused or ignored');
});
