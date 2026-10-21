import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { EventEmitter } from 'node:events';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { PassThrough } from 'node:stream';
import test from 'node:test';

import { removeHostsLine, watchdogBody } from './lib/provider_faults.mjs';
import { acquireEnvironmentLock } from './lib/remote_lock.mjs';
import { RemoteEnvironment } from './lib/remote.mjs';
import { TARGET } from './lib/target.mjs';

test('the environment lock is shared and refuses a second concurrent run', async () => {
  const child = () => {
    const process = new EventEmitter();
    process.stdout = new PassThrough();
    process.stderr = new PassThrough();
    process.stdin = new PassThrough();
    process.kill = () => {};
    return process;
  };
  const first = child();
  const second = child();
  const spawned = [];
  const spawnImpl = () => {
    const process = spawned.length === 0 ? first : second;
    spawned.push(process);
    return process;
  };
  const pendingFirst = acquireEnvironmentLock({ target: TARGET, spawnImpl });
  first.stdout.write('PIN_GLIDE_SMOKE_LOCK_ACQUIRED\n');
  const lock = await pendingFirst;
  const pendingSecond = acquireEnvironmentLock({ target: TARGET, spawnImpl });
  second.stderr.write('flock: failed to get lock\n');
  second.emit('exit', 1);
  await assert.rejects(() => pendingSecond, /holds the environment lock/);
  first.emit('exit', 0);
  await lock.release();
});

test('a timed-out lock acquisition kills the SSH child instead of leaking its lock', async () => {
  const child = new EventEmitter();
  child.stdout = new PassThrough();
  child.stderr = new PassThrough();
  child.stdin = new PassThrough();
  let signal;
  child.kill = received => {
    signal = received;
  };
  await assert.rejects(
    () => acquireEnvironmentLock({
      target: TARGET,
      timeoutMs: 10,
      spawnImpl: () => child,
    }),
    /timed out acquiring/,
  );
  assert.equal(signal, 'SIGKILL');
  assert.equal(child.stdin.destroyed, true);
});

test('controlled provider faults restore the original Compose environment and network state', async () => {
  const calls = [];
  const faultPaths = {
    environment: '/tmp/pin-glide-dev-smoke-bailian.abc123.env',
    guard: '/tmp/pin-glide-dev-smoke-bailian-watchdog.abc123',
    watchdog: '/tmp/pin-glide-dev-smoke-bailian-watchdog.abc123.sh',
  };
  const remote = new RemoteEnvironment({
    target: TARGET,
    run: async (command, args) => {
      calls.push({ command, script: args.at(-1) });
      if (args.at(-1).includes('mktemp') && calls.length === 1) {
        return {
          stdout: `${faultPaths.environment}|${faultPaths.guard}|${faultPaths.watchdog}\n`,
          stderr: '',
        };
      }
      if (args.at(-1).includes('docker inspect')) {
        return {
          stdout: '["BAILIAN_API_KEY=sk-smoke-invalid-parent-key"]\n',
          stderr: '',
        };
      }
      return { stdout: '', stderr: '' };
    },
  });
  let preserved = 0;
  remote.preserveApiLogs = async () => {
    preserved++;
  };

  await assert.rejects(
    () => remote.withInvalidBailianKey(async () => {
      throw new Error('invalid parent key observed');
    }),
    /invalid parent key observed/,
  );
  assert.match(calls[0].script, /BAILIAN_API_KEY/);
  assert.match(calls[0].script, /sudo -n docker compose/);
  assert.match(calls[0].script, /force-recreate/);
  assert.match(calls[0].script, /trap cleanup EXIT/);
  assert.match(calls[0].script, /\/srv\/pin-glide-dev\/\.env/);
  assert.match(calls[0].script, /nohup/);
  assert.match(calls[0].script, /watchdog/);
  assert.match(calls.at(-1).script, /--env-file/);
  assert.match(calls.at(-1).script, /\.env/);
  assert.match(calls.at(-1).script, new RegExp(faultPaths.guard.replaceAll('.', '\\.')));
  assert.match(calls.at(-1).script, new RegExp(faultPaths.watchdog.replaceAll('.', '\\.')));
  assert.match(calls.at(-1).script, /rm -f/);
  assert.match(calls.at(-1).script, /--force-recreate --wait api/);
  assert.equal(preserved, 2);

  calls.length = 0;
  await assert.rejects(
    () => remote.withBailianConnectivityFault('ws-kea2u5v9va6wsdvh', async () => {
      throw new Error('connectivity fault observed');
    }, { idleWaitMs: 0 }),
    /connectivity fault observed/,
  );
  assert.equal(calls.length, 2);
  assert.match(calls[0].script, /\/etc\/hosts/);
  assert.match(calls[0].script, /ws-kea2u5v9va6wsdvh/);
  assert.doesNotMatch(calls[0].script, /sed -i/);
  assert.match(calls.at(-1).script, /\/etc\/hosts/);
  assert.match(calls.at(-1).script, /pin-glide-smoke-fault/);
  assert.doesNotMatch(calls.at(-1).script, /sed -i/);
  assert.equal(preserved, 4);
});

test('invalid-key verification failure restores the original API environment', async () => {
  const calls = [];
  const faultPaths = {
    environment: '/tmp/pin-glide-dev-smoke-bailian.abc123.env',
    guard: '/tmp/pin-glide-dev-smoke-bailian-watchdog.abc123',
    watchdog: '/tmp/pin-glide-dev-smoke-bailian-watchdog.abc123.sh',
  };
  let callbackRan = false;
  const remote = new RemoteEnvironment({
    target: TARGET,
    run: async (command, args) => {
      calls.push({ command, script: args.at(-1) });
      if (calls.length === 1) {
        return {
          stdout: `${faultPaths.environment}|${faultPaths.guard}|${faultPaths.watchdog}\n`,
          stderr: '',
        };
      }
      if (args.at(-1).includes('docker inspect')) {
        return { stdout: '["BAILIAN_API_KEY=restored"]\n', stderr: '' };
      }
      return { stdout: '', stderr: '' };
    },
  });
  remote.preserveApiLogs = async () => {};

  await assert.rejects(
    () => remote.withInvalidBailianKey(async () => {
      callbackRan = true;
    }),
    /did not reach the running API/,
  );
  assert.equal(callbackRan, false);
  assert.match(calls.at(-1).script, /--env-file/);
  assert.match(calls.at(-1).script, /force-recreate/);
});

test('the connectivity cleanup removes only its own marker from a hosts file', async t => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'pin-glide-hosts-'));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const hosts = path.join(directory, 'hosts');
  fs.writeFileSync(hosts, [
    '127.0.0.1 localhost',
    '127.0.0.1 ws-kea2u5v9va6wsdvh.cn-beijing.maas.aliyuncs.com # pin-glide-smoke-fault',
    '127.0.0.1 unrelated # pin-glide-smoke-fault-other',
    '',
  ].join('\n'));
  const script = removeHostsLine('pin-glide-smoke-fault', hosts);
  const result = spawnSync('sh', ['-ec', script], { encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(fs.readFileSync(hosts, 'utf8'), [
    '127.0.0.1 localhost',
    '127.0.0.1 unrelated # pin-glide-smoke-fault-other',
    '',
  ].join('\n'));
});

test('the invalid-key watchdog restores the original environment after a crash', async t => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'pin-glide-watchdog-'));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const composeLog = path.join(directory, 'compose.log');
  const fake = path.join(directory, 'docker');
  fs.writeFileSync(fake, `#!/bin/sh
printf '%s\\n' "$*" >> ${JSON.stringify(composeLog)}
`, { mode: 0o700 });
  const guard = path.join(directory, 'guard');
  fs.writeFileSync(guard, '');
  const body = watchdogBody(
    { target: TARGET },
    { attempts: 1, intervalSeconds: 0.05 },
  ).replaceAll('sudo -n docker compose', JSON.stringify(fake));
  const script = path.join(directory, 'watchdog.sh');
  fs.writeFileSync(script, body, { mode: 0o700 });
  const child = spawn(script, [guard], {
    stdio: 'ignore',
    env: { ...process.env, PATH: `${directory}:${process.env.PATH}` },
  });
  child.unref();

  for (let attempt = 0; attempt < 20 && fs.existsSync(guard); attempt++) {
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  assert.equal(fs.existsSync(guard), false);
  assert.match(fs.readFileSync(composeLog, 'utf8'), /--env-file \/?srv\/pin-glide-dev\/\.env/);
});

test('remote SQL is sent through stdin and never embedded in the SSH command', async () => {
  const calls = [];
  const remote = new RemoteEnvironment({
    target: TARGET,
    run: async (command, args, options) => {
      calls.push({ command, args, options });
      return { command, args, code: 0, stdout: '{}', stderr: '' };
    },
  });
  const secret = 'smoke-secret-token';
  await remote.sql('SELECT 1; -- token should stay in stdin');
  assert.equal(calls.length, 1);
  assert.equal(calls[0].command, 'ssh');
  assert(!calls[0].args.at(-1).includes('token should stay in stdin'));
  assert.equal(calls[0].options.input, 'SELECT 1; -- token should stay in stdin');
  assert(!secret.includes(calls[0].args.at(-1)));
});

test('release installer actions use noninteractive sudo', async () => {
  const calls = [];
  const remote = new RemoteEnvironment({
    target: TARGET,
    run: async (command, args, options) => {
      calls.push({ command, args, options });
      return { stdout: '', stderr: '' };
    },
  });
  await remote.install('--prep', '1.2.3');
  assert.equal(calls[0].command, 'ssh');
  assert.match(
    calls[0].args.at(-1),
    /^cd '\/srv\/pin-glide-dev' && sudo -n bash '\/srv\/pin-glide-dev\/install\.sh' --version '1\.2\.3' --prep$/,
  );
});

test('service logs are bounded to the run window without reading the deployment environment', async () => {
  const calls = [];
  const remote = new RemoteEnvironment({
    target: TARGET,
    run: async (command, args, options) => {
      calls.push({ command, args, options });
      return { stdout: '{"level":"info"}\n', stderr: '' };
    },
  });
  await remote.apiLogsSince('2026-10-21T04:00:00.000Z');
  assert.equal(calls[0].command, 'ssh');
  assert.match(calls[0].args.at(-1), /logs --no-color --since/);
  assert.doesNotMatch(calls[0].args.at(-1), /--tail/);
  assert.match(calls[0].args.at(-1), /--since '2026-10-21T04:00:00\.000Z'/);
  assert.match(calls[0].args.at(-1), / api$/);
  assert.doesNotMatch(calls[0].args.at(-1), /(?:cat|sed|source).*\.env/);
});

test('preserved service logs remain scannable after the API container is replaced', async () => {
  const startedAt = '2026-10-21T04:00:00.000Z';
  let current = 'first container\n';
  const remote = new RemoteEnvironment({
    target: TARGET,
    run: async () => ({ stdout: current, stderr: '' }),
  });
  remote.smokeStartedAt = startedAt;
  await remote.preserveApiLogs();
  current = 'second container\n';
  const logs = await remote.apiLogsSince(startedAt);
  assert.equal(logs.stdout, 'first container\n\nsecond container\n');
});

test('the external-writer guard excludes only the snapshot dump connection', async () => {
  const remote = new RemoteEnvironment({ target: TARGET, run: async () => ({ stdout: '' }) });
  let query = '';
  remote.sqlJSON = async statement => {
    query = statement;
    return { connections: 0 };
  };
  await remote.assertNoExternalWriters();
  assert.match(query, /coalesce\(application_name, ''\) <> 'pg_dump'/);
});

test('the schema audit parses the JSON object returned by PostgreSQL', async () => {
  const remote = new RemoteEnvironment({
    target: TARGET,
    run: async () => ({ stdout: '', stderr: '' }),
  });
  remote.ssh = async () => ({ code: 0, stdout: '{"database":"pin_glide"}\n', stderr: '' });
  assert.deepEqual(await remote.schemaAudits(), { database: 'pin_glide' });
});

test('the external-writer guard accepts no clients and rejects a remaining writer', async () => {
  const remote = new RemoteEnvironment({ target: TARGET });
  remote.sqlJSON = async () => ({ connections: 0 });
  await remote.assertNoExternalWriters();
  remote.sqlJSON = async () => ({ connections: 1 });
  await assert.rejects(() => remote.assertNoExternalWriters(), /Another database writer/);
});

test('a stopped snapshot restarts the service when snapshotting fails', async () => {
  const actions = [];
  const remote = new RemoteEnvironment({ target: TARGET });
  remote.install = async (action, version) => {
    actions.push(`${action}:${version}`);
    if (action === '--stop-api') return { code: 0, stdout: '', stderr: '' };
    return { code: 0, stdout: '', stderr: '' };
  };
  remote.assertNoExternalWriters = async () => {};
  remote.databaseWriteStats = async () => ({ inserted: 0, updated: 0, deleted: 0 });
  remote.listSnapshots = async () => [{ name: 'pin-glide-snapshot-20261020T030000.000000001Z-1.dump' }];
  remote.makeSnapshot = async () => {
    throw new Error('snapshot failed');
  };
  await assert.rejects(() => remote.createStoppedSnapshot('1.2.3'), /snapshot failed/);
  assert.deepEqual(actions, ['--prep:1.2.3', '--stop-api:1.2.3', '--start-api:1.2.3']);
});

test('a failed stopped snapshot removes only the archive reported by its command', async () => {
  const old = 'pin-glide-snapshot-20261020T030000.000000001Z-1.dump';
  const created = 'pin-glide-snapshot-20261020T040000.000000002Z-2.dump';
  const actions = [];
  const removed = [];
  let snapshots = 0;
  let writes = 0;
  const remote = new RemoteEnvironment({ target: TARGET });
  remote.install = async (action, version) => {
    actions.push(`${action}:${version}`);
  };
  remote.assertNoExternalWriters = async () => {};
  remote.listSnapshots = async () => {
    snapshots++;
    return snapshots === 1 ? [{ name: old }] : [{ name: old }, { name: created }];
  };
  remote.databaseWriteStats = async () => (++writes === 1
    ? { inserted: 0, updated: 0, deleted: 0 }
    : { inserted: 1, updated: 0, deleted: 0 });
  remote.makeSnapshot = async () => ({ stdout: `${created}\n` });
  remote.removeSnapshot = async name => {
    removed.push(name);
  };
  await assert.rejects(() => remote.createStoppedSnapshot('1.2.3'), /Database writes occurred/);
  assert.deepEqual(removed, [created]);
  assert.deepEqual(actions, ['--prep:1.2.3', '--stop-api:1.2.3', '--start-api:1.2.3']);
});

test('a failed stopped snapshot never infers ownership from a new shared-directory archive', async () => {
  const old = 'pin-glide-snapshot-20261020T030000.000000001Z-1.dump';
  const unrelated = 'pin-glide-snapshot-20261020T040000.000000002Z-2.dump';
  const removed = [];
  let snapshots = 0;
  const remote = new RemoteEnvironment({ target: TARGET });
  remote.install = async () => {};
  remote.assertNoExternalWriters = async () => {};
  remote.databaseWriteStats = async () => ({ inserted: 0, updated: 0, deleted: 0 });
  remote.listSnapshots = async () => (
    ++snapshots === 1 ? [{ name: old }] : [{ name: old }, { name: unrelated }]
  );
  remote.makeSnapshot = async () => ({ stdout: '' });
  remote.removeSnapshot = async name => {
    removed.push(name);
  };
  await assert.rejects(
    () => remote.createStoppedSnapshot('1.2.3'),
    /reported no archive/,
  );
  assert.deepEqual(removed, []);
});

test('a successful stopped snapshot returns its own archive and never infers another run', async () => {
  const old = 'pin-glide-snapshot-20261020T030000.000000001Z-1.dump';
  const created = 'pin-glide-snapshot-20261020T040000.000000002Z-2.dump';
  const actions = [];
  let snapshots = 0;
  const remote = new RemoteEnvironment({ target: TARGET });
  remote.install = async (action, version) => {
    actions.push(`${action}:${version}`);
  };
  remote.assertNoExternalWriters = async () => {};
  remote.databaseWriteStats = async () => ({ inserted: 0, updated: 0, deleted: 0 });
  remote.listSnapshots = async () => (++snapshots === 1 ? [{ name: old }] : [{ name: old }, { name: created }]);
  remote.makeSnapshot = async () => ({ stdout: `${created}\n` });
  let held = false;
  assert.equal(await remote.createStoppedSnapshot('1.2.3', {
    afterStop: async () => {
      held = true;
    },
  }), created);
  assert.equal(held, true);
  assert.deepEqual(actions, ['--prep:1.2.3', '--stop-api:1.2.3', '--start-api:1.2.3']);

  const missing = new RemoteEnvironment({ target: TARGET });
  missing.install = async () => {};
  missing.assertNoExternalWriters = async () => {};
  missing.databaseWriteStats = async () => ({ inserted: 0, updated: 0, deleted: 0 });
  missing.listSnapshots = async () => [{ name: old }];
  missing.makeSnapshot = async () => ({ stdout: '' });
  await assert.rejects(() => missing.createStoppedSnapshot('1.2.3'), /reported no archive/);
});

test('a held-state check failure restarts the service before snapshotting', async () => {
  const actions = [];
  const remote = new RemoteEnvironment({ target: TARGET });
  remote.install = async (action, version) => {
    actions.push(`${action}:${version}`);
  };
  remote.makeSnapshot = async () => {
    throw new Error('snapshot must not run');
  };
  await assert.rejects(
    () => remote.createStoppedSnapshot('1.2.3', {
      afterStop: async () => {
        throw new Error('unknown writer state');
      },
    }),
    /unknown writer state/,
  );
  assert.deepEqual(actions, ['--prep:1.2.3', '--stop-api:1.2.3', '--start-api:1.2.3']);
});

test('the snapshot path refuses a connected writer before invoking the dump', async () => {
  const actions = [];
  const remote = new RemoteEnvironment({ target: TARGET });
  remote.install = async action => {
    actions.push(action);
  };
  remote.assertNoExternalWriters = async () => {
    throw new Error('Another database writer is still connected');
  };
  remote.makeSnapshot = async () => {
    throw new Error('snapshot must not run with a writer');
  };
  await assert.rejects(
    () => remote.createStoppedSnapshot('1.2.3'),
    /Another database writer/,
  );
  assert.deepEqual(actions, ['--prep', '--stop-api', '--start-api']);
});
