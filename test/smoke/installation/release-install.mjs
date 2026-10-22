#!/usr/bin/env node
// Real public-CLI regression coverage for the Compose installation. No Docker,
// database, or installer mocks.
// Run only after the implementation owner declares integration-ready:
//   node test/smoke/installation/release-install.mjs --integration-ready
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../..');
const mode = process.argv[2];
assert.equal(mode, '--integration-ready', 'Use --integration-ready');
const env = Object.fromEntries(Object.entries(process.env).filter(([key]) =>
  !/^(COMPOSE_|PIN_GLIDE_|POSTGRES_|RELEASE$)/.test(key)));
const project = `pin-glide-smoke-${randomBytes(8).toString('hex')}`;
const work = fs.mkdtempSync(path.join(os.tmpdir(), `${project}-`));
const logfile = path.join(work, 'commands.log');
const evidence = { project, work, real: [], mocked: [], checks: [], cleanup: [] };
const images = new Set();
const installations = [];
const postgresImage = '18-1.6.8';
let step = 0;

function run(command, args, options = {}) {
  const { cwd = work, input, allowFailure = false, inheritedRelease = false } = options;
  const result = spawnSync(command, args, {
    cwd, input, encoding: 'utf8', timeout: 900_000, maxBuffer: 32 * 1024 * 1024,
    env: inheritedRelease ? { ...env, RELEASE: '999.999.999' } : env,
  });
  fs.appendFileSync(logfile, `\n$ ${command} ${args.join(' ')} [cwd=${cwd}]\n${result.stdout || ''}${result.stderr || ''}\nexit=${result.status}\n`);
  if (!allowFailure) assert.equal(result.status, 0,
    `${command} ${args.join(' ')} failed: ${result.error || result.stderr || result.stdout}; log: ${logfile}`);
  return result;
}
const docker = (...args) => run('docker', args).stdout.trim();
const write = (name, text) => fs.writeFileSync(name, text);
const json = name => JSON.parse(fs.readFileSync(name, 'utf8'));
const hash = name => createHash('sha256').update(fs.readFileSync(name)).digest('hex');
async function check(name, action) {
  console.log(`[${++step}] ${name}`);
  await action();
  evidence.checks.push(name);
}
function compose(install, ...args) {
  return run('docker', ['compose', '--env-file', '.env', '-f', 'compose.yaml', ...args], { cwd: install.dir }).stdout.trim();
}
function cli(install, version, action, { fails = false, menu, initial = false } = {}) {
  const result = run('bash', ['install.sh', '--version', version, ...(menu === undefined ? [action] : []), ...(initial ? ['--initial-install'] : [])], {
    cwd: install.dir, inheritedRelease: true, allowFailure: fails, input: menu === undefined ? undefined : `${menu}\n`,
  });
  if (fails) {
    assert.notEqual(result.status, null, 'Expected a controlled failure, not a timeout or signal');
    assert.notEqual(result.status, 0, `${action || `menu ${menu}`} unexpectedly succeeded`);
  }
  return `${result.stdout}${result.stderr}`;
}
function active(install, version) {
  return path.join(install.dir, `attempt-${version}.json`);
}
function archives(install, version) {
  return fs.readdirSync(install.dir).filter(name => name.startsWith(`attempt-${version}-`) && name.endsWith('.json'));
}
function archived(install, version, count) {
  assert(!fs.existsSync(active(install, version)), 'Successful start left an active attempt');
  assert.equal(archives(install, version).length, count, 'Archive missing, duplicated, or overwritten');
  for (const name of archives(install, version)) assert.equal(json(path.join(install.dir, name)).target, version);
}
function saveCompose(install, configure = () => {}) {
  const config = structuredClone(install.config);
  configure(config);
  write(path.join(install.dir, 'compose.yaml'), JSON.stringify(config, null, 2));
}
function sql(install, statement) {
  return compose(install, 'exec', '-T', 'postgres', 'psql', '-X', '-v', 'ON_ERROR_STOP=1', '-U', 'pin_glide', '-d', 'pin_glide', '-Atc', statement);
}
function jobs(install) {
  return sql(install, "SELECT jobname || '=' || active FROM cron.job ORDER BY jobname").split('\n').filter(Boolean);
}
// The retention data script every release ships keeps one job active, and the
// 63.0.1 fixture adds its own active and deliberately inactive jobs.
const retentionJob = 'pin-glide-retention-cleanup=true';
const declaredJobs = [retentionJob, 'pin_glide_smoke_dormant=false', 'pin_glide_smoke_probe=true'];
function cronRunning(install) {
  return sql(install, "SELECT (SELECT count(*) FROM cron.job_run_details WHERE status = 'running') + (SELECT count(*) FROM pg_stat_activity WHERE application_name = 'pg_cron')");
}
function infra(install) {
  return { postgres: compose(install, 'ps', '-q', 'postgres') };
}
function stopped(install) {
  assert.equal(compose(install, 'ps', '--status', 'running', '--services', 'api'), '');
}
function ready(install, version) {
  const id = compose(install, 'ps', '-q', 'api');
  assert(id, 'api missing');
  const detail = JSON.parse(docker('inspect', id))[0];
  assert.equal(detail.State.Health.Status, 'healthy', 'api not healthy');
  assert.equal(detail.Config.Image, `${registry}/${project}/pin-glide-api:${version}`, 'Inherited RELEASE escaped isolation');
  const health = JSON.parse(compose(install, 'exec', '-T', 'api', 'wget', '-q', '-O', '-', 'http://127.0.0.1:8080/health'));
  assert.equal(health.status, 'ok');
}
function snapshot(install, filename, ...schemas) {
  // pg_dump restrict keys and object creation order change on restore. Compare the
  // complete SQL line multiset, including every schema, row, function and sequence.
  const dump = compose(install, 'exec', '-T', 'postgres', 'pg_dump', '-U', 'pin_glide', '-d', 'pin_glide', '--inserts', ...schemas.flatMap(schema => ['--schema', schema]));
  const normalized = dump.split('\n').filter(line => !/^\\(?:un)?restrict /.test(line)).sort().join('\n');
  const destination = path.join(work, filename);
  write(destination, normalized);
  return hash(destination);
}
function cleanupFailedMigration(install) {
  // This is the explicitly authorized simulation of an operator, not installer cleanup.
  const name = `${install.project}-db-tools`;
  const detail = JSON.parse(docker('inspect', name))[0];
  assert.equal(detail.Config.Labels['com.docker.compose.project'], install.project);
  assert.equal(detail.State.Status, 'exited');
  assert.notEqual(detail.State.ExitCode, 0);
  docker('rm', detail.Id);
}
function retainFailure(install, version) {
  assert.equal(json(active(install, version)).migration.status, 'failed');
  const container = JSON.parse(docker('inspect', `${install.project}-db-tools`))[0];
  assert.equal(container.State.Status, 'exited');
  assert.notEqual(container.State.ExitCode, 0);
  write(path.join(work, `${install.project}-${version}-migration.log`), docker('logs', container.Id));
}
function makeInstall(name, config) {
  const dir = path.join(work, name);
  fs.mkdirSync(dir);
  const install = { dir, project: name, config: { ...structuredClone(config), name } };
  installations.push(install);
  fs.copyFileSync(path.join(root, 'install.sh'), path.join(dir, 'install.sh'));
  write(path.join(dir, '.env'), `# Complete backup sentinel\nCOMPOSE_PROJECT_NAME=${name}\nRELEASE=63.0.0\nPOSTGRES_USER=pin_glide\nPOSTGRES_PASSWORD=isolated-smoke-only\nWECHAT_APP_ID=isolated-smoke-appid\nWECHAT_APP_SECRET=isolated-smoke-appsecret\nBAILIAN_API_KEY=isolated-smoke-bailian-key\n`);
  install.config.services['db-tools'].container_name = `${name}-db-tools`;
  saveCompose(install);
  return install;
}
let registry;
try {
  for (const [command, args] of [['docker', ['version', '--format', '{{.Server.Version}}']], ['docker', ['compose', 'version']], ['jq', ['--version']], ['flock', ['--version']]]) run(command, args);
  evidence.real.push('Docker Engine and Compose reachable');
  // The full integration journey is the only mode.
  {
    // Resolve only repository configuration, with a controlled interpolation environment.
    const sourceEnv = path.join(work, 'source.env');
    write(sourceEnv, `RELEASE=63.0.0\nCOMPOSE_PROJECT_NAME=${project}\nPOSTGRES_USER=pin_glide\nPOSTGRES_PASSWORD=isolated-smoke-only\nWECHAT_APP_ID=isolated-smoke-appid\nWECHAT_APP_SECRET=isolated-smoke-appsecret\nBAILIAN_API_KEY=isolated-smoke-bailian-key\n`);
    const source = JSON.parse(run('docker', ['compose', '--env-file', sourceEnv, '-f', path.join(root, 'deploy/compose/docker-compose.yml'), 'config', '--format', 'json']).stdout);
    const config = { services: {}, volumes: { database: {}, registry: {} } };
    for (const service of ['postgres', 'api', 'db-tools']) {
      assert(source.services[service], `Missing real Compose service: ${service}`);
      const copy = structuredClone(source.services[service]);
      for (const key of ['build', 'ports', 'volumes', 'networks', 'container_name', 'profiles']) delete copy[key];
      if (copy.healthcheck) Object.assign(copy.healthcheck, { interval: '1s', timeout: '3s', retries: 60 });
      config.services[service] = copy;
    }
    assert.ok(!source.services.postgres.ports, 'Production PostgreSQL must not publish a host port');
    assert.match(source.services.postgres.image, /^ghcr\.io\/buddylabsco\/pin-glide-postgres:[0-9]+-[0-9.]+$/,
      'Compose must use the derived PostgreSQL/pg_cron distribution, not the base image');
    assert.match(source.services.api.stop_grace_period, /^(?:[1-9][0-9]*s|[1-9][0-9]*)$/,
      'The api needs an explicit grace period for its bounded shutdown');
    config.services.postgres.volumes = ['database:/var/lib/postgresql'];
    config.services.registry = { image: 'registry:2', ports: ['127.0.0.1::5000'], volumes: ['registry:/var/lib/registry'] };
    const main = makeInstall(project, config);
    assert.equal(docker('ps', '-aq', '--filter', `label=com.docker.compose.project=${project}`), '');
    compose(main, 'up', '-d', '--no-deps', 'registry');
    registry = compose(main, 'port', 'registry', '5000');
    assert.match(registry, /^127\.0\.0\.1:\d+$/);
    for (let attempt = 0; ; attempt++) {
      try { if ((await fetch(`http://${registry}/v2/`)).ok) break; } catch { /* registry is starting */ }
      assert(attempt < 100, 'Disposable registry never became ready');
      await new Promise(resolve => setTimeout(resolve, 200));
    }
    evidence.real.push('Disposable loopback registry; no GHCR writes');
    const baseOrder = run('bash', [path.join(root, 'scripts/release/migration-order.sh')], { cwd: root }).stdout.trim().split('\n').filter(Boolean);
    for (const service of ['api', 'db-tools', 'postgres']) {
      const image = service === 'postgres' ? `${registry}/${project}/pin-glide-postgres:${postgresImage}` : `${registry}/${project}/pin-glide-${service}:base`;
      images.add(image);
      console.log(`Building real ${service} image`);
      run('docker', ['build', '--label', `pin-glide.smoke.owner=${project}`, '-f', `deploy/compose/${service}.Dockerfile`, '-t', image, '.'], { cwd: root });
      if (service === 'postgres') {
        docker('push', image);
        main.config.services.postgres.image = image;
        continue;
      }
      if (service === 'db-tools') {
        const manifest = path.join(work, 'script-manifest');
        fs.mkdirSync(manifest);
        write(path.join(manifest, 'order.txt'), baseOrder.join('\n') + '\n');
        write(path.join(manifest, 'Dockerfile'), `FROM ${image}\nCOPY order.txt /app/db/migrations/order.txt\n`);
        docker('build', '-t', image, manifest);
      }
      for (const version of ['63.0.0', '63.0.1', '63.0.2', '63.0.3', '63.0.4']) {
        const tag = `${registry}/${project}/pin-glide-${service}:${version}`;
        images.add(tag);
        docker('tag', image, tag);
      }
      main.config.services[service].image = `${registry}/${project}/pin-glide-${service}:\${RELEASE}`;
    }
    // These 63.x tags are synthetic installer-fault identities, not supported
    // historical releases; the local runner derives the real origin above.
    const fixture = path.join(work, 'migration-fixture');
    fs.mkdirSync(path.join(fixture, 'migrations/pin_glide'), { recursive: true });
    fs.mkdirSync(path.join(fixture, 'scripts'));
    write(path.join(fixture, 'Dockerfile'), `FROM ${registry}/${project}/pin-glide-db-tools:base\nCOPY migrations/ /app/db/migrations/\nCOPY scripts/ /app/db/scripts/\n`);
    write(path.join(fixture, 'migrations/pin_glide/zz_issue12_01.sql'), `
CREATE SCHEMA IF NOT EXISTS backup_probe;
CREATE TABLE IF NOT EXISTS backup_probe.records (id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, value text NOT NULL);
INSERT INTO backup_probe.records (id, value) VALUES (1, 'original one'), (2, 'original two') ON CONFLICT (id) DO NOTHING;
SELECT setval('backup_probe.records_id_seq', 41, true);
CREATE OR REPLACE FUNCTION backup_probe.answer() RETURNS integer LANGUAGE sql AS 'SELECT 42';
CREATE SCHEMA IF NOT EXISTS removed_probe;
CREATE TABLE IF NOT EXISTS removed_probe.documents (body text PRIMARY KEY);
INSERT INTO removed_probe.documents VALUES ('restore the entire schema') ON CONFLICT DO NOTHING;
`);
    // Scheduled database work belongs to Data Scripts: one job stays active and
    // one is deliberately kept inactive. The extension itself is desired schema.
    write(path.join(fixture, 'scripts/zz_issue12_01.sql'), `
SELECT cron.schedule('pin_glide_smoke_probe', '0 0 1 1 *', 'SELECT 1');
SELECT cron.schedule('pin_glide_smoke_dormant', '0 0 1 1 *', 'SELECT 1');
UPDATE cron.job SET active = false WHERE jobname = 'pin_glide_smoke_dormant';
`);
    write(path.join(fixture, 'migrations/order.txt'), [...baseOrder, 'db/migrations/pin_glide/zz_issue12_01.sql', 'db/scripts/zz_issue12_01.sql'].join('\n') + '\n');
    docker('build', '-t', `${registry}/${project}/pin-glide-db-tools:63.0.1`, fixture);
    write(path.join(fixture, 'migrations/pin_glide/zz_issue12_02.sql'), `
CREATE SCHEMA IF NOT EXISTS broken_release;
CREATE TABLE IF NOT EXISTS broken_release.partial_commit (id integer PRIMARY KEY);
INSERT INTO broken_release.partial_commit VALUES (12) ON CONFLICT DO NOTHING;
UPDATE backup_probe.records SET value = 'partially migrated';
ALTER TABLE backup_probe.records ADD COLUMN IF NOT EXISTS broken boolean DEFAULT true;
SELECT setval('backup_probe.records_id_seq', 900, true);
CREATE OR REPLACE FUNCTION backup_probe.answer() RETURNS integer LANGUAGE sql AS 'SELECT 99';
DROP SCHEMA IF EXISTS removed_probe CASCADE;
UPDATE cron.job SET active = false WHERE jobname = 'pin_glide_smoke_probe';
`);
    write(path.join(fixture, 'migrations/pin_glide/zz_issue12_03.sql'), 'SELECT 1 / 0;\n');
    fs.copyFileSync(path.join(fixture, 'migrations/pin_glide/zz_issue12_02.sql'), path.join(fixture, 'scripts/zz_issue12_02.sql'));
    write(path.join(fixture, 'migrations/order.txt'), [...baseOrder, 'db/migrations/pin_glide/zz_issue12_01.sql', 'db/scripts/zz_issue12_01.sql', 'db/migrations/pin_glide/zz_issue12_02.sql', 'db/migrations/pin_glide/zz_issue12_03.sql', 'db/scripts/zz_issue12_02.sql'].join('\n') + '\n');
    docker('build', '-t', `${registry}/${project}/pin-glide-db-tools:63.0.2`, fixture);
    const dataFixture = path.join(work, 'data-fixture');
    fs.mkdirSync(dataFixture);
    write(path.join(dataFixture, 'Dockerfile'), `FROM ${registry}/${project}/pin-glide-db-tools:base\nCOPY issue_12_99_data_failure.sql /app/db/scripts/\nCOPY order.txt /app/db/migrations/order.txt\n`);
    write(path.join(dataFixture, 'issue_12_99_data_failure.sql'), 'SELECT 1 / 0;\n');
    write(path.join(dataFixture, 'order.txt'), [...baseOrder, 'db/scripts/issue_12_99_data_failure.sql'].join('\n') + '\n');
    docker('build', '-t', `${registry}/${project}/pin-glide-db-tools:63.0.3`, dataFixture);
    for (const image of images) if (!image.endsWith(':base')) docker('push', image);
    evidence.real.push('Real repository Dockerfiles; the migration variants contain real SQL files');
    saveCompose(main);
    const initialEnv = fs.readFileSync(path.join(main.dir, '.env'), 'utf8');
    await check('Menu does not auto-prep; actions require an active attempt', () => {
      cli(main, '63.0.0', undefined, { menu: '', fails: true });
      cli(main, '63.0.0', '--full-release', { fails: true });
      assert(!fs.existsSync(active(main, '63.0.0')));
      assert(!fs.existsSync(path.join(main.dir, '.env.bak')));
    });
    await check('Initial installation uses the public prep menu and CLI', () => {
      cli(main, '63.0.0', undefined, { menu: '1\ny' });
      assert.equal(json(active(main, '63.0.0')).initial, true);
      assert.equal(compose(main, 'ps', '-aq', 'postgres'), '', 'Initial prep must not start PostgreSQL');
      assert.equal(fs.readFileSync(path.join(main.dir, '.env.bak'), 'utf8'), initialEnv);
      cli(main, '63.0.0', '--initial-deploy');
      ready(main, '63.0.0'); archived(main, '63.0.0', 1);
      assert.equal(sql(main, 'SELECT release_version FROM release.release_deploy_history ORDER BY id DESC LIMIT 1'), '63.0.0');
      assert.match(sql(main, 'SHOW server_version'), /^18\./);
      assert.match(sql(main, "SHOW shared_preload_libraries"), /pg_cron/);
      assert.equal(sql(main, "SELECT extversion FROM pg_extension WHERE extname = 'pg_cron'"), '1.6',
        'the packaged desired schema installs the extension the image preloads');
      assert.deepEqual(jobs(main), [retentionJob],
        'the first release declares only the adopted retention job');
    });
    const infrastructure = infra(main);
    evidence.infrastructure = infrastructure;
    await check('Successful upgrade preserves the PostgreSQL container and installs scheduled work', () => {
      cli(main, '63.0.1', '--prep');
      cli(main, '63.0.1', undefined, { menu: 7 });
      ready(main, '63.0.1'); archived(main, '63.0.1', 1);
      assert.deepEqual(infra(main), infrastructure, 'an ordinary release must not recreate PostgreSQL');
      assert.equal(sql(main, 'SELECT backup_probe.answer()'), '42');
      assert.equal(sql(main, "SELECT extversion FROM pg_extension WHERE extname = 'pg_cron'"), '1.6');
      assert.deepEqual(jobs(main), declaredJobs,
        'a release that declares jobs must leave their declared state untouched');
    });
    let baseline;
    const backup = path.join(main.dir, 'backup-63.0.2.dump');
    let frozen;
    await check('Stopped backup requires the api and scheduled SQL to be down and drained', async () => {
      cli(main, '63.0.2', '--prep');
      sql(main, "SELECT cron.schedule('pin_glide_smoke_sleeper', '2 seconds', 'SELECT pg_sleep(60)')");
      for (let attempt = 0; cronRunning(main) === '0'; attempt++) {
        assert(attempt < 100, 'the sleeper job never started');
        await new Promise(resolve => setTimeout(resolve, 200));
      }
      cli(main, '63.0.2', '--pull');
      cli(main, '63.0.2', '--backup');
      assert.equal(json(active(main, '63.0.2')).backup.stopped, false, 'an online backup is not a recovery baseline');
      cli(main, '63.0.2', '--stop-api');
      assert.equal(cronRunning(main), '0', 'running scheduled SQL must have exited before the baseline');
      assert.ok(jobs(main).every(job => job.endsWith('=false')), 'no scheduled job may stay active while stopped');
      const held = json(active(main, '63.0.2'));
      assert.equal(held.cron.status, 'paused');
      const probeId = sql(main, "SELECT jobid FROM cron.job WHERE jobname = 'pin_glide_smoke_probe'");
      assert.equal(held.cron.jobs[probeId], true, 'the intended active jobs are recorded before they are held');
      sql(main, "SELECT cron.unschedule('pin_glide_smoke_sleeper')");
      cli(main, '63.0.2', '--backup');
      assert.equal(json(active(main, '63.0.2')).backup.stopped, true);
      assert.equal(fs.readFileSync(backup).subarray(0, 5).toString(), 'PGDMP');
      frozen = hash(backup);
      baseline = snapshot(main, 'before-failure.sql', 'backup_probe', 'removed_probe');
    });
    await check('First migration commits; second fails and is retained; retry freezes backup', () => {
      cli(main, '63.0.2', '--migrate', { fails: true }); retainFailure(main, '63.0.2'); stopped(main);
      assert.equal(sql(main, 'SELECT id FROM broken_release.partial_commit'), '12');
      assert.equal(sql(main, 'SELECT backup_probe.answer()'), '99');
      assert.equal(sql(main, "SELECT count(*) FROM release.release_migration_scripts WHERE filename = 'zz_issue12_02.sql'"), '1');
      assert.equal(sql(main, "SELECT count(*) FROM release.release_migration_scripts WHERE filename = 'zz_issue12_03.sql'"), '0');
      assert.notEqual(snapshot(main, 'partial-failure.sql', 'backup_probe', 'removed_probe'), baseline);
      assert.equal(cronRunning(main), '0', 'a failed release must not restart scheduled SQL');
      assert.ok(jobs(main).every(job => job.endsWith('=false')), 'scheduled work stays held while the release is unfinished');
      cli(main, '63.0.2', '--backup', { fails: true });
      cleanupFailedMigration(main);
      cli(main, '63.0.2', '--full-release', { fails: true }); retainFailure(main, '63.0.2');
      assert.equal(hash(backup), frozen);
      assert.deepEqual(infra(main), infrastructure);
    });
    await check('Cancel rebuilds the complete database and restores the intended scheduled state', () => {
      saveCompose(main, c => { c.services.api.entrypoint = ['/issue12-intentionally-missing']; });
      cli(main, '63.0.2', '--cancel', { fails: true }); stopped(main);
      assert.equal(json(active(main, '63.0.2')).restore.database, true);
      assert.equal(archives(main, '63.0.2').length, 0);
      assert.equal(snapshot(main, 'restored.sql', 'backup_probe', 'removed_probe'), baseline, 'Full database contents differ after pg_restore');
      assert.equal(sql(main, "SELECT count(*) FROM pg_namespace WHERE nspname = 'broken_release'"), '0');
      assert.ok(jobs(main).every(job => job.endsWith('=false')), 'a restored database keeps scheduling held until the release finishes');
      assert.equal(cronRunning(main), '0');
      assert.equal(hash(backup), frozen);
      assert.deepEqual(infra(main), infrastructure);
      evidence.real.push('PostgreSQL 18 pg_dump -Fc / pg_restore with pg_cron job rows; full probe snapshot matched after a partial commit');
      saveCompose(main); cli(main, '63.0.2', '--cancel'); ready(main, '63.0.1'); archived(main, '63.0.2', 1);
      assert.deepEqual(jobs(main), declaredJobs,
        'a successful cancellation must restore the intended scheduling state');
      assert.deepEqual(infra(main), infrastructure);
    });
    await check('Retained failed container blocks prep; operator cleanup permits menu start without migration', () => {
      const before = hash(path.join(main.dir, '.env'));
      cli(main, '63.0.2', '--prep', { fails: true });
      assert.equal(hash(path.join(main.dir, '.env')), before);
      cleanupFailedMigration(main); cli(main, '63.0.2', '--prep');
      cli(main, '63.0.2', undefined, { menu: 6 }); ready(main, '63.0.2'); archived(main, '63.0.2', 2);
      assert.deepEqual(infra(main), infrastructure);
      assert.deepEqual(jobs(main), declaredJobs,
        'a standalone start must not leave the schedule held');
    });
    await check('Same-version retry invalidates a completed restore checkpoint', () => {
      // The preceding public prep/start deliberately skipped migration: the immutable
      // 63.0.2 application image now runs on the restored 63.0.1 database.
      cli(main, '63.0.2', '--prep');
      assert.equal(json(active(main, '63.0.2')).previous, '63.0.2');
      assert.equal(json(active(main, '63.0.2')).database_version, '63.0.1');
      cli(main, '63.0.2', '--stop-api'); cli(main, '63.0.2', '--backup');
      const backupHash = hash(backup);
      const original = snapshot(main, 'same-version-original.sql', 'backup_probe', 'removed_probe');
      const databaseOID = () => sql(main, "SELECT oid FROM pg_database WHERE datname = 'pin_glide'");
      const beforeOID = databaseOID();
      cli(main, '63.0.2', '--migrate', { fails: true }); retainFailure(main, '63.0.2'); stopped(main);
      assert.equal(sql(main, 'SELECT id FROM broken_release.partial_commit'), '12');
      assert.notEqual(snapshot(main, 'same-version-first-failure.sql', 'backup_probe', 'removed_probe'), original);
      assert.equal(hash(backup), backupHash);

      // The first cancel restores the frozen database, then fails to start it.
      saveCompose(main, c => { c.services.api.entrypoint = ['/issue12-intentionally-missing']; });
      cli(main, '63.0.2', '--cancel', { fails: true });
      stopped(main);
      const retained = json(active(main, '63.0.2'));
      assert.equal(retained.restore.database, true);
      assert.equal(retained.restore.environment, true);
      write(path.join(work, 'same-version-cancelled-attempt.json'), JSON.stringify(retained, null, 2));
      assert.equal(archives(main, '63.0.2').length, 2);
      const firstRestore = snapshot(main, 'same-version-first-restored.sql', 'backup_probe', 'removed_probe');
      assert.equal(firstRestore, original, 'First cancel did not fully restore the frozen database');
      const firstRestoreOID = databaseOID();
      assert.notEqual(firstRestoreOID, beforeOID);

      // Only remove the owned failed container; preserve the attempt and its restore marker.
      cleanupFailedMigration(main);
      cli(main, '63.0.2', '--migrate', { fails: true }); retainFailure(main, '63.0.2'); stopped(main);
      assert.equal(json(active(main, '63.0.2')).restore, undefined);
      assert.equal(sql(main, 'SELECT id FROM broken_release.partial_commit'), '12');
      assert.equal(sql(main, 'SELECT backup_probe.answer()'), '99');
      const retrySnapshot = snapshot(main, 'same-version-retry-failure.sql', 'backup_probe', 'removed_probe');
      assert.notEqual(retrySnapshot, original);
      assert.equal(databaseOID(), firstRestoreOID);
      assert.equal(hash(backup), backupHash);
      saveCompose(main); cli(main, '63.0.2', '--cancel'); ready(main, '63.0.2'); archived(main, '63.0.2', 3);
      const secondRestoreOID = databaseOID();
      assert.notEqual(secondRestoreOID, firstRestoreOID, 'Second cancel skipped rebuilding the database');
      const restoredProbes = snapshot(main, 'same-version-second-restored.sql', 'backup_probe', 'removed_probe');
      assert.equal(restoredProbes, original);
      assert.deepEqual(jobs(main), declaredJobs);
      assert.equal(sql(main, "SELECT count(*) FROM pg_namespace WHERE nspname = 'broken_release'"), '0');
      assert.equal(sql(main, "SELECT count(*) FROM release.release_migration_scripts WHERE filename IN ('zz_issue12_02.sql', 'zz_issue12_03.sql')"), '0');
      assert.equal(hash(path.join(main.dir, '.env')), hash(path.join(main.dir, '.env.bak')));
      assert.equal(hash(backup), backupHash);
      assert.deepEqual(infra(main), infrastructure);
      evidence.sameVersionRetry = {
        backupSha256: backupHash, originalSnapshotSha256: original, firstRestoreSnapshotSha256: firstRestore,
        retrySnapshotSha256: retrySnapshot, restoredSnapshotSha256: restoredProbes,
        databaseOIDs: { before: beforeOID, firstRestore: firstRestoreOID, secondRestore: secondRestoreOID },
      };
      evidence.real.push('A same-version retry after a failed cancellation restored the original frozen database again');
    });
    await check('Incorrect same-version initial claim cannot acquire retry permission', () => {
      cleanupFailedMigration(main);
      const before = sql(main, 'SELECT count(*) FROM release.release_deploy_history');
      cli(main, '63.0.1', '--prep', { initial: true });
      cli(main, '63.0.1', '--stop-api');
      for (let retry = 0; retry < 2; retry++) {
        cli(main, '63.0.1', '--migrate', { fails: true }); retainFailure(main, '63.0.1');
        assert.notEqual(json(active(main, '63.0.1')).migration.initial_verified, true);
        assert.equal(sql(main, 'SELECT count(*) FROM release.release_deploy_history'), before);
        cleanupFailedMigration(main);
      }
      cli(main, '63.0.1', '--start-api'); ready(main, '63.0.1');
      assert.deepEqual(jobs(main), declaredJobs);
    });
    await check('Initial data failure retains Database Release; only verified same-target retries may continue', () => {
      const cleanConfig = structuredClone(main.config);
      delete cleanConfig.services.registry; delete cleanConfig.volumes.registry;
      const fresh = makeInstall(`${project}-no-backup`, cleanConfig);
      const original = hash(path.join(fresh.dir, '.env'));
      cli(fresh, '63.0.3', '--prep', { fails: true });
      assert.equal(hash(path.join(fresh.dir, '.env')), original);
      assert(!fs.existsSync(active(fresh, '63.0.3')));
      cli(fresh, '63.0.3', '--prep', { initial: true });
      assert.equal(compose(fresh, 'ps', '-aq', 'postgres'), '');
      cli(fresh, '63.0.3', '--initial-deploy', { fails: true });
      retainFailure(fresh, '63.0.3'); stopped(fresh);
      assert.equal(json(active(fresh, '63.0.3')).migration.initial_verified, true);
      const history = () => sql(fresh, 'SELECT release_version FROM release.release_deploy_history ORDER BY id');
      assert.equal(history(), '63.0.3');
      cleanupFailedMigration(fresh);
      cli(fresh, '63.0.3', '--migrate', { fails: true }); retainFailure(fresh, '63.0.3');
      assert.equal(history(), '63.0.3\n63.0.3', 'Same-target retry must reach Schema Sync before failing data again');
      cleanupFailedMigration(fresh);
      // Another real release reaches Schema Sync; do not fabricate history rows.
      saveCompose(fresh, c => { c.services['db-tools'].image = `${registry}/${project}/pin-glide-db-tools:63.0.4`; });
      compose(fresh, 'run', '--rm', '--no-deps', '-e', 'PIN_GLIDE_RELEASE_VERSION=63.0.4',
        '-e', 'PIN_GLIDE_RELEASE_INITIAL=false', '-e', 'PIN_GLIDE_RELEASE_INITIAL_RETRY=false', 'db-tools', 'migrate');
      saveCompose(fresh);
      const beforeHistory = history();
      assert.equal(beforeHistory.split('\n').at(-1), '63.0.4');
      cli(fresh, '63.0.3', '--migrate', { fails: true }); retainFailure(fresh, '63.0.3');
      assert.equal(history(), beforeHistory, 'Verified initial retry must reject a different Database Release');
      const before = snapshot(fresh, 'no-backup-before.sql', 'pin_glide');
      const oid = sql(fresh, "SELECT oid FROM pg_database WHERE datname = 'pin_glide'");
      const ids = infra(fresh);
      assert(!fs.existsSync(path.join(fresh.dir, 'backup-63.0.3.dump')));
      cli(fresh, '63.0.3', '--cancel', { fails: true });
      assert.equal(snapshot(fresh, 'no-backup-after.sql', 'pin_glide'), before);
      assert.equal(sql(fresh, "SELECT oid FROM pg_database WHERE datname = 'pin_glide'"), oid);
      assert.deepEqual(infra(fresh), ids); stopped(fresh);
      assert(fs.existsSync(active(fresh, '63.0.3')));
      assert.equal(archives(fresh, '63.0.3').length, 0);
    });
    evidence.status = 'passed';
  }
} catch (error) {
  evidence.status = 'failed';
  evidence.failure = error.stack;
  process.exitCode = 1;
  console.error(error.stack);
} finally {
  for (const install of installations.reverse()) {
    for (const [suffix, args] of [['containers.json', ['ps', '--all', '--format', 'json']], ['logs.txt', ['logs', '--no-color']]]) {
      const result = run('docker', ['compose', '--env-file', '.env', '-f', 'compose.yaml', ...args], { cwd: install.dir, allowFailure: true });
      write(path.join(work, `${install.project}-${suffix}`), `${result.stdout || ''}${result.stderr || ''}`);
    }
    try {
      // These generated projects contain no external network/volume or host bind mounts.
      compose(install, 'down', '--volumes', '--remove-orphans', '--timeout', '10');
      for (const kind of ['container', 'network', 'volume']) {
        const remaining = docker(kind, 'ls', kind === 'container' ? '-aq' : '-q', '--filter', `label=com.docker.compose.project=${install.project}`);
        assert.equal(remaining, '', `Leaked ${kind} for ${install.project}`);
      }
      evidence.cleanup.push(`${install.project}: no containers, networks or volumes remain`);
    } catch (error) { evidence.cleanup.push(error.message); process.exitCode = 1; }
  }
  for (const image of [...images].reverse()) run('docker', ['image', 'rm', image], { allowFailure: true });
  evidence.status ||= 'failed';
  if (process.exitCode) evidence.status = 'failed';
  write(path.join(work, 'evidence.json'), JSON.stringify(evidence, null, 2));
  console.log(`Evidence: ${path.join(work, 'evidence.json')}`);
}
