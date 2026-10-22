import { escapeSQLString, sqlTextArray } from './fixtures.mjs';
import { retentionJobName } from './database.mjs';
import {
  deleteOwnedBody,
  guardedTransaction,
  sqlStringLiteral,
  markerPattern,
  positiveInteger,
  retentionMarker,
  timestamp,
} from './retention_sql.mjs';

export { retentionDigest, retentionMarker } from './retention_sql.mjs';
export { retentionStateQuery, runOwnedPrivacyQuery } from './retention_observations.mjs';

const retentionJob = retentionJobName;

function bigintArray(runIDs) {
  if (!Array.isArray(runIDs)) throw new Error('Retention cron run IDs must be an array');
  const values = runIDs.map(value => positiveInteger(value, 'cron run ID'));
  return `ARRAY[${values.join(', ')}]::bigint[]`;
}

export function renderRetentionCleanupSQL(plan, runIDs = []) {
  const marker = retentionMarker(plan);
  const pattern = markerPattern(plan);
  return guardedTransaction(`
SELECT cron.unschedule(jobname)
FROM cron.job
WHERE jobname LIKE ${escapeSQLString(`${marker}-probe-%`)};

${deleteOwnedBody(plan)}
DELETE FROM cron.job_run_details
WHERE command LIKE ${pattern}
   OR runid = ANY(${bigintArray(runIDs)});
`);
}

export function renderRetentionInterruptedSQL(referenceTime) {
  const reference = timestamp(referenceTime);
  return guardedTransaction(`
INSERT INTO pin_glide.retention_policies
    (schema_name, table_name, retention_days, date_column)
VALUES ('pin_glide', 'identity_accounts', 1, 'missing_retention_column')
ON CONFLICT (schema_name, table_name) DO UPDATE
    SET retention_days = EXCLUDED.retention_days,
        date_column = EXCLUDED.date_column;
SELECT pin_glide.pin_glide_fn_cleanup_expired_rows(${reference});
`, 'retention interruption', 'ROLLBACK');
}

export function cleanupCallSQL(referenceTime) {
  return `
SELECT json_build_object(
  'removed', pin_glide.pin_glide_fn_cleanup_expired_rows(${timestamp(referenceTime)})
) AS cleanup;
`;
}

export function renderDeliveryAgeSQL(plan, messageID) {
  return `
UPDATE pin_glide.delivery_deliveries
SET created_at = now() - interval '8 days',
    completed_at = now() - interval '8 days'
WHERE sender_id = ${sqlStringLiteral(plan.senders.find(sender => sender.label === 'pair2').id, 'Sender')}
  AND message_id = ${sqlStringLiteral(messageID, 'Message')}
  AND message_id LIKE ${escapeSQLString(`${retentionMarker(plan)}%`)};
`;
}

export function renderConfigurationDeleteProbeSQL(plan, {
  senderID,
  receiverID,
  pairingID,
}) {
  const marker = retentionMarker(plan);
  return guardedTransaction(`
DELETE FROM pin_glide.model_configurations WHERE id = 'default';
SELECT json_build_object(
  'modelConfigurations', (SELECT count(*) FROM pin_glide.model_configurations),
  'account', (
    SELECT count(*) FROM pin_glide.identity_accounts
    WHERE id = ANY(${sqlTextArray(plan.accounts.map(account => account.id))})
  ),
  'sender', (
    SELECT count(*) FROM pin_glide.identity_sender_registrations
    WHERE id = ${sqlStringLiteral(senderID, 'Sender')}
      AND state = 'active'
  ),
  'receiver', (
    SELECT count(*) FROM pin_glide.identity_receiver_registrations
    WHERE id = ${sqlStringLiteral(receiverID, 'Receiver')}
      AND state = 'active'
  ),
  'receiverCredential', (
    SELECT count(*) FROM pin_glide.identity_receiver_credentials
    WHERE receiver_id = ${sqlStringLiteral(receiverID, 'Receiver')}
      AND revoked_at IS NULL
  ),
  'pairing', (
    SELECT count(*) FROM pin_glide.pairing_pairings
    WHERE id = ${sqlStringLiteral(pairingID, 'Pairing')}
      AND state = 'active'
  ),
  'deliveryHistory', (
    SELECT count(*) FROM pin_glide.delivery_deliveries
    WHERE message_id LIKE ${markerPattern(plan)}
  ),
  'diagnosticHistory', (
    SELECT count(*) FROM pin_glide.service_diagnostics
    WHERE request_id LIKE ${markerPattern(plan)}
  ),
  'auditHistory', (
    SELECT count(*) FROM release.release_schema_audits
    WHERE executed_by LIKE ${markerPattern(plan)}
  ),
  'releaseDataScripts', (SELECT count(*) FROM release.release_data_scripts),
  'releaseDeployHistory', (SELECT count(*) FROM release.release_deploy_history),
  'releaseMigrationScripts', (SELECT count(*) FROM release.release_migration_scripts),
  'retentionPolicies', (SELECT count(*) FROM pin_glide.retention_policies),
  'retentionJob', (
    SELECT count(*) FROM cron.job
    WHERE jobname = ${sqlStringLiteral(retentionJob, 'retention job')}
  )
) AS configuration_probe;
`, 'retention configuration probe', 'ROLLBACK');
}
