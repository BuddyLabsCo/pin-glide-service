export function databaseConfigurationFactsQuery({
  includeReleaseLedgers = false,
  includeRestoreChecks = false,
  includeCronBackends = false,
} = {}) {
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
  'runningCronJobs', (
    SELECT count(*)
    FROM cron.job_run_details
    WHERE status = 'running'
  )${includeCronBackends ? `,
  'cronBackends', (
    SELECT count(*)
    FROM pg_stat_activity
    WHERE application_name = 'pg_cron'
  )` : ''}${includeRestoreChecks ? `,
  'pgCronVersion', (
    SELECT extversion
    FROM pg_extension
    WHERE extname = 'pg_cron'
  ),
  'otherConnections', (
    SELECT count(*)
    FROM pg_stat_activity
    WHERE datname = current_database()
      AND pid <> pg_backend_pid()
      AND backend_type = 'client backend'
  )` : ''}${includeReleaseLedgers ? `,
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
  )` : ''}
);
`;
}
