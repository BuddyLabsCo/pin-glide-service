import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';

import { summarizeCompose } from './lib/installation.mjs';
import { TARGET } from './lib/target.mjs';
import { cleanupInstallations, parseArguments, rewriteCompose } from './published-install.mjs';

function composeFixture(version = '1.2.3') {
  return {
    name: TARGET.project,
    services: {
      postgres: {
        image: 'ghcr.io/buddylabsco/pin-glide-postgres:18-1.6.8',
        environment: {
          POSTGRES_USER: 'pin_glide',
          POSTGRES_PASSWORD: 'secret',
          POSTGRES_DB: 'pin_glide',
        },
      },
      'db-tools': {
        image: `ghcr.io/buddylabsco/pin-glide-db-tools:${version}`,
        environment: {
          PIN_GLIDE_ENV: 'production',
          PIN_GLIDE_DATABASE_URL: 'postgres://pin_glide:secret@postgres:5432/pin_glide?sslmode=disable',
        },
      },
      api: {
        image: `ghcr.io/buddylabsco/pin-glide-api:${version}`,
        environment: {
          PIN_GLIDE_ENV: 'production',
          PIN_GLIDE_API_ADDRESS: ':8080',
          PIN_GLIDE_DATABASE_URL: 'postgres://pin_glide:secret@postgres:5432/pin_glide?sslmode=disable',
          PIN_GLIDE_BAILIAN_API_KEY: 'parent-secret',
          PIN_GLIDE_QQ_APP_ID: 'qq-app',
          PIN_GLIDE_QQ_APP_KEY: 'qq-key',
        },
        ports: [{ mode: 'ingress', target: 8080, published: '33489', protocol: 'tcp' }],
      },
    },
  };
}

test('the installation summary verifies exact project, port, database and release images without retaining secrets', () => {
  const summary = summarizeCompose(composeFixture(), TARGET, '1.2.3');
  assert.equal(summary.project, TARGET.project);
  assert.equal(summary.apiAddress, ':8080');
  assert.equal(summary.publishedPort, '33489');
  assert.equal(summary.database, 'pin_glide');
  assert.equal(summary.images.api, 'ghcr.io/buddylabsco/pin-glide-api:1.2.3');
  assert(!JSON.stringify(summary).includes('secret'));

  const wrongPort = composeFixture();
  wrongPort.services.api.ports[0].published = '8080';
  assert.throws(() => summarizeCompose(wrongPort, TARGET, '1.2.3'), /33489/);

  const wrongDatabase = composeFixture();
  wrongDatabase.services.api.environment.PIN_GLIDE_DATABASE_URL =
    'postgres://pin_glide:secret@postgres:5432/production?sslmode=disable';
  assert.throws(() => summarizeCompose(wrongDatabase, TARGET, '1.2.3'), /pin_glide/);

  const wrongImage = composeFixture();
  wrongImage.services.api.image = 'ghcr.io/buddylabsco/pin-glide-api:latest';
  assert.throws(() => summarizeCompose(wrongImage, TARGET, '1.2.3'), /immutable release image/);

  const unrelated = composeFixture();
  unrelated.services.debug = { image: 'example/debug:latest' };
  assert.throws(() => summarizeCompose(unrelated, TARGET, '1.2.3'), /unrelated service/);
});

test('published installation arguments require explicit origin and target versions', () => {
  assert.deepEqual(parseArguments([
    '--integration-ready',
    '--from',
    '0.1.5',
    '--to',
    '0.1.6',
  ]), {
    mode: 'integration-ready',
    from: '0.1.5',
    to: '0.1.6',
  });
  assert.throws(() => parseArguments(['--check', '--to', '0.1.6']), /explicit --from/);
  assert.throws(() => parseArguments(['--check', '--from', 'latest', '--to', '0.1.6']), /X\.Y\.Z/);
  assert.throws(() => parseArguments(['--check', '--check', '--from', '0.1.5', '--to', '0.1.6']), /exactly one/);
});

test('published installation Compose keeps release tags but removes host-wide resources', () => {
  const source = composeFixture();
  for (const service of Object.values(source.services)) {
    Object.assign(service, {
      build: '.',
      networks: ['shared'],
      profiles: ['unsafe'],
      volumes: ['./host:/host'],
    });
  }
  source.services.api.ports = [{ target: 8080, published: '33489' }];
  const config = rewriteCompose(source, 'pin-glide-published-test');
  assert.equal(config.name, 'pin-glide-published-test');
  assert.equal(config.services.api.ports, undefined);
  assert.equal(config.services.api.image, 'ghcr.io/buddylabsco/pin-glide-api:${RELEASE}');
  assert.equal(config.services['db-tools'].container_name, 'pin-glide-published-test-db-tools');
  for (const service of Object.values(config.services)) {
    assert.equal(service.build, undefined);
    assert.equal(service.networks, undefined);
    assert.equal(service.profiles, undefined);
  }
  assert.equal(config.services.api.volumes, undefined);
  assert.equal(config.services['db-tools'].volumes, undefined);
  assert.deepEqual(config.services.postgres.volumes, ['database:/var/lib/postgresql']);
  assert.deepEqual(Object.keys(config.volumes), ['database']);
});

test('failed installation cleanup retains the project files needed to retry', async t => {
  const work = fs.mkdtempSync(path.join(os.tmpdir(), 'pin-glide-cleanup-test-'));
  t.after(() => fs.rmSync(work, { recursive: true, force: true }));
  fs.writeFileSync(path.join(work, '.env'), 'RELEASE=1.2.3\n');
  fs.writeFileSync(path.join(work, 'compose.yaml'), '{}\n');
  const errors = await cleanupInstallations({
    installations: [{
      directory: work,
      async run() {
        throw new Error('compose down failed');
      },
    }],
    work,
    env: {},
  });
  assert.equal(errors.length, 1);
  assert.equal(fs.existsSync(path.join(work, '.env')), true);
  assert.equal(fs.existsSync(path.join(work, 'compose.yaml')), true);
});
