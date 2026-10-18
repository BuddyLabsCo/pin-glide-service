import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { once } from 'node:events';
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

function installation(t) {
  const root = mkdtempSync(join(tmpdir(), "pin-glide snapshot's %-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const bin = join(root, 'bin'), snapshots = join(root, 'snapshots');
  mkdirSync(bin); mkdirSync(snapshots);
  copyFileSync(new URL('../../install.sh', import.meta.url), join(root, 'install.sh'));
  writeFileSync(join(root, 'compose.yaml'), 'name: snapshot-test\nservices: {}\n');
  writeFileSync(join(root, 'attempt-9.0.0.json'), '{"keep":"unchanged"}');
  writeFileSync(join(root, 'cron'), '17 3 * * * echo unrelated\n# pin-glide-snapshot:another-project\n');
  writeFileSync(join(root, 'calls'), '');
  writeFileSync(join(bin, 'docker'), `#!/usr/bin/env node
const fs=require('node:fs'), args=process.argv.slice(2), root=process.env.FIXTURE;
fs.appendFileSync(root+'/calls',JSON.stringify(args)+'\\n');
if(args.includes('run')) { const files=args.flatMap((x,i)=> x==='-f'?[args[i+1]]:[]); fs.writeFileSync(root+'/override',fs.readFileSync(files.at(-1))); }
if(args.includes('--environment')) console.log(fs.readFileSync(root+'/.env','utf8'));
else if(args.includes('config')) console.log(JSON.stringify({name:'snapshot-test'}));
else if(args.includes('run')) {
  if(process.env.SNAPSHOT_FAIL) process.exit(42);
  if(process.env.SNAPSHOT_HOLD) { console.log('holding'); setTimeout(()=>{},1200); }
} else process.exit(43);
`);
  writeFileSync(join(bin, 'crontab'), `#!/usr/bin/env node
const fs=require('node:fs'), root=process.env.FIXTURE;
if(process.env.CRON_FAIL) { console.error('permission denied'); process.exit(1); }
if(process.argv[2]==='-l' && !fs.readFileSync(root+'/cron','utf8')) { console.error('no crontab for fixture'); process.exit(1); }
if(process.argv[2]==='-l') console.log(fs.readFileSync(root+'/cron','utf8').trimEnd());
else fs.copyFileSync(process.argv[2],root+'/cron');
`);
  for (const command of ['docker', 'crontab']) chmodSync(join(bin, command), 0o755);
  const env = { ...process.env, PATH: `${bin}:${process.env.PATH}`, FIXTURE: root,
    PIN_GLIDE_SNAPSHOT_PATH: '/inherited-must-not-win', PIN_GLIDE_SNAPSHOT_TIME: 'bad', PIN_GLIDE_SNAPSHOT_KEEP: 'bad' };
  const configure = (patch = {}) => writeFileSync(join(root, '.env'),
    Object.entries({ RELEASE: '1.0.0', PIN_GLIDE_SNAPSHOT_PATH: snapshots, PIN_GLIDE_SNAPSHOT_TIME: '03:07', PIN_GLIDE_SNAPSHOT_KEEP: '2', ...patch })
      .map(([key, value]) => `${key}=${value}`).join('\n')+'\n');
  configure();
  const run = (action, overrides = {}, input) => spawnSync('bash', [join(root, 'install.sh'), ...action], {
    cwd: tmpdir(), env: { ...env, ...overrides }, input, encoding: 'utf8', timeout: 10000,
  });
  const read = name => readFileSync(join(root, name), 'utf8');
  return { root, env, configure, run, read, calls: () => read('calls').trim().split('\n').filter(Boolean).map(JSON.parse) };
}

test('snapshot scheduling is idempotent and disables only this project entry without release state changes', t => {
  const f = installation(t), original = f.read('cron'), attempt = f.read('attempt-9.0.0.json'), environment = f.read('.env');
  const enabled = f.run(['--enable-auto-snapshot']);
  assert.equal(enabled.status, 0, enabled.stderr);
  const cron = f.read('cron');
  assert.ok(cron.startsWith(original));
  assert.match(cron, /7 3 \* \* \* .*install\.sh.*--make-snapshot.*# pin-glide-snapshot:snapshot-test/);
  const command = cron.split('\n').find(line => line.includes('--make-snapshot'))
    .replace(/^\d+ \d+ \* \* \* /, '').replace(/\\%/g, '%');
  const fromCron = spawnSync('bash', ['-c', command], { cwd: tmpdir(), env: f.env, encoding: 'utf8' });
  assert.equal(fromCron.status, 0, fromCron.stderr);
  assert.equal(f.run(['--enable-auto-snapshot']).status, 0);
  assert.equal(f.read('cron'), cron);
  assert.equal(f.run(['--disable-auto-snapshot']).status, 0);
  assert.equal(f.read('cron'), original);
  assert.equal(f.run(['--disable-auto-snapshot']).status, 0);
  assert.equal(f.read('cron'), original);
  assert.equal(f.read('attempt-9.0.0.json'), attempt);
  assert.equal(f.read('.env'), environment);
  assert.equal(readdirSync(f.root).some(n => n.endsWith('.bak')), false);
});

test('enable rejects invalid directory, time and retention before replacing crontab', t => {
  for (const patch of [{ PIN_GLIDE_SNAPSHOT_PATH: '' }, { PIN_GLIDE_SNAPSHOT_PATH: '/nonexistent-snapshot-132' },
    { PIN_GLIDE_SNAPSHOT_TIME: '24:00' }, { PIN_GLIDE_SNAPSHOT_TIME: '3:07' }, { PIN_GLIDE_SNAPSHOT_KEEP: '0' }, { PIN_GLIDE_SNAPSHOT_KEEP: '2x' }]) {
    const f = installation(t), original = f.read('cron');
    f.configure(patch);
    const result = f.run(['--enable-auto-snapshot']);
    assert.notEqual(result.status, 0);
    assert.equal(f.read('cron'), original);
    assert.equal(f.calls().some(args => args.includes('run')), false);
  }
});

test('a crontab read error never overwrites existing jobs', t => {
  const f = installation(t), original = f.read('cron');
  assert.notEqual(f.run(['--enable-auto-snapshot'], { CRON_FAIL: '1' }).status, 0);
  assert.equal(f.read('cron'), original);
});

test('cron wrapper runs installed db-tools snapshot independently of release attempts and propagates failure', t => {
  const f = installation(t);
  const result = f.run(['--make-snapshot']);
  assert.equal(result.status, 0, result.stderr);
  const args = f.calls().find(args => args.includes('run'));
  assert.ok(args.includes('--rm') && args.includes('--no-deps') && args.includes('-T'));
  assert.deepEqual(args.slice(-2), ['db-tools', 'snapshot']);
  assert.equal(args[args.indexOf('--user') + 1], `${process.getuid()}:${process.getgid()}`);
  const config = JSON.parse(f.read('override')).services['db-tools'];
  assert.deepEqual(config.volumes, [{ type: 'bind', source: join(f.root, 'snapshots'), target: '/snapshots', bind: { create_host_path: false } }]);
  assert.deepEqual(config.environment, { PIN_GLIDE_SNAPSHOT_PATH: '/snapshots', PIN_GLIDE_SNAPSHOT_KEEP: '2' });
  assert.notEqual(f.run(['--make-snapshot'], { SNAPSHOT_FAIL: '1' }).status, 0);
  assert.equal(f.read('attempt-9.0.0.json'), '{"keep":"unchanged"}');
  assert.deepEqual(readdirSync(join(f.root, 'snapshots')), []);
});

test('snapshot lock excludes another snapshot but is independent of the release directory lock', async t => {
  const f = installation(t);
  const releaseLock = spawn('flock', [f.root, 'sh', '-c', 'echo locked; cat'], { stdio: ['pipe', 'pipe', 'pipe'] });
  t.after(() => releaseLock.kill());
  await once(releaseLock.stdout, 'data');
  const snapshot = spawn('bash', [join(f.root, 'install.sh'), '--make-snapshot'], {
    cwd: tmpdir(), env: { ...f.env, SNAPSHOT_HOLD: '1' }, stdio: ['ignore', 'pipe', 'pipe'],
  });
  t.after(() => snapshot.kill());
  await Promise.race([once(snapshot.stdout, 'data'), once(snapshot, 'exit').then(([code]) => { throw new Error(`snapshot exited before running: ${code}`); })]);
  const competing = f.run(['--make-snapshot']);
  assert.notEqual(competing.status, 0);
  assert.match(competing.stderr, /snapshot.*running/);
  const [code] = await once(snapshot, 'exit');
  assert.equal(code, 0);
});


test('snapshot menu choices work without a version, and an absent crontab can be initialized', t => {
  const f = installation(t);
  writeFileSync(join(f.root, 'cron'), '');
  const enabled = f.run([], {}, '10\n');
  assert.equal(enabled.status, 0, enabled.stderr);
  assert.equal(f.read('cron').split('\n').filter(line => line.includes('--make-snapshot')).length, 1);
  const disabled = f.run([], {}, '11\n');
  assert.equal(disabled.status, 0, disabled.stderr);
  assert.equal(f.read('cron'), '');
  assert.equal(f.read('attempt-9.0.0.json'), '{"keep":"unchanged"}');
});
