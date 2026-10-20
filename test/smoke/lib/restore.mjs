import { randomBytes } from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';

import { validateConfiguration, validateReleaseHistory } from './database.mjs';
import { quote, snapshotName } from './remote.mjs';

const databaseName = /^[a-z][a-z0-9_]{0,62}$/;

function database(db) {
  if (!databaseName.test(db)) throw new Error('Restore database name is invalid');
  return `"${db}"`;
}

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
}) {
  if (facts?.database !== expectedDatabase) throw new Error('Restored database has the wrong name');
  const history = release
    ? validateReleaseHistory(facts, { release, origins, upgradeOrigin })
    : { versions: facts.history, installation: 'unknown', upgradeOrigin: null };
  const configuration = validateConfiguration(facts, model, { cronActive: false });
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
  };
}

export async function restoreSnapshotToDisposableDatabase(remote, snapshotPath, {
  release,
  origins,
  upgradeOrigin,
  model,
} = {}) {
  const name = `smoke_restore_${randomBytes(8).toString('hex')}`;
  const target = database(name);
  await remote.sqlOn('postgres', `CREATE DATABASE ${target};`);
  let restored;
  let failure;
  try {
    const archive = await remote.restoreList(snapshotPath, { timeoutMs: 120_000 });
    if (!archive.stdout.trim()) throw new Error('Restore archive has no pg_restore entries');
    await remote.restoreDump(name, snapshotPath, { timeoutMs: 600_000 });
    const facts = await remote.sqlJSONOn(name, restoreFactsQuery(), { timeoutMs: 120_000 });
    restored = validateRestoredFacts(facts, {
      database: name,
      release,
      origins,
      upgradeOrigin,
      model,
    });
  } catch (error) {
    failure = error;
  } finally {
    try {
      await remote.sqlOn('postgres', `DROP DATABASE IF EXISTS ${target} WITH (FORCE);`, {
        timeoutMs: 120_000,
      });
    } catch (cleanupError) {
      failure = failure
        ? new AggregateError([failure, cleanupError], 'Restore and disposable database cleanup both failed')
        : cleanupError;
    }
  }
  if (failure) throw failure;
  return restored;
}
