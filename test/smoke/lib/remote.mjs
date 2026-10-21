import { runProcess } from './commands.mjs';
import { withFailureJoiningCleanup } from './cleanup.mjs';
import {
  withBailianConnectivityFault,
  withInvalidBailianKey,
} from './provider_faults.mjs';
import { quote } from './shell.mjs';

const repository = 'BuddyLabsCo/pin-glide-service';
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

export class RemoteEnvironment {
  constructor({ target, run = runProcess }) {
    this.target = target;
    this.run = run;
    this.composeArgs = [
      'docker',
      'compose',
      '--project-directory', quote(target.deploymentDir),
      '--env-file', quote(`${target.deploymentDir}/.env`),
      '-f', quote(`${target.deploymentDir}/docker-compose.yml`),
    ];
    this.compose = ['sudo', '-n', ...this.composeArgs].join(' ');
  }

  ssh(script, options = {}) {
    return this.run('ssh', [
      '-o', 'BatchMode=yes',
      '-o', 'ConnectTimeout=10',
      this.target.sshHost,
      script,
    ], options);
  }

  async verifyDeployment() {
    const result = await this.ssh([
      `test -d ${quote(this.target.deploymentDir)}`,
      `readlink -f ${quote(this.target.deploymentDir)}`,
      'hostname -I',
    ].join(' && '), { timeoutMs: 30_000 });
    const lines = result.stdout.trim().split('\n');
    const path = lines[0];
    const addresses = lines.slice(1).join(' ').split(/\s+/).filter(Boolean);
    if (path !== this.target.deploymentDir) {
      throw new Error(`Smoke deployment directory must resolve to ${this.target.deploymentDir}`);
    }
    if (!addresses.includes('192.168.10.17')) {
      throw new Error('Smoke SSH target does not report the adopted 192.168.10.17 address');
    }
    return { path, addresses };
  }

  async composeConfig() {
    const result = await this.ssh(`${this.compose} config --format json`, { timeoutMs: 30_000 });
    try {
      return JSON.parse(result.stdout);
    } catch {
      throw new Error('Remote Compose configuration is not valid JSON');
    }
  }

  sql(statement, options = {}) {
    return this.sqlOn('pin_glide', statement, options);
  }

  sqlOn(database, statement, options = {}) {
    if (!/^[a-z][a-z0-9_]{0,62}$/.test(database)) throw new Error('Invalid smoke database name');
    return this.ssh(`${this.compose} exec -T postgres psql -X -v ON_ERROR_STOP=1 -A -t -U pin_glide -d ${quote(database)} -f -`, {
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
      throw new Error('Remote smoke query did not return one JSON value');
    }
  }

  install(action, version) {
    const command = `sudo -n bash ${quote(`${this.target.deploymentDir}/install.sh`)}`;
    return this.ssh(
      `cd ${quote(this.target.deploymentDir)} && ${command} --version ${quote(version)} ${action}`,
      { timeoutMs: 600_000 },
    );
  }

  stopApi() {
    return this.ssh(`${this.compose} stop api`, { timeoutMs: 60_000 });
  }

  startApi() {
    return this.ssh(`${this.compose} up -d --no-deps --wait api`, { timeoutMs: 180_000 });
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
    return this.ssh(
      `sudo -n bash ${quote(`${this.target.deploymentDir}/install.sh`)} --make-snapshot`,
      { timeoutMs: 600_000 },
    );
  }

  async listSnapshots() {
    const result = await this.ssh(
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
    return this.ssh(
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
    return this.ssh(`${this.compose} stop api`, { timeoutMs: 60_000 });
  }

  startAPI() {
    return this.ssh(`${this.compose} up -d --no-deps --wait api`, { timeoutMs: 180_000 });
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
    return this.ssh(`${this.compose} stop postgres`, { timeoutMs: 60_000 });
  }

  startPostgres() {
    return this.ssh(`${this.compose} up -d --no-deps --wait postgres`, { timeoutMs: 180_000 });
  }

  image(reference) {
    return this.ssh(
      `sudo -n docker image inspect ${quote(reference)} --format '{{json .Config.Labels}}|{{json .RepoDigests}}|{{.Id}}'`,
      { timeoutMs: 30_000 },
    );
  }

  scriptManifest(reference) {
    return this.ssh(
      `sudo -n docker run --rm --network none --entrypoint cat ${quote(reference)} /app/db/migrations/order.txt`,
      { timeoutMs: 30_000 },
    );
  }

  deployedImage(service) {
    return this.ssh(`sudo -n docker inspect "$(${this.compose} ps -q ${quote(service)})" --format '{{.Config.Image}}|{{json .Config.Labels}}|{{.Image}}'`, {
      timeoutMs: 30_000,
    });
  }

  apiContainerPort() {
    return this.ssh([
      `id=$(${this.compose} ps -q api)`,
      'test -n "$id"',
      'sudo -n docker port "$id" 8080/tcp',
    ].join(' && '), { timeoutMs: 30_000 });
  }

  apiLogsSince(since) {
    const value = new Date(since);
    if (!Number.isFinite(value.getTime())) throw new Error('Smoke service-log start time is invalid');
    return this.ssh(
      `${this.compose} logs --no-color --since ${quote(value.toISOString())} api`,
      { timeoutMs: 30_000 },
    );
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
    return this.sqlJSON(`
SELECT json_build_object(
  'database', current_database(),
  'release', (
    SELECT release_version
    FROM release.release_deploy_history
    ORDER BY id DESC
    LIMIT 1
  ),
  'history', (
    SELECT coalesce(json_agg(release_version ORDER BY id), '[]'::json)
    FROM release.release_deploy_history
  ),
  'model', (
    SELECT json_build_object(
      'id', id,
      'workspaceId', workspace_id,
      'model', model,
      'enabled', enabled
    )
    FROM pin_glide.model_configurations
    WHERE id = 'default'
  ),
  'retention', (
    SELECT coalesce(json_agg(json_build_object(
      'schema', schema_name,
      'table', table_name,
      'days', retention_days,
      'column', date_column
    ) ORDER BY schema_name, table_name), '[]'::json)
    FROM pin_glide.retention_policies
  ),
  'cron', (
    SELECT coalesce(json_agg(json_build_object(
      'jobname', jobname,
      'username', username,
      'database', database,
      'schedule', schedule,
      'active', active,
      'command', command
    ) ORDER BY jobname), '[]'::json)
    FROM cron.job
  ),
  'runningCronJobs', (
    SELECT count(*)
    FROM cron.job_run_details
    WHERE status = 'running'
  ),
  'cronBackends', (
    SELECT count(*)
    FROM pg_stat_activity
    WHERE application_name = 'pg_cron'
  ),
  'audits', (
    SELECT coalesce(json_object_agg(identity, exit_code), '{}'::json)
    FROM (
      SELECT DISTINCT ON (target_schema, operation)
        target_schema || ':' || operation AS identity,
        exit_code
      FROM release.release_schema_audits
      ORDER BY target_schema, operation, id DESC
    ) latest
  ),
  'dataScripts', (
    SELECT coalesce(json_agg(json_build_object(
      'filename', filename,
      'checksum', checksum
    ) ORDER BY filename), '[]'::json)
    FROM release.release_data_scripts
  ),
  'migrationScripts', (
    SELECT coalesce(json_agg(json_build_object(
      'schema', target_schema,
      'filename', filename,
      'checksum', checksum
    ) ORDER BY target_schema, filename), '[]'::json)
    FROM release.release_migration_scripts
  )
);
`);
  }
}

function snapshotFromOutput(stdout = '') {
  const match = stdout.match(/pin-glide-snapshot-\d{8}T\d{6}\.\d{9}Z-[0-9]+\.dump/);
  const name = match?.[0];
  if (name && !snapshotName.test(name)) throw new Error('Snapshot command reported an invalid archive name');
  return name || null;
}

export { quote, repository };
