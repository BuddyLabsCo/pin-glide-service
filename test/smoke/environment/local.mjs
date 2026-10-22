import { runProcess } from '../support/commands.mjs';
import { withFailureJoiningCleanup } from '../support/cleanup.mjs';
import { databaseConfigurationFactsQuery } from './database-facts.mjs';
import {
  withBailianConnectivityFault,
  withInvalidBailianKey,
} from './provider-faults.mjs';
import { quote } from '../support/shell.mjs';

const repository = 'BuddyLabsCo/pin-glide-service';
const deploymentEnvironment = /^(?:COMPOSE_|RELEASE$|PIN_GLIDE_|POSTGRES_|BAILIAN_|WECHAT_|QQ_)/;
export const snapshotName = /^pin-glide-snapshot-\d{8}T\d{6}\.\d{9}Z-[0-9]+\.dump$/;
export const externalWriterQuery = `
SELECT json_build_object(
  'connections', count(*)
)
FROM pg_stat_activity
WHERE datname = current_database()
  AND pid <> pg_backend_pid()
  AND backend_type = 'client backend'
  AND coalesce(application_name, '') <> 'pg_dump';
`;

function isolatedEnvironment(inherited, overrides) {
  const base = Object.fromEntries(
    Object.entries(inherited).filter(([name]) => !deploymentEnvironment.test(name)),
  );
  const extra = Object.fromEntries(
    Object.entries(overrides).filter(([name, value]) =>
      value !== undefined && !deploymentEnvironment.test(name)),
  );
  return { ...base, ...extra };
}

export class LocalEnvironment {
  constructor({ target, run = runProcess, inheritedEnvironment = process.env }) {
    this.target = target;
    this.run = run;
    this.inheritedEnvironment = inheritedEnvironment;
    this.preservedApiLogs = [];
    this.smokeStartedAt = null;
    this.sudo = 'sudo -n env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES';
    this.compose = [
      this.sudo,
      'docker', 'compose',
      '--project-name', quote(target.project),
      '--project-directory', quote(target.deploymentDir),
      '--env-file', quote(`${target.deploymentDir}/.env`),
      '-f', quote(`${target.deploymentDir}/docker-compose.yml`),
    ].join(' ');
  }

  exec(script, options = {}) {
    return this.run('bash', ['-ec', script], {
      ...options,
      env: {
        ...isolatedEnvironment(this.inheritedEnvironment, options.env || {}),
      },
    });
  }

  async verifyDeployment() {
    const result = await this.exec([
      `test -d ${quote(this.target.deploymentDir)}`,
      `readlink -f ${quote(this.target.deploymentDir)}`,
    ].join(' && '), { timeoutMs: 30_000 });
    const lines = result.stdout.trim().split('\n');
    const path = lines[0];
    if (path !== this.target.deploymentDir) {
      throw new Error(`Smoke deployment directory must resolve to ${this.target.deploymentDir}`);
    }
    const environment = await this.verifyDeploymentEnvironment();
    return { path, environment };
  }

  async verifyDeploymentEnvironment() {
    // The deployment .env is authoritative: exec() removes inherited Compose
    // and service variables before Compose resolves this file.
    const expected = {
      COMPOSE_PROJECT_NAME: this.target.project,
      PIN_GLIDE_API_BIND: this.target.hostIP,
      PIN_GLIDE_SNAPSHOT_PATH: this.target.snapshotDir,
    };
    const names = Object.keys(expected).join('|');
    const result = await this.exec(
      `set -o pipefail\n` +
        `sudo -n env -u COMPOSE_PROJECT_NAME -u COMPOSE_FILE -u COMPOSE_PROFILES -u PIN_GLIDE_API_BIND -u PIN_GLIDE_SNAPSHOT_PATH ` +
        `docker compose --project-name ${quote(this.target.project)} ` +
        `--project-directory ${quote(this.target.deploymentDir)} ` +
        `--env-file ${quote(`${this.target.deploymentDir}/.env`)} ` +
        `-f ${quote(`${this.target.deploymentDir}/docker-compose.yml`)} config --environment ` +
        `| sed -n -E ${quote(`/^(${names})=/p`)}`,
      { timeoutMs: 30_000 },
    );
    const actual = Object.fromEntries(result.stdout
      .split('\n')
      .filter(Boolean)
      .map(line => {
        const separator = line.indexOf('=');
        return [line.slice(0, separator), line.slice(separator + 1)];
      }));
    for (const [name, value] of Object.entries(expected)) {
      if (actual[name] !== value) {
        throw new Error(
          `${this.target.deploymentDir}/.env must set ${name}=${value}, found ${actual[name] ?? 'no value'}`,
        );
      }
    }
    return actual;
  }

  async composeConfig() {
    const result = await this.exec(`${this.compose} config --format json`, { timeoutMs: 30_000 });
    try {
      return JSON.parse(result.stdout);
    } catch {
      throw new Error('Local Compose configuration is not valid JSON');
    }
  }

  sql(statement, options = {}) {
    return this.sqlOn('pin_glide', statement, options);
  }

  sqlOn(database, statement, options = {}) {
    if (!/^[a-z][a-z0-9_]{0,62}$/.test(database)) throw new Error('Invalid smoke database name');
    return this.exec(`${this.compose} exec -T postgres psql -X -v ON_ERROR_STOP=1 -A -t -U pin_glide -d ${quote(database)} -f -`, {
      ...options,
      input: statement,
    });
  }

  async sqlJSON(statement, options = {}) {
    return this.sqlJSONOn('pin_glide', statement, options);
  }

  async sqlJSONOn(database, statement, options = {}) {
    const result = await this.sqlOn(database, statement, options);
    try {
      return JSON.parse(result.stdout.trim());
    } catch {
      throw new Error('Local smoke query did not return one JSON value');
    }
  }

  install(action, version) {
    const command = `${this.sudo} bash ${quote(`${this.target.deploymentDir}/install.sh`)}`;
    return this.exec(
      `cd ${quote(this.target.deploymentDir)} && ${command} --version ${quote(version)} ${action}`,
      { timeoutMs: 600_000 },
    );
  }

  stopApi() {
    return this.exec(`${this.compose} stop api`, { timeoutMs: 60_000 });
  }

  startApi() {
    return this.exec(`${this.compose} up -d --no-deps --wait api`, { timeoutMs: 180_000 });
  }

  async withInvalidBailianKey(callback) {
    return withInvalidBailianKey(this, callback);
  }

  async withBailianConnectivityFault(workspaceID, callback, options) {
    return withBailianConnectivityFault(this, workspaceID, callback, options);
  }

  async createStoppedSnapshot(version, { afterStop } = {}) {
    let prepared = false;
    let stopped = false;
    let failure;
    let createdSnapshot;
    let snapshotsBefore;
    try {
      await this.install('--prep', version);
      prepared = true;
      await this.install('--stop-api', version);
      stopped = true;
      if (afterStop) await afterStop();
      await this.assertNoExternalWriters();
      snapshotsBefore = new Set((await this.listSnapshots()).map(entry => entry.name));
      const writesBefore = await this.databaseWriteStats();
      let monitoring = true;
      const writerMonitor = (async () => {
        while (monitoring) {
          try {
            await this.assertNoExternalWriters();
          } catch (error) {
            return error;
          }
          await new Promise(resolve => setTimeout(resolve, 200));
        }
        return null;
      })();
      let snapshotError;
      try {
        const result = await this.makeSnapshot();
        createdSnapshot = snapshotFromOutput(result?.stdout);
      } catch (error) {
        snapshotError = error;
      } finally {
        monitoring = false;
        const monitorError = await writerMonitor;
        if (monitorError) {
          throw snapshotError
            ? new AggregateError([snapshotError, monitorError], 'Snapshot and writer monitoring both failed')
            : monitorError;
        }
      }
      if (snapshotError) throw snapshotError;
      await this.assertNoExternalWriters();
      const writesAfter = await this.databaseWriteStats();
      if (JSON.stringify(writesBefore) !== JSON.stringify(writesAfter)) {
        throw new Error('Database writes occurred while the stopped snapshot was being created');
      }
      await this.assertNoExternalWriters();
      const snapshotsAfter = await this.listSnapshots();
      const added = snapshotsAfter
        .map(entry => entry.name)
        .filter(name => !snapshotsBefore.has(name));
      if (createdSnapshot && !added.includes(createdSnapshot)) {
        throw new Error('The stopped snapshot command reported an archive that is not present');
      }
      if (!createdSnapshot) {
        throw new Error('The stopped snapshot action reported no archive');
      }
    } catch (error) {
      failure = error;
    } finally {
      if (failure && createdSnapshot) {
        try {
          await this.removeSnapshot(createdSnapshot);
        } catch (cleanupError) {
          failure = failure
            ? new AggregateError([failure, cleanupError], 'Stopped snapshot cleanup and recovery action both failed')
            : cleanupError;
        }
      }
      if (prepared) {
        try {
          await this.install(stopped ? '--start-api' : '--cancel', version);
        } catch (cleanupError) {
          failure = failure
            ? new AggregateError([failure, cleanupError], 'Stopped snapshot and recovery action both failed')
            : cleanupError;
        }
      }
    }
    if (failure) throw failure;
    return createdSnapshot;
  }

  makeSnapshot() {
    return this.exec(
      `${this.sudo} bash ${quote(`${this.target.deploymentDir}/install.sh`)} --make-snapshot`,
      { timeoutMs: 600_000 },
    );
  }

  async listSnapshots() {
    const result = await this.exec(
      `find ${quote(this.target.snapshotDir)} -maxdepth 1 -type f -name 'pin-glide-snapshot-*.dump' -printf '%f\\n' | sort`,
      { timeoutMs: 30_000 },
    );
    const names = result.stdout.trim().split('\n').filter(Boolean);
    if (names.some(name => !snapshotName.test(name))) {
      throw new Error('Snapshot directory contains a malformed completed snapshot name');
    }
    return names.map(name => ({ name }));
  }

  removeSnapshot(name) {
    if (!snapshotName.test(name)) throw new Error('Refusing to remove a non-smoke snapshot');
    return this.exec(
      `sudo -n rm -f -- ${quote(`${this.target.snapshotDir}/${name}`)}`,
      { timeoutMs: 30_000 },
    );
  }

  async assertNoExternalWriters() {
    const result = await this.sqlJSON(externalWriterQuery);
    if (Number(result.connections) !== 0) {
      throw new Error('Another database writer is still connected; refusing the snapshot');
    }
  }

  databaseWriteStats() {
    return this.sqlJSON(`
SELECT json_build_object(
  'inserted', tup_inserted,
  'updated', tup_updated,
  'deleted', tup_deleted
)
FROM pg_stat_database
WHERE datname = current_database();
`);
  }

  stopAPI() {
    return this.exec(`${this.compose} stop api`, { timeoutMs: 60_000 });
  }

  startAPI() {
    return this.exec(`${this.compose} up -d --no-deps --wait api`, { timeoutMs: 180_000 });
  }

  async restartAPI() {
    let stopped = false;
    await withFailureJoiningCleanup(
      async () => {
        await this.stopAPI();
        stopped = true;
        await this.startAPI();
        stopped = false;
      },
      async () => {
        if (stopped) await this.startAPI();
      },
      'API restart and recovery both failed',
    );
  }

  stopPostgres() {
    return this.exec(`${this.compose} stop postgres`, { timeoutMs: 60_000 });
  }

  startPostgres() {
    return this.exec(`${this.compose} up -d --no-deps --wait postgres`, { timeoutMs: 180_000 });
  }

  image(reference) {
    return this.exec(
      `sudo -n docker image inspect ${quote(reference)} --format '{{json .Config.Labels}}|{{json .RepoDigests}}|{{.Id}}'`,
      { timeoutMs: 30_000 },
    );
  }

  scriptManifest(reference) {
    return this.exec(
      `sudo -n docker run --rm --network none --entrypoint cat ${quote(reference)} /app/db/migrations/order.txt`,
      { timeoutMs: 30_000 },
    );
  }

  deployedImage(service) {
    return this.exec(`sudo -n docker inspect "$(${this.compose} ps -q ${quote(service)})" --format '{{.Config.Image}}|{{json .Config.Labels}}|{{.Image}}'`, {
      timeoutMs: 30_000,
    });
  }

  apiContainerPort() {
    return this.exec([
      `id=$(${this.compose} ps -q api)`,
      'test -n "$id"',
      'sudo -n docker port "$id" 8080/tcp',
    ].join(' && '), { timeoutMs: 30_000 });
  }

  rawApiLogsSince(since) {
    const value = new Date(since);
    if (!Number.isFinite(value.getTime())) throw new Error('Smoke service-log start time is invalid');
    return this.exec(
      `${this.compose} logs --no-color --since ${quote(value.toISOString())} api`,
      { timeoutMs: 30_000 },
    );
  }

  async preserveApiLogs(since = this.smokeStartedAt) {
    if (!since) throw new Error('Smoke service-log start time is unavailable');
    const result = await this.rawApiLogsSince(since);
    this.preservedApiLogs.push({
      since: new Date(since).toISOString(),
      stdout: result?.stdout || '',
      stderr: result?.stderr || '',
    });
  }

  async apiLogsSince(since) {
    const value = new Date(since);
    if (!Number.isFinite(value.getTime())) throw new Error('Smoke service-log start time is invalid');
    const current = await this.rawApiLogsSince(since);
    const preserved = this.preservedApiLogs.filter(
      entry => Date.parse(entry.since) >= value.getTime(),
    );
    return {
      stdout: [...preserved.map(entry => entry.stdout), current?.stdout || '']
        .filter(Boolean)
        .join('\n'),
      stderr: [...preserved.map(entry => entry.stderr), current?.stderr || '']
        .filter(Boolean)
        .join('\n'),
    };
  }

  scheduledWorkState() {
    return this.sqlJSON(`
SELECT json_build_object(
  'activeJobs', (
    SELECT coalesce(json_agg(jobname ORDER BY jobname), '[]'::json)
    FROM cron.job
    WHERE active
  ),
  'runningRuns', (
    SELECT count(*)
    FROM cron.job_run_details
    WHERE status = 'running'
  ),
  'cronBackends', (
    SELECT count(*)
    FROM pg_stat_activity
    WHERE application_name = 'pg_cron'
  ),
  'maxRunID', (
    SELECT coalesce(max(runid), 0)
    FROM cron.job_run_details
  )
);
`);
  }

  schemaAudits() {
    return this.sqlJSON(databaseConfigurationFactsQuery({
      includeReleaseLedgers: true,
      includeCronBackends: true,
    }));
  }
}

function snapshotFromOutput(stdout = '') {
  const match = stdout.match(/pin-glide-snapshot-\d{8}T\d{6}\.\d{9}Z-[0-9]+\.dump/);
  const name = match?.[0];
  if (name && !snapshotName.test(name)) throw new Error('Snapshot command reported an invalid archive name');
  return name || null;
}

export { quote, repository };
