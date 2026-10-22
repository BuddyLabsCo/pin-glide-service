import { escapeSQLString } from '../support/sql.mjs';
import {
  deleteOwnedBody,
  guardedTransaction,
  sqlStringLiteral,
  positiveInteger,
  retentionDigest,
  retentionMarker,
  timestamp,
} from './sql.mjs';

export { retentionDigest, retentionMarker } from './sql.mjs';

export function renderRetentionAgingSQL(plan, {
  referenceTime,
  senderID,
  receiverID,
  pairingID,
  jobID,
}) {
  const reference = timestamp(referenceTime);
  const sender = sqlStringLiteral(senderID, 'Sender');
  const receiver = sqlStringLiteral(receiverID, 'Receiver');
  const pairing = sqlStringLiteral(pairingID, 'Pairing');
  const job = positiveInteger(jobID, 'cron job ID');
  const marker = retentionMarker(plan);
  return guardedTransaction(`
${deleteOwnedBody(plan)}

INSERT INTO pin_glide.delivery_deliveries
    (id, sender_id, message_id, receiver_id, pairing_id, status, created_at, completed_at)
VALUES
    (${sqlStringLiteral(`${marker}-delivery-old`, 'Delivery ID')}, ${sender},
        ${sqlStringLiteral(`${marker}-delivery-old`, 'Message')}, ${receiver}, ${pairing}, 'offline',
        ${reference} - interval '8 days', ${reference} - interval '8 days'),
    (${sqlStringLiteral(`${marker}-delivery-fresh`, 'Delivery ID')}, ${sender},
        ${sqlStringLiteral(`${marker}-delivery-fresh`, 'Message')}, ${receiver}, ${pairing}, 'offline',
        ${reference} - interval '6 days', ${reference} - interval '6 days');

INSERT INTO pin_glide.identity_credentials
    (sender_id, family_id, scope, token_digest, issued_at, expires_at)
VALUES
    (${sender}, ${sqlStringLiteral(`${marker}-credential-old`, 'credential family')}, 'refresh',
        ${sqlStringLiteral(retentionDigest(plan, 'credential-old'), 'credential digest')},
        ${reference} - interval '8 days', ${reference} - interval '8 days'),
    (${sender}, ${sqlStringLiteral(`${marker}-credential-fresh`, 'credential family')}, 'refresh',
        ${sqlStringLiteral(retentionDigest(plan, 'credential-fresh'), 'credential digest')},
        ${reference}, ${reference} + interval '1 day');

INSERT INTO pin_glide.identity_refresh_operations
    (sender_id, operation_id, attempt, parent_digest, expires_at, committed_at)
VALUES
    (${sender}, ${sqlStringLiteral(`${marker}-operation-old`, 'operation')}, 0,
        ${sqlStringLiteral(retentionDigest(plan, 'operation-old-parent'), 'operation digest')},
        ${reference} - interval '8 days', ${reference} - interval '9 days'),
    (${sender}, ${sqlStringLiteral(`${marker}-operation-fresh`, 'operation')}, 0,
        ${sqlStringLiteral(retentionDigest(plan, 'operation-fresh-parent'), 'operation digest')},
        ${reference} + interval '1 day', ${reference});

INSERT INTO pin_glide.pairing_sessions
    (id, mode, receiver_id, authorized_sender_id, state, token_digest, created_at,
        expires_at, closed_at)
VALUES
    (${sqlStringLiteral(`${marker}-session-old`, 'Pairing Session')}, 'new_receiver', ${receiver},
        ${sender}, 'expired', ${sqlStringLiteral(retentionDigest(plan, 'session-old'), 'session digest')},
        ${reference} - interval '9 days', ${reference} - interval '8 days',
        ${reference} - interval '8 days'),
    (${sqlStringLiteral(`${marker}-session-fresh`, 'Pairing Session')}, 'new_receiver', ${receiver},
        ${sender}, 'expired', ${sqlStringLiteral(retentionDigest(plan, 'session-fresh'), 'session digest')},
        ${reference}, ${reference} + interval '1 day', NULL);

INSERT INTO pin_glide.service_diagnostics
    (occurred_at, request_id, kind, method, code, duration_ms)
VALUES
    (${reference} - interval '31 days', ${sqlStringLiteral(`${marker}-diagnostic-old`, 'diagnostic')},
        'http', 'GET', 200, 1),
    (${reference} - interval '29 days', ${sqlStringLiteral(`${marker}-diagnostic-fresh`, 'diagnostic')},
        'http', 'GET', 200, 1);

INSERT INTO release.release_schema_audits
    (target_schema, operation, stdout, stderr, exit_code, executed_at, duration_ms, executed_by)
VALUES
    ('pin_glide', 'check', ${sqlStringLiteral(`${marker}-audit-old`, 'audit output')}, '', 0,
        ${reference} - interval '31 days', 1, ${sqlStringLiteral(marker, 'audit owner')}),
    ('pin_glide', 'check', ${sqlStringLiteral(`${marker}-audit-fresh`, 'audit output')}, '', 0,
        ${reference} - interval '29 days', 1, ${sqlStringLiteral(marker, 'audit owner')});

INSERT INTO cron.job_run_details
    (jobid, database, username, command, status, start_time, end_time)
VALUES
    (${job}, current_database(), current_user, ${sqlStringLiteral(`${marker}-cron-old`, 'cron command')},
        'succeeded', ${reference} - interval '31 days', ${reference} - interval '31 days'),
    (${job}, current_database(), current_user, ${sqlStringLiteral(`${marker}-cron-fresh`, 'cron command')},
        'succeeded', ${reference} - interval '29 days', ${reference} - interval '29 days'),
    (${job}, current_database(), current_user, ${sqlStringLiteral(`${marker}-cron-running`, 'cron command')},
        'running', ${reference} - interval '31 days', NULL);
`);
}

export function renderRetentionBoundarySQL(plan, {
  referenceTime,
  senderID,
  receiverID,
  pairingID,
  jobID,
  liveTokenDigest,
  liveGuardDigest,
}) {
  const reference = timestamp(referenceTime);
  const sender = sqlStringLiteral(senderID, 'Sender');
  const receiver = sqlStringLiteral(receiverID, 'Receiver');
  const pairing = sqlStringLiteral(pairingID, 'Pairing');
  const job = positiveInteger(jobID, 'cron job ID');
  const marker = retentionMarker(plan);
  const liveDigest = sqlStringLiteral(liveTokenDigest, 'live token digest');
  const liveGuard = sqlStringLiteral(liveGuardDigest, 'live guard digest');
  return guardedTransaction(`
DELETE FROM release.release_schema_audits
WHERE executed_by = ${sqlStringLiteral(marker, 'audit owner')}
  AND stdout LIKE ${escapeSQLString(`${marker}-audit-%cutoff%`)};
DELETE FROM cron.job_run_details
WHERE command LIKE ${escapeSQLString(`${marker}-cron-%cutoff%`)};
DELETE FROM pin_glide.service_diagnostics
WHERE request_id LIKE ${escapeSQLString(`${marker}-diagnostic-%cutoff%`)};
DELETE FROM pin_glide.pairing_sessions
WHERE id LIKE ${escapeSQLString(`${marker}-session-%cutoff%`)};
DELETE FROM pin_glide.identity_refresh_operations
WHERE operation_id LIKE ${escapeSQLString(`${marker}-operation-%cutoff%`)};
DELETE FROM pin_glide.identity_credentials
WHERE family_id = ${sqlStringLiteral(`${marker}-live`, 'live family')}
   OR token_digest IN (
       ${sqlStringLiteral(retentionDigest(plan, 'credential-cutoff'), 'credential digest')},
       ${sqlStringLiteral(retentionDigest(plan, 'credential-past-cutoff'), 'credential digest')}
   );
DELETE FROM pin_glide.delivery_deliveries
WHERE message_id LIKE ${escapeSQLString(`${marker}-delivery-%cutoff%`)};

INSERT INTO pin_glide.delivery_deliveries
    (id, sender_id, message_id, receiver_id, pairing_id, status, created_at, completed_at)
VALUES
    (${sqlStringLiteral(`${marker}-delivery-cutoff`, 'Delivery ID')}, ${sender},
        ${sqlStringLiteral(`${marker}-delivery-cutoff`, 'Message')}, ${receiver}, ${pairing}, 'offline',
        ${reference} - interval '7 days', ${reference} - interval '7 days'),
    (${sqlStringLiteral(`${marker}-delivery-past-cutoff`, 'Delivery ID')}, ${sender},
        ${sqlStringLiteral(`${marker}-delivery-past-cutoff`, 'Message')}, ${receiver}, ${pairing}, 'offline',
        ${reference} - interval '7 days' - interval '1 second',
        ${reference} - interval '7 days' - interval '1 second');

INSERT INTO pin_glide.identity_credentials
    (sender_id, family_id, scope, token_digest, issued_at, expires_at)
VALUES
    (${sender}, ${sqlStringLiteral(`${marker}-credential-cutoff`, 'credential family')}, 'refresh',
        ${sqlStringLiteral(retentionDigest(plan, 'credential-cutoff'), 'credential digest')},
        ${reference} - interval '7 days', ${reference} - interval '7 days'),
    (${sender}, ${sqlStringLiteral(`${marker}-credential-past-cutoff`, 'credential family')}, 'refresh',
        ${sqlStringLiteral(retentionDigest(plan, 'credential-past-cutoff'), 'credential digest')},
        ${reference} - interval '7 days' - interval '1 second',
        ${reference} - interval '7 days' - interval '1 second'),
    (${sender}, ${sqlStringLiteral(`${marker}-live`, 'live family')}, 'refresh',
        ${liveDigest}, ${reference}, ${reference} + interval '1 day');
INSERT INTO pin_glide.identity_credentials
    (sender_id, family_id, scope, token_digest, issued_at, expires_at)
VALUES
    (${sender}, ${sqlStringLiteral(`${marker}-live-guard`, 'live guard family')}, 'refresh',
        ${liveGuard}, ${reference}, ${reference} + interval '1 day');

INSERT INTO pin_glide.identity_refresh_operations
    (sender_id, operation_id, attempt, parent_digest, expires_at, committed_at)
VALUES
    (${sender}, ${sqlStringLiteral(`${marker}-operation-cutoff`, 'operation')}, 0,
        ${sqlStringLiteral(retentionDigest(plan, 'operation-cutoff-parent'), 'operation digest')},
        ${reference} - interval '7 days', ${reference} - interval '8 days'),
    (${sender}, ${sqlStringLiteral(`${marker}-operation-past-cutoff`, 'operation')}, 0,
        ${sqlStringLiteral(retentionDigest(plan, 'operation-past-cutoff-parent'), 'operation digest')},
        ${reference} - interval '7 days' - interval '1 second', ${reference} - interval '8 days'),
    (${sender}, ${sqlStringLiteral(`${marker}-operation-live`, 'operation')}, 0,
        ${liveGuard}, ${reference} + interval '1 day', ${reference});

INSERT INTO pin_glide.pairing_sessions
    (id, mode, receiver_id, authorized_sender_id, state, token_digest, created_at,
        expires_at, closed_at)
VALUES
    (${sqlStringLiteral(`${marker}-session-cutoff`, 'Pairing Session')}, 'new_receiver', ${receiver},
        ${sender}, 'expired', ${sqlStringLiteral(retentionDigest(plan, 'session-cutoff'), 'session digest')},
        ${reference} - interval '8 days', ${reference} - interval '7 days',
        ${reference} - interval '7 days'),
    (${sqlStringLiteral(`${marker}-session-past-cutoff`, 'Pairing Session')}, 'new_receiver', ${receiver},
        ${sender}, 'expired',
        ${sqlStringLiteral(retentionDigest(plan, 'session-past-cutoff'), 'session digest')},
        ${reference} - interval '8 days', ${reference} - interval '7 days' - interval '1 second',
        ${reference} - interval '7 days' - interval '1 second');

INSERT INTO pin_glide.service_diagnostics
    (occurred_at, request_id, kind, method, code, duration_ms)
VALUES
    (${reference} - interval '30 days',
        ${sqlStringLiteral(`${marker}-diagnostic-cutoff`, 'diagnostic')}, 'http', 'GET', 200, 1),
    (${reference} - interval '30 days' - interval '1 second',
        ${sqlStringLiteral(`${marker}-diagnostic-past-cutoff`, 'diagnostic')}, 'http', 'GET', 200, 1);

INSERT INTO release.release_schema_audits
    (target_schema, operation, stdout, stderr, exit_code, executed_at, duration_ms, executed_by)
VALUES
    ('pin_glide', 'check', ${sqlStringLiteral(`${marker}-audit-cutoff`, 'audit output')}, '', 0,
        ${reference} - interval '30 days', 1, ${sqlStringLiteral(marker, 'audit owner')}),
    ('pin_glide', 'check', ${sqlStringLiteral(`${marker}-audit-past-cutoff`, 'audit output')}, '', 0,
        ${reference} - interval '30 days' - interval '1 second', 1,
        ${sqlStringLiteral(marker, 'audit owner')});

INSERT INTO cron.job_run_details
    (jobid, database, username, command, status, start_time, end_time)
VALUES
    (${job}, current_database(), current_user,
        ${sqlStringLiteral(`${marker}-cron-cutoff`, 'cron command')}, 'succeeded',
        ${reference} - interval '30 days', ${reference} - interval '30 days'),
    (${job}, current_database(), current_user,
        ${sqlStringLiteral(`${marker}-cron-past-cutoff`, 'cron command')}, 'succeeded',
        ${reference} - interval '30 days' - interval '1 second',
        ${reference} - interval '30 days' - interval '1 second');
`);
}
