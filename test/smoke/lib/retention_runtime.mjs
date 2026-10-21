import { randomBytes } from 'node:crypto';

import { withFailureJoiningCleanup } from './cleanup.mjs';
import { cronCommand } from './database.mjs';
import { escapeSQLString } from './fixtures.mjs';
import { retentionStateQuery } from './retention_checks.mjs';
import { retentionMarker } from './retention_fixtures.mjs';
import { quote } from './shell.mjs';

const stableJobName = 'pin-glide-retention-cleanup';

export function assertRetentionCounts(state, expected) {
  for (const [name, wanted] of Object.entries(expected)) {
    const actual = Number(name.split('.').reduce((current, key) => current?.[key], state));
    if (actual !== wanted) {
      throw new Error(`Retention state ${name} is ${actual}, want ${wanted}`);
    }
  }
}

export function retentionProbeCommand(probeName) {
  if (!/^[a-z0-9][a-z0-9-]{0,62}$/.test(probeName || '')) {
    throw new Error('Retention probe job name is invalid');
  }
  return `DO $smoke$
BEGIN
    PERFORM pin_glide.pin_glide_fn_cleanup_expired_rows(now());
    PERFORM cron.unschedule(${escapeSQLString(probeName)});
END
$smoke$;`;
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
  const job = await session.remote.sqlJSON(`
SELECT json_build_object(
  'jobID', jobid,
  'jobname', jobname,
  'database', database,
  'username', username,
  'schedule', schedule,
  'command', command,
  'active', active
) AS job
FROM cron.job
WHERE jobname = ${escapeSQLString(stableJobName)};
`);
  if (
    job?.jobname !== stableJobName
    || job.database !== 'pin_glide'
    || job.username !== 'pin_glide'
    || job.schedule !== '0 19 * * *'
    || job.command !== cronCommand
    || job.active !== true
  ) {
    throw new Error('The deployed retention job does not match the adopted owner, schedule and command');
  }
  return job;
}

export async function unknownWriterBlocksSnapshot(remote, application, sleepImpl = sleep) {
  if (typeof application !== 'string' || application === '' || application.length > 63) {
    throw new Error('Unknown-writer application name must be 1 to 63 characters');
  }
  const holder = remote.ssh(
    `${remote.compose} exec -T -e ${quote(`PGAPPNAME=${application}`)} postgres ` +
      `psql -X -v ON_ERROR_STOP=1 -U pin_glide -d pin_glide -c 'SELECT pg_sleep(20);'`,
    { timeoutMs: 30_000 },
  );
  holder.catch(() => {});
  return withFailureJoiningCleanup(
    async () => {
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
    async () => {
      await remote.sql(`
SELECT pg_terminate_backend(pid)
FROM pg_stat_activity
WHERE application_name = ${escapeSQLString(application)};
`);
      await holder.catch(() => {});
      await remote.assertNoExternalWriters();
    },
    'Unknown-writer probe and writer restoration both failed',
  );
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
SELECT coalesce(max(runid), 0)::bigint AS "maxRunID"
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
          return {
            jobID,
            probeName,
            runIDs: runs.map(run => run.runID),
            status: completed.status,
            completedAt: completed.endTime,
            command: cronCommand,
            selfRemoving: true,
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
