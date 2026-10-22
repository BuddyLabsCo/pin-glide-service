import { randomBytes } from 'node:crypto';
import path from 'node:path';

import { validateConfiguration, validateReleaseHistory } from './database.mjs';
import { databaseConfigurationFactsQuery } from './database-facts.mjs';
import { quote, snapshotName } from './local.mjs';

const restoreContainer = /^smoke_restore_[a-f0-9]{16}$/;

export function restoreFactsQuery() {
  return databaseConfigurationFactsQuery({ includeRestoreChecks: true });
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
      || probe.schedule !== retentionDrainProbe.schedule
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
    await remote.exec([
      'sudo -n docker run --detach --rm',
      `--name ${quote(name)}`,
      '--network none --pull never',
      '--env POSTGRES_USER=pin_glide',
      '--env POSTGRES_DB=pin_glide',
      '--env POSTGRES_HOST_AUTH_METHOD=trust',
      `--volume ${quote(`${remote.target.snapshotDir}:/snapshots:ro`)}`,
      quote(image),
    ].join(' '), { timeoutMs: 120_000, phase: 'create' });
    started = true;
    const attempts = Math.max(1, Math.ceil(waitMs / sleepMs));
    await remote.exec(`
attempt=0
while [ "$attempt" -lt ${attempts} ]; do
    if sudo -n docker exec ${quote(name)} pg_isready -U pin_glide -d pin_glide >/dev/null 2>&1; then
        exit 0
    fi
    attempt=$((attempt + 1))
    sleep ${Math.max(1, Math.ceil(sleepMs / 1000))}
done
exit 1
`, { timeoutMs: waitMs + 30_000, phase: 'ready' });
    await remote.exec(
      `sudo -n docker exec -i ${quote(name)} pg_restore --exit-on-error ` +
        `--username=pin_glide --dbname=pin_glide ${quote(`/snapshots/${fileName}`)}`,
      { timeoutMs: 600_000, phase: 'restore' },
    );
    const result = await remote.exec(
      `sudo -n docker exec -i ${quote(name)} psql -X -v ON_ERROR_STOP=1 -A -t ` +
        '-U pin_glide -d pin_glide -f -',
      { input: factsQuery, timeoutMs: 120_000, phase: 'facts' },
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
        await remote.exec(
          `if sudo -n docker container inspect ${quote(name)} >/dev/null 2>&1; then ` +
            `sudo -n docker rm -fv ${quote(name)} >/dev/null; fi`,
          { timeoutMs: 120_000, phase: 'cleanup' },
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
