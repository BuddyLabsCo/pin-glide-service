import { escapeSQLString, sqlTextArray } from './fixtures.mjs';
import { retentionDigest, retentionMarker } from './retention_fixtures.mjs';

const retentionJob = 'pin-glide-retention-cleanup';

function markerPattern(plan) {
  return escapeSQLString(`${retentionMarker(plan)}%`);
}

function timestamp(value) {
  const parsed = new Date(value);
  if (!Number.isFinite(parsed.getTime())) throw new Error('Retention reference time is invalid');
  return `${escapeSQLString(parsed.toISOString())}::timestamptz`;
}

function identifier(value, label) {
  if (typeof value !== 'string' || value === '') throw new Error(`Retention ${label} is missing`);
  return escapeSQLString(value);
}

function positiveInteger(value, label) {
  if (!Number.isSafeInteger(value) || value <= 0) throw new Error(`Retention ${label} must be positive`);
  return String(value);
}

function guardedTransaction(body) {
  return `BEGIN;
SET LOCAL lock_timeout = '10s';
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION 'refusing smoke retention fixture outside pin_glide';
    END IF;
END
$$;
${body.trim()}
COMMIT;
`;
}

function deleteOwnedBody(plan) {
  const pattern = markerPattern(plan);
  return `
DELETE FROM release.release_schema_audits
WHERE executed_by LIKE ${pattern};
DELETE FROM cron.job_run_details
WHERE command LIKE ${pattern};
DELETE FROM pin_glide.service_diagnostics
WHERE request_id LIKE ${pattern};
DELETE FROM pin_glide.pairing_sessions
WHERE id LIKE ${pattern};
DELETE FROM pin_glide.identity_refresh_operations
WHERE operation_id LIKE ${pattern};
DELETE FROM pin_glide.identity_credentials
WHERE family_id LIKE ${pattern};
DELETE FROM pin_glide.delivery_deliveries
WHERE message_id LIKE ${pattern};
`;
}

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
  return `BEGIN;
SET LOCAL lock_timeout = '10s';
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION 'refusing smoke retention interruption outside pin_glide';
    END IF;
END
$$;
INSERT INTO pin_glide.retention_policies
    (schema_name, table_name, retention_days, date_column)
VALUES ('pin_glide', 'identity_accounts', 1, 'missing_retention_column')
ON CONFLICT (schema_name, table_name) DO UPDATE
    SET retention_days = EXCLUDED.retention_days,
        date_column = EXCLUDED.date_column;
SELECT pin_glide.pin_glide_fn_cleanup_expired_rows(${reference});
ROLLBACK;
`;
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
WHERE sender_id = ${identifier(plan.senders.find(sender => sender.label === 'pair2').id, 'Sender')}
  AND message_id = ${identifier(messageID, 'Message')}
  AND message_id LIKE ${escapeSQLString(`${retentionMarker(plan)}%`)};
`;
}

export function renderConfigurationDeleteProbeSQL(plan, {
  senderID,
  receiverID,
  pairingID,
}) {
  const marker = retentionMarker(plan);
  return `BEGIN;
SET LOCAL lock_timeout = '10s';
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION 'refusing smoke retention configuration probe outside pin_glide';
    END IF;
END
$$;
DELETE FROM pin_glide.model_configurations WHERE id = 'default';
SELECT json_build_object(
  'modelConfigurations', (SELECT count(*) FROM pin_glide.model_configurations),
  'account', (
    SELECT count(*) FROM pin_glide.identity_accounts
    WHERE id = ANY(${sqlTextArray(plan.accounts.map(account => account.id))})
  ),
  'sender', (
    SELECT count(*) FROM pin_glide.identity_sender_registrations
    WHERE id = ${identifier(senderID, 'Sender')}
      AND state = 'active'
  ),
  'receiver', (
    SELECT count(*) FROM pin_glide.identity_receiver_registrations
    WHERE id = ${identifier(receiverID, 'Receiver')}
      AND state = 'active'
  ),
  'receiverCredential', (
    SELECT count(*) FROM pin_glide.identity_receiver_credentials
    WHERE receiver_id = ${identifier(receiverID, 'Receiver')}
      AND revoked_at IS NULL
  ),
  'pairing', (
    SELECT count(*) FROM pin_glide.pairing_pairings
    WHERE id = ${identifier(pairingID, 'Pairing')}
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
    WHERE jobname = ${identifier(retentionJob, 'retention job')}
  )
) AS configuration_probe;
ROLLBACK;
`;
}

export function retentionStateQuery(plan, {
  senderID,
  receiverID,
  pairingID,
  liveTokenDigest,
}) {
  const marker = retentionMarker(plan);
  const pattern = markerPattern(plan);
  const exact = suffix => identifier(`${marker}-${suffix}`, suffix);
  const digest = suffix => identifier(retentionDigest(plan, suffix), `${suffix} digest`);
  return `
SELECT json_build_object(
  'aging', json_build_object(
    'deliveryOld', (
      SELECT count(*) FROM pin_glide.delivery_deliveries
      WHERE message_id = ${exact('delivery-old')}
    ),
    'deliveryFresh', (
      SELECT count(*) FROM pin_glide.delivery_deliveries
      WHERE message_id = ${exact('delivery-fresh')}
    ),
    'credentialOld', (
      SELECT count(*) FROM pin_glide.identity_credentials
      WHERE token_digest = ${digest('credential-old')}
    ),
    'credentialFresh', (
      SELECT count(*) FROM pin_glide.identity_credentials
      WHERE token_digest = ${digest('credential-fresh')}
    ),
    'operationOld', (
      SELECT count(*) FROM pin_glide.identity_refresh_operations
      WHERE operation_id = ${exact('operation-old')}
    ),
    'operationFresh', (
      SELECT count(*) FROM pin_glide.identity_refresh_operations
      WHERE operation_id = ${exact('operation-fresh')}
    ),
    'sessionOld', (
      SELECT count(*) FROM pin_glide.pairing_sessions
      WHERE id = ${exact('session-old')}
    ),
    'sessionFresh', (
      SELECT count(*) FROM pin_glide.pairing_sessions
      WHERE id = ${exact('session-fresh')}
    ),
    'diagnosticOld', (
      SELECT count(*) FROM pin_glide.service_diagnostics
      WHERE request_id = ${exact('diagnostic-old')}
    ),
    'diagnosticFresh', (
      SELECT count(*) FROM pin_glide.service_diagnostics
      WHERE request_id = ${exact('diagnostic-fresh')}
    ),
    'auditOld', (
      SELECT count(*) FROM release.release_schema_audits
      WHERE stdout = ${exact('audit-old')}
    ),
    'auditFresh', (
      SELECT count(*) FROM release.release_schema_audits
      WHERE stdout = ${exact('audit-fresh')}
    ),
    'cronOld', (
      SELECT count(*) FROM cron.job_run_details
      WHERE command = ${exact('cron-old')}
    ),
    'cronFresh', (
      SELECT count(*) FROM cron.job_run_details
      WHERE command = ${exact('cron-fresh')}
    ),
    'cronRunning', (
      SELECT count(*) FROM cron.job_run_details
      WHERE command = ${exact('cron-running')}
        AND end_time IS NULL
    )
  ),
  'boundary', json_build_object(
    'deliveryCutoff', (
      SELECT count(*) FROM pin_glide.delivery_deliveries
      WHERE message_id = ${exact('delivery-cutoff')}
    ),
    'deliveryPastCutoff', (
      SELECT count(*) FROM pin_glide.delivery_deliveries
      WHERE message_id = ${exact('delivery-past-cutoff')}
    ),
    'credentialCutoff', (
      SELECT count(*) FROM pin_glide.identity_credentials
      WHERE token_digest = ${digest('credential-cutoff')}
    ),
    'credentialPastCutoff', (
      SELECT count(*) FROM pin_glide.identity_credentials
      WHERE token_digest = ${digest('credential-past-cutoff')}
    ),
    'operationCutoff', (
      SELECT count(*) FROM pin_glide.identity_refresh_operations
      WHERE operation_id = ${exact('operation-cutoff')}
    ),
    'operationPastCutoff', (
      SELECT count(*) FROM pin_glide.identity_refresh_operations
      WHERE operation_id = ${exact('operation-past-cutoff')}
    ),
    'sessionCutoff', (
      SELECT count(*) FROM pin_glide.pairing_sessions
      WHERE id = ${exact('session-cutoff')}
    ),
    'sessionPastCutoff', (
      SELECT count(*) FROM pin_glide.pairing_sessions
      WHERE id = ${exact('session-past-cutoff')}
    ),
    'diagnosticCutoff', (
      SELECT count(*) FROM pin_glide.service_diagnostics
      WHERE request_id = ${exact('diagnostic-cutoff')}
    ),
    'diagnosticPastCutoff', (
      SELECT count(*) FROM pin_glide.service_diagnostics
      WHERE request_id = ${exact('diagnostic-past-cutoff')}
    ),
    'auditCutoff', (
      SELECT count(*) FROM release.release_schema_audits
      WHERE stdout = ${exact('audit-cutoff')}
    ),
    'auditPastCutoff', (
      SELECT count(*) FROM release.release_schema_audits
      WHERE stdout = ${exact('audit-past-cutoff')}
    ),
    'cronCutoff', (
      SELECT count(*) FROM cron.job_run_details
      WHERE command = ${exact('cron-cutoff')}
    ),
    'cronPastCutoff', (
      SELECT count(*) FROM cron.job_run_details
      WHERE command = ${exact('cron-past-cutoff')}
    )
  ),
  'live', json_build_object(
    'credential', (
      SELECT count(*) FROM pin_glide.identity_credentials
      WHERE token_digest = ${identifier(liveTokenDigest, 'live token digest')}
        AND revoked_at IS NULL
    ),
    'guard', (
      SELECT count(*) FROM pin_glide.identity_refresh_operations
      WHERE operation_id = ${exact('operation-live')}
        AND parent_digest = ${identifier(liveTokenDigest, 'live token digest')}
    )
  ),
  'durable', json_build_object(
    'accounts', (
      SELECT count(*) FROM pin_glide.identity_accounts
      WHERE id = ANY(${sqlTextArray(plan.accounts.map(account => account.id))})
    ),
    'senders', (
      SELECT count(*) FROM pin_glide.identity_sender_registrations
      WHERE id = ANY(${sqlTextArray(plan.senders.map(sender => sender.id))})
        AND state = 'active'
    ),
    'receiver', (
      SELECT count(*) FROM pin_glide.identity_receiver_registrations
      WHERE id = ${identifier(receiverID, 'Receiver')}
        AND state = 'active'
    ),
    'receiverCredential', (
      SELECT count(*) FROM pin_glide.identity_receiver_credentials
      WHERE receiver_id = ${identifier(receiverID, 'Receiver')}
        AND revoked_at IS NULL
    ),
    'pairing', (
      SELECT count(*) FROM pin_glide.pairing_pairings
      WHERE id = ${identifier(pairingID, 'Pairing')}
        AND state = 'active'
    ),
    'model', (
      SELECT count(*) FROM pin_glide.model_configurations
      WHERE id = 'default' AND enabled
    ),
    'retentionPolicies', (SELECT count(*) FROM pin_glide.retention_policies),
    'releaseDataScripts', (SELECT count(*) FROM release.release_data_scripts),
    'releaseDeployHistory', (SELECT count(*) FROM release.release_deploy_history),
    'releaseMigrationScripts', (SELECT count(*) FROM release.release_migration_scripts),
    'retentionJob', (
      SELECT count(*) FROM cron.job
      WHERE jobname = ${identifier(retentionJob, 'retention job')}
        AND database = 'pin_glide'
        AND username = 'pin_glide'
        AND schedule = '0 19 * * *'
        AND active
    )
  ),
  'owned', json_build_object(
    'deliveries', (
      SELECT count(*) FROM pin_glide.delivery_deliveries
      WHERE message_id LIKE ${pattern}
    ),
    'diagnostics', (
      SELECT count(*) FROM pin_glide.service_diagnostics
      WHERE request_id LIKE ${pattern}
    ),
    'audits', (
      SELECT count(*) FROM release.release_schema_audits
      WHERE executed_by LIKE ${pattern}
    )
  )
) AS retention_state;
`;
}

export function runOwnedPrivacyQuery(plan, {
  receiverIDs = [],
  requestIDs = [],
  subjects = [],
}) {
  const senderIDs = sqlTextArray(plan.senders.map(sender => sender.id));
  const accountIDs = sqlTextArray(plan.accounts.map(account => account.id));
  const receivers = sqlTextArray(receiverIDs);
  const requests = sqlTextArray(requestIDs);
  const ownedSubjects = sqlTextArray(subjects);
  const pattern = markerPattern(plan);
  return `
SELECT json_build_object(
  'accounts', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM pin_glide.identity_accounts AS owned
    WHERE owned.id = ANY(${accountIDs})
  ),
  'senders', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM pin_glide.identity_sender_registrations AS owned
    WHERE owned.id = ANY(${senderIDs})
  ),
  'credentials', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM pin_glide.identity_credentials AS owned
    WHERE owned.sender_id = ANY(${senderIDs})
  ),
  'refreshOperations', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM pin_glide.identity_refresh_operations AS owned
    WHERE owned.sender_id = ANY(${senderIDs})
  ),
  'receivers', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM pin_glide.identity_receiver_registrations AS owned
    WHERE owned.id = ANY(${receivers})
  ),
  'receiverCredentials', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM pin_glide.identity_receiver_credentials AS owned
    WHERE owned.receiver_id = ANY(${receivers})
  ),
  'pairings', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM pin_glide.pairing_pairings AS owned
    WHERE owned.sender_id = ANY(${senderIDs})
       OR owned.receiver_id = ANY(${receivers})
  ),
  'pairingSessions', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM pin_glide.pairing_sessions AS owned
    WHERE owned.authorized_sender_id = ANY(${senderIDs})
       OR owned.receiver_id = ANY(${receivers})
  ),
  'deliveries', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM pin_glide.delivery_deliveries AS owned
    WHERE owned.sender_id = ANY(${senderIDs})
       OR owned.receiver_id = ANY(${receivers})
  ),
  'diagnostics', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM pin_glide.service_diagnostics AS owned
    WHERE owned.request_id = ANY(${requests})
       OR owned.subject = ANY(${ownedSubjects})
  ),
  'schemaAudits', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM release.release_schema_audits AS owned
    WHERE owned.executed_by LIKE ${pattern}
       OR owned.stdout LIKE ${pattern}
       OR owned.stderr LIKE ${pattern}
  ),
  'cronRuns', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM cron.job_run_details AS owned
    WHERE owned.command LIKE ${pattern}
  ),
  'dataScripts', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM release.release_data_scripts AS owned
  ),
  'deployHistory', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM release.release_deploy_history AS owned
  ),
  'migrationScripts', (
    SELECT coalesce(json_agg(to_jsonb(owned)), '[]'::json)
    FROM release.release_migration_scripts AS owned
  )
) AS run_owned_privacy;
`;
}
