import { escapeSQLString, sqlTextArray } from './fixtures.mjs';
import { cronJobs, cronTimezones, retentionJobName } from './database.mjs';
import {
  identifier,
  markerPattern,
  retentionDigest,
  retentionMarker,
} from './retention_sql.mjs';

const retentionJob = retentionJobName;
const expectedRetentionJob = cronJobs.get(retentionJob);
if (!expectedRetentionJob) throw new Error('The adopted retention job has no expected configuration');

export function retentionStateQuery(plan, {
  senderID,
  receiverID,
  pairingID,
  liveTokenDigest,
  liveGuardDigest,
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
        AND parent_digest = ${identifier(liveGuardDigest, 'live guard digest')}
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
        AND database = ${escapeSQLString(expectedRetentionJob.database)}
        AND username = ${escapeSQLString(expectedRetentionJob.username)}
        AND schedule = ${escapeSQLString(expectedRetentionJob.schedule)}
        AND current_setting('cron.timezone') = ANY(${sqlTextArray([...cronTimezones])})
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
