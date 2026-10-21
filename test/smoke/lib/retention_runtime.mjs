import { randomBytes } from 'node:crypto';

import { withFailureJoiningCleanup } from './cleanup.mjs';
import {
  cronCommand,
  cronJobs,
  cronTimezones,
  retentionJobName,
} from './database.mjs';
import { escapeSQLString } from './fixtures.mjs';
import { retentionStateQuery } from './retention_checks.mjs';
import { retentionMarker } from './retention_fixtures.mjs';
import { quote } from './shell.mjs';

const stableJobName = retentionJobName;
export const retentionDrainProbeSchedule = '0 0 1 1 *';

function assertProbeName(probeName) {
  if (!/^[a-z0-9][a-z0-9-]{0,62}$/.test(probeName || '')) {
    throw new Error('Retention probe job name is invalid');
  }
}

export function retentionProbeCleanupSQL(probeName) {
  assertProbeName(probeName);
  return `
SELECT cron.unschedule(jobname)
FROM cron.job
WHERE jobname = ${escapeSQLString(probeName)};
SELECT pg_cancel_backend(pid)
FROM pg_stat_activity
WHERE application_name = 'pg_cron'
  AND query LIKE ${escapeSQLString(`%${probeName}%`)};
`;
}

export function retentionProbeHistoryCleanupSQL(probeName) {
  assertProbeName(probeName);
  return `
DELETE FROM cron.job_run_details
WHERE command LIKE ${escapeSQLString(`%${probeName}%`)};
`;
}

export function terminateApplicationBackendsSQL(application) {
  if (typeof application !== 'string' || application === '' || application.length > 63) {
    throw new Error('Unknown-writer application name must be 1 to 63 characters');
  }
  return `
SELECT pg_terminate_backend(pid)
FROM pg_stat_activity
WHERE application_name = ${escapeSQLString(application)};
`;
}

export function assertRetentionCounts(state, expected) {
  for (const [name, wanted] of Object.entries(expected)) {
    const actual = Number(name.split('.').reduce((current, key) => current?.[key], state));
    if (actual !== wanted) {
      throw new Error(`Retention state ${name} is ${actual}, want ${wanted}`);
    }
  }
}

export function retentionProbeCommand(probeName) {
  assertProbeName(probeName);
  return `DO $smoke$
BEGIN
    PERFORM pin_glide.pin_glide_fn_cleanup_expired_rows(now());
    PERFORM cron.unschedule(${escapeSQLString(probeName)});
END
$smoke$;`;
}

export function retentionDrainProbeCommand(probeName, sleepSeconds = 8) {
  assertProbeName(probeName);
  if (!Number.isSafeInteger(sleepSeconds) || sleepSeconds < 1 || sleepSeconds > 50) {
    throw new Error('Retention drain probe sleep must be between 1 and 50 seconds');
  }
  return `DO $smoke$
BEGIN
    PERFORM pin_glide.pin_glide_fn_cleanup_expired_rows('2000-01-01T00:00:00Z'::timestamptz);
    PERFORM pg_sleep(${sleepSeconds});
END
$smoke$;
-- ${probeName}`;
}

export function jsonFromOutput(result, label) {
  const line = result.stdout
    .split('\n')
    .map(value => value.trim())
    .filter(value => value.startsWith('{'))
    .at(-1);
  if (!line) throw new Error(`${label} returned no JSON evidence`);
  try {
    return JSON.parse(line);
  } catch {
    throw new Error(`${label} returned invalid JSON evidence`);
  }
}

export async function databaseNow(session) {
  const result = await session.remote.sqlJSON(`
SELECT json_build_object('now', now()) AS database_time;
`);
  const value = new Date(result.now);
  if (!Number.isFinite(value.getTime())) throw new Error('Database did not report a usable time');
  return value.toISOString();
}

export async function stableRetentionJob(session) {
  const expected = cronJobs.get(stableJobName);
  if (!expected) throw new Error('The adopted retention job has no expected configuration');
  const job = await session.remote.sqlJSON(`
SELECT json_build_object(
  'jobID', jobid,
  'jobname', jobname,
  'database', database,
  'username', username,
  'schedule', schedule,
  'command', command,
  'active', active,
  'cronTimezone', current_setting('cron.timezone')
) AS job
FROM cron.job
WHERE jobname = ${escapeSQLString(stableJobName)};
`);
  if (
    job?.jobname !== stableJobName
    || job.database !== expected.database
    || job.username !== expected.username
    || job.schedule !== expected.schedule
    || job.command !== expected.command
    || job.active !== true
    || !cronTimezones.has(job.cronTimezone)
  ) {
    throw new Error('The deployed retention job does not match the adopted UTC schedule or owner');
  }
  return job;
}

export async function holdExternalWriter(remote, application, sleepImpl = sleep) {
  const terminate = terminateApplicationBackendsSQL(application);
  const holder = remote.ssh(
    `${remote.compose} exec -T -e ${quote(`PGAPPNAME=${application}`)} postgres ` +
      `psql -X -v ON_ERROR_STOP=1 -U pin_glide -d pin_glide -c 'SELECT pg_sleep(20);'`,
    { timeoutMs: 30_000 },
  );
  holder.catch(() => {});
  try {
    const deadline = Date.now() + 10_000;
    for (;;) {
      const writers = await remote.sqlJSON(`
SELECT json_build_object('connections', count(*))
FROM pg_stat_activity
WHERE application_name = ${escapeSQLString(application)};
`);
      if (Number(writers.connections) > 0) break;
      if (Date.now() >= deadline) throw new Error('Controlled external writer did not connect');
      await sleepImpl(100);
    }
  } catch (error) {
    await remote.sql(terminate).catch(() => {});
    await holder.catch(() => {});
    throw error;
  }
  return {
    application,
    async cleanup() {
      await remote.sql(terminate);
      await holder.catch(() => {});
      await remote.assertNoExternalWriters();
    },
  };
}

export async function unknownWriterBlocksSnapshot(remote, application, sleepImpl = sleep) {
  const writer = await holdExternalWriter(remote, application, sleepImpl);
  return withFailureJoiningCleanup(
    async () => {
      let blocked = false;
      try {
        await remote.assertNoExternalWriters();
      } catch (error) {
        if (!/Another database writer/.test(error.message)) throw error;
        blocked = true;
      }
      if (!blocked) throw new Error('The snapshot writer guard accepted a connected external writer');
      return { blocked: true, application };
    },
    () => writer.cleanup(),
    'Unknown-writer probe and writer restoration both failed',
  );
}

async function removeRetentionProbe(remote, { probeName }, {
  pollMs,
  timeoutMs,
  sleepImpl,
}) {
  const pattern = escapeSQLString(`%${probeName}%`);
  await remote.sql(retentionProbeCleanupSQL(probeName));
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const remaining = await remote.sqlJSON(`
SELECT json_build_object(
  'cronBackends', (
    SELECT count(*)
    FROM pg_stat_activity
    WHERE application_name = 'pg_cron'
      AND query LIKE ${pattern}
  )
);
`);
    if (Number(remaining.cronBackends) === 0) break;
    if (Date.now() >= deadline) throw new Error('Retention drain probe SQL did not stop');
    await sleepImpl(pollMs);
  }
  await remote.sql(retentionProbeHistoryCleanupSQL(probeName));
}

function drainProbeStateQuery(probe) {
  return `
SELECT json_build_object(
  'jobExists', EXISTS (SELECT 1 FROM cron.job WHERE jobid = ${probe.jobID}),
  'schedule', (SELECT schedule FROM cron.job WHERE jobid = ${probe.jobID}),
  'active', (SELECT active FROM cron.job WHERE jobid = ${probe.jobID}),
  'runID', ${probe.runID},
  'status', (
    SELECT status
    FROM cron.job_run_details
    WHERE jobid = ${probe.jobID} AND runid = ${probe.runID}
  ),
  'returnMessage', (
    SELECT return_message
    FROM cron.job_run_details
    WHERE jobid = ${probe.jobID} AND runid = ${probe.runID}
  ),
  'endTime', (
    SELECT end_time
    FROM cron.job_run_details
    WHERE jobid = ${probe.jobID} AND runid = ${probe.runID}
  ),
  'cronBackends', (
    SELECT count(*)
    FROM pg_stat_activity
    WHERE application_name = 'pg_cron'
      AND query LIKE ${escapeSQLString(`%${probe.probeName}%`)}
  )
);
`;
}

export async function startRunningRetentionProbe(remote, {
  runId,
  sleepSeconds = 8,
  pollMs = 200,
  timeoutMs = 30_000,
  sleepImpl = sleep,
} = {}) {
  if (typeof runId !== 'string' || !/^[a-z0-9][a-z0-9-]{0,35}$/.test(runId)) {
    throw new Error('Recovery retention run ID is invalid');
  }
  const probeName = `${runId}-retention-drain-${randomBytes(3).toString('hex')}`;
  assertProbeName(probeName);
  const command = retentionDrainProbeCommand(probeName, sleepSeconds);
  const baseline = await remote.sqlJSON(`
SELECT json_build_object(
  'maxRunID', coalesce(max(runid), 0)::bigint
) AS baseline
FROM cron.job_run_details;
`);
  let jobID;
  try {
    const scheduled = await remote.sqlJSON(`
SELECT json_build_object(
  'jobID', cron.schedule(
    ${escapeSQLString(probeName)},
    '1 second',
    ${escapeSQLString(command)}
  )
) AS scheduled;
`);
    jobID = Number(scheduled.jobID);
    if (!Number.isSafeInteger(jobID) || jobID <= 0) {
      throw new Error('Retention drain probe returned no job ID');
    }

    const deadline = Date.now() + timeoutMs;
    let observedRunID;
    for (;;) {
      const observed = await remote.sqlJSON(`
SELECT json_build_object(
  'runID', coalesce(max(runid) FILTER (WHERE status = 'running'), 0),
  'runningRuns', count(*) FILTER (WHERE status = 'running'),
  'cronBackends', (
    SELECT count(*)
    FROM pg_stat_activity
    WHERE application_name = 'pg_cron'
      AND query LIKE ${escapeSQLString(`%${probeName}%`)}
  )
)
FROM cron.job_run_details
WHERE jobid = ${jobID}
  AND runid > ${Number(baseline.maxRunID)};
`);
      if (Number(observed.runningRuns) === 1 && Number(observed.cronBackends) === 1) {
        observedRunID = Number(observed.runID);
        break;
      }
      if (Date.now() >= deadline) throw new Error('Retention drain probe did not start');
      await sleepImpl(pollMs);
    }
    await remote.sql(`
SELECT cron.alter_job(
  ${jobID},
  schedule := ${escapeSQLString(retentionDrainProbeSchedule)}
);
`);
    const probe = { jobID, probeName, runID: observedRunID };
    return {
      ...probe,
      command,
      observedRunning: true,
      async inspect() {
        return remote.sqlJSON(drainProbeStateQuery(probe));
      },
      cleanup: () => removeRetentionProbe(remote, probe, {
        pollMs,
        timeoutMs,
        sleepImpl,
      }),
    };
  } catch (error) {
    if (jobID) {
      try {
        await removeRetentionProbe(remote, {
          jobID,
          probeName,
        }, {
          pollMs,
          timeoutMs,
          sleepImpl,
        });
      } catch (cleanupError) {
        throw new AggregateError(
          [error, cleanupError],
          'Retention drain probe and its job cleanup both failed',
        );
      }
    }
    throw error;
  }
}

export async function retentionState(session, plan, fixture) {
  return session.remote.sqlJSON(retentionStateQuery(plan, fixture));
}

function sleep(milliseconds) {
  return new Promise(resolve => setTimeout(resolve, milliseconds));
}

export async function runScheduledCleanupProbe(session, {
  pollMs = 200,
  timeoutMs = 30_000,
  sleepImpl = sleep,
} = {}) {
  const plan = session.plan;
  const probeName = `${retentionMarker(plan)}-probe-${randomBytes(4).toString('hex')}`;
  const probeCommand = retentionProbeCommand(probeName);
  const baseline = await session.remote.sqlJSON(`
SELECT json_build_object(
  'maxRunID', coalesce(max(runid), 0)::bigint
) AS baseline
FROM cron.job_run_details;
`);
  const scheduled = await session.remote.sqlJSON(`
SELECT json_build_object(
  'jobID', cron.schedule(
    ${escapeSQLString(probeName)},
    '1 second',
    ${escapeSQLString(probeCommand)}
  )
) AS scheduled;
`);
  const jobID = Number(scheduled.jobID);
  if (!Number.isSafeInteger(jobID) || jobID <= 0) {
    throw new Error('Scheduled cleanup probe returned no job ID');
  }

  return withFailureJoiningCleanup(
    async () => {
      const deadline = Date.now() + timeoutMs;
      for (;;) {
        const observed = await session.remote.sqlJSON(`
SELECT json_build_object(
  'runs', coalesce(json_agg(json_build_object(
    'runID', runid,
    'status', status,
    'startTime', start_time,
    'endTime', end_time,
    'returnMessage', return_message
  ) ORDER BY runid), '[]'::json)
)
FROM cron.job_run_details
WHERE jobid = ${jobID}
  AND runid > ${Number(baseline.maxRunID)}
  AND end_time IS NOT NULL;
`);
        const runs = (observed.runs || [])
          .map(run => ({ ...run, runID: Number(run.runID) }))
          .filter(run => run.endTime);
        for (const run of runs) session.retentionRunIDs.add(run.runID);
        const failed = runs.find(run => run.status !== 'succeeded');
        if (failed) throw new Error(`Scheduled cleanup probe ${failed.status}: ${failed.returnMessage || 'no detail'}`);
        const completed = runs.find(run => run.status === 'succeeded');
        if (completed) {
          const remaining = await session.remote.sqlJSON(`
SELECT json_build_object(
  'jobExists', EXISTS (SELECT 1 FROM cron.job WHERE jobid = ${jobID})
) AS remaining;
`);
          if (remaining.jobExists) {
            throw new Error('Scheduled cleanup probe remained scheduled after its successful run');
          }
          return {
            jobID,
            probeName,
            runIDs: runs.map(run => run.runID),
            status: completed.status,
            completedAt: completed.endTime,
            command: cronCommand,
            selfRemoving: !remaining.jobExists,
          };
        }
        if (Date.now() >= deadline) throw new Error('Scheduled cleanup probe did not complete');
        await sleepImpl(pollMs);
      }
    },
    () => session.remote.sql(`
SELECT cron.unschedule(jobname)
FROM cron.job
WHERE jobname = ${escapeSQLString(probeName)};
`),
    'Scheduled cleanup probe and job removal both failed',
  );
}
