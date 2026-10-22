import assert from 'node:assert/strict';
import test from 'node:test';

import { applySmokeEnvironment, ensureSmokeDeployment, installationAction } from './provision.mjs';
import { TARGET } from '../harness/target.mjs';

test('smoke provisioning patches only its deployment values and keeps every other setting', () => {
  const source = [
    '# Deployment template',
    'COMPOSE_PROJECT_NAME=pin-glide-service',
    'RELEASE=0.1.6',
    'POSTGRES_USER=pin_glide',
    'POSTGRES_PASSWORD=secret',
    'PIN_GLIDE_API_BIND=0.0.0.0',
    'PIN_GLIDE_SNAPSHOT_PATH=/mnt/share/workspace/snapshots/pin-glide',
    '',
  ].join('\n');
  const result = applySmokeEnvironment(source, TARGET);
  assert.match(result, /^COMPOSE_PROJECT_NAME=pin-glide-smoke$/m);
  assert.match(result, /^RELEASE=0\.1\.6$/m);
  assert.match(result, /^PIN_GLIDE_API_BIND=127\.0\.0\.1$/m);
  assert.match(result, /^PIN_GLIDE_SNAPSHOT_PATH=\/tmp\/snapshots\/pin-glide$/m);
  assert.match(result, /^POSTGRES_PASSWORD=secret$/m);
  assert.equal(result.match(/^RELEASE=/gm).length, 1);
  assert.match(result, /^RELEASE=0\.1\.6$/m);
});

test('smoke provisioning adds missing values and rejects ambiguous duplicates', () => {
  const result = applySmokeEnvironment('POSTGRES_USER=pin_glide\n', TARGET);
  assert.match(result, /^POSTGRES_USER=pin_glide$/m);
  assert.match(result, /^COMPOSE_PROJECT_NAME=pin-glide-smoke$/m);
  assert.match(result, /^PIN_GLIDE_API_BIND=127\.0\.0\.1$/m);
  assert.match(result, /^PIN_GLIDE_SNAPSHOT_PATH=\/tmp\/snapshots\/pin-glide$/m);
  assert.throws(
    () => applySmokeEnvironment(
      'COMPOSE_PROJECT_NAME=one\nCOMPOSE_PROJECT_NAME=two\n',
      TARGET,
    ),
    /duplicate COMPOSE_PROJECT_NAME/,
  );
});

test('smoke provisioning chooses initial, redeploy or a declared upgrade', () => {
  assert.equal(installationAction('', '0.1.6', ['0.1.5']), 'initial');
  assert.equal(installationAction('0.1.6', '0.1.6', ['0.1.5']), 'redeploy');
  assert.equal(installationAction('0.1.5', '0.1.6', ['0.1.5']), 'upgrade');
  assert.throws(
    () => installationAction('0.1.4', '0.1.6', ['0.1.5']),
    /not a declared origin/,
  );
  assert.throws(() => installationAction('latest', '0.1.6', []), /not X\.Y\.Z/);
});

function fakeRemote({ volume = 'absent', release = '' } = {}) {
  const actions = [];
  let environment = [
    'COMPOSE_PROJECT_NAME=pin-glide-service',
    'RELEASE=0.1.6',
    'POSTGRES_USER=pin_glide',
    'PIN_GLIDE_API_BIND=0.0.0.0',
    'PIN_GLIDE_SNAPSHOT_PATH=/mnt/share/workspace/snapshots/pin-glide',
    '',
  ].join('\n');
  return {
    actions,
    target: TARGET,
    compose: 'sudo -n docker compose',
    async exec(script, options = {}) {
      actions.push({ script, input: options.input || '' });
      if (script.includes('docker volume inspect')) return { stdout: `${volume}\n`, stderr: '' };
      if (script.includes('to_regclass')) return { stdout: `${release ? 't' : 'f'}\n`, stderr: '' };
      if (script.includes('release_version')) return { stdout: `${release}\n`, stderr: '' };
      if (script.includes('sudo -n cat')) return { stdout: environment, stderr: '' };
      if (options.input) environment = options.input;
      return { stdout: '', stderr: '' };
    },
    async install(action, version) {
      actions.push({ action, version });
    },
    environment() {
      return environment;
    },
  };
}

test('smoke provisioning initializes a missing installation with the smoke environment', async () => {
  const remote = fakeRemote();
  const result = await ensureSmokeDeployment(remote, {
    root: '/repo',
    release: '0.1.6',
    origins: ['0.1.5'],
  });
  assert.deepEqual(result, { action: 'initial', previousRelease: null });
  assert.match(remote.environment(), /^COMPOSE_PROJECT_NAME=pin-glide-smoke$/m);
  assert.match(remote.environment(), /^PIN_GLIDE_API_BIND=127\.0\.0\.1$/m);
  assert.match(remote.environment(), /^PIN_GLIDE_SNAPSHOT_PATH=\/tmp\/snapshots\/pin-glide$/m);
  assert.deepEqual(remote.actions.slice(-2), [
    { action: '--prep --initial-install', version: '0.1.6' },
    { action: '--initial-deploy', version: '0.1.6' },
  ]);
});

test('smoke provisioning redeploys or upgrades an existing installation', async () => {
  const redeploy = fakeRemote({ volume: 'present', release: '0.1.6' });
  assert.deepEqual(await ensureSmokeDeployment(redeploy, {
    root: '/repo',
    release: '0.1.6',
    origins: ['0.1.5'],
  }), { action: 'redeploy', previousRelease: '0.1.6' });
  assert.deepEqual(redeploy.actions.slice(-2), [
    { action: '--prep', version: '0.1.6' },
    { action: '--full-release', version: '0.1.6' },
  ]);

  const upgrade = fakeRemote({ volume: 'present', release: '0.1.5' });
  assert.deepEqual(await ensureSmokeDeployment(upgrade, {
    root: '/repo',
    release: '0.1.6',
    origins: ['0.1.5'],
  }), { action: 'upgrade', previousRelease: '0.1.5' });
  assert.deepEqual(upgrade.actions.slice(-2), [
    { action: '--prep', version: '0.1.6' },
    { action: '--full-release', version: '0.1.6' },
  ]);
});
