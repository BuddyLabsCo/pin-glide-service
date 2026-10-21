import { randomBytes } from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';

import { validateConfiguration, validateReleaseHistory } from './database.mjs';
import { retentionDrainProbeSchedule } from './retention_runtime.mjs';
import { quote, snapshotName } from './remote.mjs';

const restoreContainer = /^smoke_restore_[a-f0-9]{16}$/;

export function lastCompletedSnapshot(entries, directory, sshHost) {
  const matching = entries
    .filter(entry => entry.isFile() && snapshotName.test(entry.name))
    .map(entry => entry.name)
    .sort();
  if (matching.length === 0) {
    throw new Error(`No completed snapshot exists in ${sshHost}:${directory}`);
  }
  return path.posix.join(directory, matching.at(-1));
}

export async function verifySnapshotDirectory(remote) {
  const { target } = remote;
  const command = [
    `test -d ${quote(target.snapshotDir)}`,
    `readlink -f ${quote(target.snapshotDir)}`,
    `sudo -n test -w ${quote(target.snapshotDir)}`,
  ].join(' && ');
  const result = await remote.ssh(command, { timeoutMs: 30_000 });
  const resolved = result.stdout.trim().split('\n').at(-1);
  if (resolved !== target.snapshotDir) {
    throw new Error(`Snapshot directory must resolve to ${target.snapshotDir}`);
  }
}

export async function verifyLocalSnapshotDirectory(directory) {
  let resolved;
  try {
    resolved = await fs.promises.realpath(directory);
  } catch (error) {
    throw new Error(`Local snapshot directory is unavailable: ${error.message}`);
  }
  if (resolved !== directory) {
    throw new Error(`Local snapshot directory must resolve to ${directory}`);
  }
  const stat = await fs.promises.stat(directory);
  if (!stat.isDirectory()) throw new Error('Local snapshot path must be a directory');
  await fs.promises.access(directory, fs.constants.R_OK | fs.constants.W_OK);
  return resolved;
}

export async function verifySnapshotLocations(remote) {
  await verifySnapshotDirectory(remote);
  await verifyLocalSnapshotDirectory(remote.target.snapshotDir);
}

export async function listRemoteSnapshots(remote) {
  if (typeof remote.listSnapshots === 'function') return remote.listSnapshots();
  const result = await remote.ssh(
    `find ${quote(remote.target.snapshotDir)} -maxdepth 1 -type f -name 'pin-glide-snapshot-*.dump' -printf '%f %s\\n' | sort`,
    { timeoutMs: 30_000 },
  );
  const partial = result.stdout.trim().split('\n').filter(Boolean).map(line => {
    const [name] = line.split(' ');
    return { name };
  });
  if (partial.some(entry => !snapshotName.test(entry.name))) {
    throw new Error('Snapshot directory contains a malformed completed snapshot name');
  }
  return partial;
}

export async function listLocalSnapshots(directory) {
  const entries = await fs.promises.readdir(directory, { withFileTypes: true });
  return entries
    .filter(entry => entry.isFile() && entry.name.endsWith('.dump'))
    .map(entry => entry.name)
    .sort();
}

export async function recoverySnapshot(remote, { allowMissing = false } = {}) {
  await verifySnapshotLocations(remote);
  const remoteEntries = await listRemoteSnapshots(remote);
  const localNames = await listLocalSnapshots(remote.target.snapshotDir);
  const remoteMatches = remoteEntries.filter(entry => snapshotName.test(entry.name));
  const localMatches = localNames.filter(name => snapshotName.test(name));
  if (remoteMatches.length === 0 && localMatches.length === 0 && allowMissing) return null;
  const remoteName = lastCompletedSnapshot(
    remoteMatches.map(entry => ({ ...entry, isFile: () => true })),
    remote.target.snapshotDir,
    remote.target.sshHost,
  );
  const localName = lastCompletedSnapshot(
    localMatches.map(name => ({ name, isFile: () => true })),
    remote.target.snapshotDir,
    'workstation',
  );
  if (path.basename(remoteName) !== path.basename(localName)) {
    throw new Error('Server and workstation do not expose the same newest snapshot');
  }
  const localPath = path.posix.join(remote.target.snapshotDir, path.basename(localName));
  const handle = await fs.promises.open(localPath, 'r');
  try {
    const header = Buffer.alloc(5);
    await handle.read(header, 0, header.length, 0);
    if (header.toString() !== 'PGDMP') throw new Error('Local snapshot is not a PostgreSQL custom archive');
  } finally {
    await handle.close();
  }
  return {
    name: path.basename(remoteName),
    remotePath: remoteName,
    localPath,
  };
}

export function restoreFactsQuery() {
  return `
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
  'cronTimezone', current_setting('cron.timezone'),
  'pgCronVersion', (
    SELECT extversion
    FROM pg_extension
    WHERE extname = 'pg_cron'
  ),
  'runningCronJobs', (
    SELECT count(*)
    FROM cron.job_run_details
    WHERE status = 'running'
  ),
  'otherConnections', (
    SELECT count(*)
    FROM pg_stat_activity
    WHERE datname = current_database()
      AND pid <> pg_backend_pid()
      AND backend_type = 'client backend'
  )
);
`;
}

export function validateRestoredFacts(facts, {
  database: expectedDatabase,
  release,
  origins = null,
  upgradeOrigin = null,
  model,
  retentionDrainProbe = null,
}) {
  if (facts?.database !== expectedDatabase) throw new Error('Restored database has the wrong name');
  const history = release
    ? validateReleaseHistory(facts, { release, origins, upgradeOrigin })
    : { versions: facts.history, installation: 'unknown', upgradeOrigin: null };
  let cron = facts.cron;
  let restoredProbe;
  if (retentionDrainProbe) {
    const matching = (facts.cron || []).filter(job => job.jobname === retentionDrainProbe.name);
    if (matching.length !== 1) {
      throw new Error('Restored database lost the run-owned retention drain job');
    }
    const [probe] = matching;
    if (
      probe.active !== false
      || probe.schedule !== retentionDrainProbeSchedule
      || probe.username !== 'pin_glide'
      || probe.database !== 'pin_glide'
      || !probe.command.includes('pin_glide_fn_cleanup_expired_rows')
      || !/pg_sleep\([1-9][0-9]*\)/.test(probe.command)
    ) {
      throw new Error('Restored retention drain job is not the held run-owned probe');
    }
    cron = facts.cron.filter(job => job.jobname !== retentionDrainProbe.name);
    restoredProbe = {
      name: probe.jobname,
      active: probe.active,
      schedule: probe.schedule,
    };
  }
  const configuration = validateConfiguration({ ...facts, cron }, model, { cronActive: false });
  if (facts.pgCronVersion !== '1.6') throw new Error('Restored database did not restore pg_cron 1.6');
  if (Number(facts.runningCronJobs) !== 0 || Number(facts.otherConnections) !== 0) {
    throw new Error('Restored database has an unexpected writer');
  }
  return {
    database: facts.database,
    release: facts.release,
    history: history.versions,
    installation: history.installation,
    upgradeOrigin: history.upgradeOrigin,
    model: facts.model,
    retentionPolicies: configuration.retentionPolicies,
    cron: configuration.cron,
    pgCronVersion: facts.pgCronVersion,
    ...(restoredProbe ? { retentionDrainProbe: restoredProbe } : {}),
  };
}

export async function restoreFactsInDisposablePostgres(remote, {
  name,
  snapshot,
  image,
  factsQuery,
}, {
  waitMs = 60_000,
  sleepMs = 1_000,
} = {}) {
  if (!restoreContainer.test(name)) throw new Error('Disposable restore container name is invalid');
  if (typeof image !== 'string' || !/^[a-z0-9][a-z0-9./_:@-]+$/.test(image)) {
    throw new Error('Disposable restore image reference is invalid');
  }
  if (
    path.posix.dirname(snapshot) !== remote.target.snapshotDir
    || !snapshotName.test(path.posix.basename(snapshot))
  ) {
    throw new Error('Disposable restore snapshot path is invalid');
  }
  if (typeof factsQuery !== 'string' || factsQuery.trim() === '') {
    throw new Error('Restored facts query is missing');
  }
  const fileName = path.posix.basename(snapshot);
  let started = false;
  let failure;
  try {
    await remote.ssh([
      'sudo -n docker run --detach --rm',
      `--name ${quote(name)}`,
      '--network none --pull never',
      '--env POSTGRES_USER=pin_glide',
      '--env POSTGRES_DB=pin_glide',
      '--env POSTGRES_HOST_AUTH_METHOD=trust',
      `--volume ${quote(`${remote.target.snapshotDir}:/snapshots:ro`)}`,
      quote(image),
    ].join(' '), { timeoutMs: 120_000 });
    started = true;
    const attempts = Math.max(1, Math.ceil(waitMs / sleepMs));
    await remote.ssh(`
attempt=0
while [ "$attempt" -lt ${attempts} ]; do
    if sudo -n docker exec ${quote(name)} pg_isready -U pin_glide -d pin_glide >/dev/null 2>&1; then
        exit 0
    fi
    attempt=$((attempt + 1))
    sleep ${Math.max(1, Math.ceil(sleepMs / 1000))}
done
exit 1
`, { timeoutMs: waitMs + 30_000 });
    await remote.ssh(
      `sudo -n docker exec -i ${quote(name)} pg_restore --exit-on-error ` +
        `--username=pin_glide --dbname=pin_glide ${quote(`/snapshots/${fileName}`)}`,
      { timeoutMs: 600_000 },
    );
    const result = await remote.ssh(
      `sudo -n docker exec -i ${quote(name)} psql -X -v ON_ERROR_STOP=1 -A -t ` +
        '-U pin_glide -d pin_glide -f -',
      { input: factsQuery, timeoutMs: 120_000 },
    );
    const line = result.stdout
      .split('\n')
      .map(value => value.trim())
      .filter(value => value.startsWith('{'))
      .at(-1);
    if (!line) throw new Error('Restored facts query returned no JSON');
    return JSON.parse(line);
  } catch (error) {
    failure = error;
  } finally {
    if (started) {
      try {
        await remote.ssh(
          `if sudo -n docker container inspect ${quote(name)} >/dev/null 2>&1; then ` +
            `sudo -n docker rm -fv ${quote(name)} >/dev/null; fi`,
          { timeoutMs: 120_000 },
        );
      } catch (cleanupError) {
        failure = failure
          ? new AggregateError([failure, cleanupError], 'Restore and disposable container cleanup both failed')
          : cleanupError;
      }
    }
  }
  if (failure) throw failure;
}

export async function restoreSnapshotToDisposableDatabase(remote, snapshotPath, {
  release,
  origins,
  upgradeOrigin,
  model,
  postgresImage,
  retentionDrainProbe = null,
  restoreFacts = restoreFactsInDisposablePostgres,
} = {}) {
  if (remote.target?.database !== 'pin_glide') {
    throw new Error('Disposable restore target database is not pin_glide');
  }
  const name = `smoke_restore_${randomBytes(8).toString('hex')}`;
  const facts = await restoreFacts(remote, {
    name,
    snapshot: snapshotPath,
    image: postgresImage,
    factsQuery: restoreFactsQuery(),
  });
  return validateRestoredFacts(facts, {
    database: remote.target.database,
    release,
    origins,
    upgradeOrigin,
    model,
    retentionDrainProbe,
  });
}
