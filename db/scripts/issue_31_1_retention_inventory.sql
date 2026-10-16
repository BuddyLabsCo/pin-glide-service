-- Complete the adopted whole-table inventory with the thirty-day history
-- tiers the first retention script did not reach: schema apply/check audit
-- output and pg_cron's own execution history. The cron clock is the native
-- nullable end_time, so a job run still in progress is never deleted.
INSERT INTO pin_glide.retention_policies
(schema_name, table_name, retention_days, date_column)
VALUES
('release', 'release_schema_audits', 30, 'executed_at'),
('cron', 'job_run_details', 30, 'end_time')
ON CONFLICT (schema_name, table_name) DO UPDATE
    SET
        retention_days = EXCLUDED.retention_days,
        date_column = EXCLUDED.date_column;

-- Cleanup configuration and the dynamic DELETE routine stay with the release
-- owner: no other role can point cleanup at a table or run it.
REVOKE ALL ON TABLE pin_glide.retention_policies FROM PUBLIC;
REVOKE ALL ON FUNCTION
pin_glide.pin_glide_fn_cleanup_expired_rows(timestamptz) FROM PUBLIC;

-- The scheduled job keeps its stable owner and name: declaring it again
-- rewrites the schedule and command of the row this owner holds instead of
-- adding a second one, and this release declares it instead of assuming the
-- earlier script already did.
SELECT cron.schedule(
    'pin-glide-retention-cleanup',
    '0 19 * * *',
    'SELECT pin_glide.pin_glide_fn_cleanup_expired_rows(now())'
);

-- cron.schedule converges schedule and command but not run state, so the job
-- this script owns is activated explicitly.
SELECT cron.alter_job(
    (
        SELECT jobid FROM cron.job
        WHERE jobname = 'pin-glide-retention-cleanup'
    ),
    active => true
);

-- Retirement: deleting a declaration never removes the deployed row, so this
-- release unschedules every job in its own name prefix that it no longer
-- declares. Jobs with any other name are left alone. A recorded script is
-- never applied twice, so a later release that stops declaring a job retires
-- it from its own script.
SELECT cron.unschedule(jobname)
FROM cron.job
WHERE
    jobname LIKE 'pin-glide-%'
    AND jobname <> 'pin-glide-retention-cleanup';
