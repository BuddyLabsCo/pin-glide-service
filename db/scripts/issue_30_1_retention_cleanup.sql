-- The three adopted retention tiers, one row per whole-table policy. Seven-day
-- targets filter on the clock their table already declares; durable R state,
-- effective Pairings and the release ledgers have no row here and are never
-- purge targets.
INSERT INTO pin_glide.retention_policies
(schema_name, table_name, retention_days, date_column)
VALUES
('pin_glide', 'delivery_deliveries', 7, 'created_at'),
('pin_glide', 'identity_credentials', 7, 'expires_at'),
('pin_glide', 'identity_refresh_operations', 7, 'expires_at'),
('pin_glide', 'pairing_sessions', 7, 'expires_at'),
('pin_glide', 'service_diagnostics', 30, 'occurred_at')
ON CONFLICT (schema_name, table_name) DO UPDATE
    SET
        retention_days = EXCLUDED.retention_days,
        date_column = EXCLUDED.date_column;

-- 03:00 Asia/Shanghai is 19:00 UTC the day before; the stored schedule is UTC
-- so it does not follow the database session time zone. Re-running this script
-- updates the named job in place and leaves its identity alone.
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
