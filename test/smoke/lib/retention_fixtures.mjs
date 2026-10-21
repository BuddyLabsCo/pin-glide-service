import { createHash } from 'node:crypto';

import { escapeSQLString, sqlTextArray } from './fixtures.mjs';

export function retentionMarker(plan) {
  return `smoke-${plan.runId}-retention`;
}

function markerName(plan, suffix) {
  return `${retentionMarker(plan)}-${suffix}`;
}

export function retentionDigest(plan, suffix) {
  return createHash('sha256')
    .update(`${retentionMarker(plan)}:${suffix}`)
    .digest('hex');
}

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

export function renderRetentionAgingSQL(plan, {
  referenceTime,
  senderID,
  receiverID,
  pairingID,
  jobID,
}) {
  const reference = timestamp(referenceTime);
  const sender = identifier(senderID, 'Sender');
  const receiver = identifier(receiverID, 'Receiver');
  const pairing = identifier(pairingID, 'Pairing');
  const job = positiveInteger(jobID, 'cron job ID');
  const marker = retentionMarker(plan);
  return guardedTransaction(`
${deleteOwnedBody(plan)}

INSERT INTO pin_glide.delivery_deliveries
    (id, sender_id, message_id, receiver_id, pairing_id, status, created_at, completed_at)
VALUES
    (${identifier(`${marker}-delivery-old`, 'Delivery ID')}, ${sender},
        ${identifier(`${marker}-delivery-old`, 'Message')}, ${receiver}, ${pairing}, 'offline',
        ${reference} - interval '8 days', ${reference} - interval '8 days'),
    (${identifier(`${marker}-delivery-fresh`, 'Delivery ID')}, ${sender},
        ${identifier(`${marker}-delivery-fresh`, 'Message')}, ${receiver}, ${pairing}, 'offline',
        ${reference} - interval '6 days', ${reference} - interval '6 days');

INSERT INTO pin_glide.identity_credentials
    (sender_id, family_id, scope, token_digest, issued_at, expires_at)
VALUES
    (${sender}, ${identifier(`${marker}-credential-old`, 'credential family')}, 'refresh',
        ${identifier(retentionDigest(plan, 'credential-old'), 'credential digest')},
        ${reference} - interval '8 days', ${reference} - interval '8 days'),
    (${sender}, ${identifier(`${marker}-credential-fresh`, 'credential family')}, 'refresh',
        ${identifier(retentionDigest(plan, 'credential-fresh'), 'credential digest')},
        ${reference}, ${reference} + interval '1 day');

INSERT INTO pin_glide.identity_refresh_operations
    (sender_id, operation_id, attempt, parent_digest, expires_at, committed_at)
VALUES
    (${sender}, ${identifier(`${marker}-operation-old`, 'operation')}, 0,
        ${identifier(retentionDigest(plan, 'operation-old-parent'), 'operation digest')},
        ${reference} - interval '8 days', ${reference} - interval '9 days'),
    (${sender}, ${identifier(`${marker}-operation-fresh`, 'operation')}, 0,
        ${identifier(retentionDigest(plan, 'operation-fresh-parent'), 'operation digest')},
        ${reference} + interval '1 day', ${reference});

INSERT INTO pin_glide.pairing_sessions
    (id, mode, receiver_id, authorized_sender_id, state, token_digest, created_at,
        expires_at, closed_at)
VALUES
    (${identifier(`${marker}-session-old`, 'Pairing Session')}, 'new_receiver', ${receiver},
        ${sender}, 'expired', ${identifier(retentionDigest(plan, 'session-old'), 'session digest')},
        ${reference} - interval '9 days', ${reference} - interval '8 days',
        ${reference} - interval '8 days'),
    (${identifier(`${marker}-session-fresh`, 'Pairing Session')}, 'new_receiver', ${receiver},
        ${sender}, 'expired', ${identifier(retentionDigest(plan, 'session-fresh'), 'session digest')},
        ${reference}, ${reference} + interval '1 day', NULL);

INSERT INTO pin_glide.service_diagnostics
    (occurred_at, request_id, kind, method, code, duration_ms)
VALUES
    (${reference} - interval '31 days', ${identifier(`${marker}-diagnostic-old`, 'diagnostic')},
        'http', 'GET', 200, 1),
    (${reference} - interval '29 days', ${identifier(`${marker}-diagnostic-fresh`, 'diagnostic')},
        'http', 'GET', 200, 1);

INSERT INTO release.release_schema_audits
    (target_schema, operation, stdout, stderr, exit_code, executed_at, duration_ms, executed_by)
VALUES
    ('pin_glide', 'check', ${identifier(`${marker}-audit-old`, 'audit output')}, '', 0,
        ${reference} - interval '31 days', 1, ${identifier(marker, 'audit owner')}),
    ('pin_glide', 'check', ${identifier(`${marker}-audit-fresh`, 'audit output')}, '', 0,
        ${reference} - interval '29 days', 1, ${identifier(marker, 'audit owner')});

INSERT INTO cron.job_run_details
    (jobid, database, username, command, status, start_time, end_time)
VALUES
    (${job}, current_database(), current_user, ${identifier(`${marker}-cron-old`, 'cron command')},
        'succeeded', ${reference} - interval '31 days', ${reference} - interval '31 days'),
    (${job}, current_database(), current_user, ${identifier(`${marker}-cron-fresh`, 'cron command')},
        'succeeded', ${reference} - interval '29 days', ${reference} - interval '29 days'),
    (${job}, current_database(), current_user, ${identifier(`${marker}-cron-running`, 'cron command')},
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
}) {
  const reference = timestamp(referenceTime);
  const sender = identifier(senderID, 'Sender');
  const receiver = identifier(receiverID, 'Receiver');
  const pairing = identifier(pairingID, 'Pairing');
  const job = positiveInteger(jobID, 'cron job ID');
  const marker = retentionMarker(plan);
  const liveDigest = identifier(liveTokenDigest, 'live token digest');
  return guardedTransaction(`
DELETE FROM release.release_schema_audits
WHERE executed_by = ${identifier(marker, 'audit owner')}
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
WHERE family_id = ${identifier(`${marker}-live`, 'live family')}
   OR token_digest IN (
       ${identifier(retentionDigest(plan, 'credential-cutoff'), 'credential digest')},
       ${identifier(retentionDigest(plan, 'credential-past-cutoff'), 'credential digest')}
   );
DELETE FROM pin_glide.delivery_deliveries
WHERE message_id LIKE ${escapeSQLString(`${marker}-delivery-%cutoff%`)};

INSERT INTO pin_glide.delivery_deliveries
    (id, sender_id, message_id, receiver_id, pairing_id, status, created_at, completed_at)
VALUES
    (${identifier(`${marker}-delivery-cutoff`, 'Delivery ID')}, ${sender},
        ${identifier(`${marker}-delivery-cutoff`, 'Message')}, ${receiver}, ${pairing}, 'offline',
        ${reference} - interval '7 days', ${reference} - interval '7 days'),
    (${identifier(`${marker}-delivery-past-cutoff`, 'Delivery ID')}, ${sender},
        ${identifier(`${marker}-delivery-past-cutoff`, 'Message')}, ${receiver}, ${pairing}, 'offline',
        ${reference} - interval '7 days' - interval '1 second',
        ${reference} - interval '7 days' - interval '1 second');

INSERT INTO pin_glide.identity_credentials
    (sender_id, family_id, scope, token_digest, issued_at, expires_at)
VALUES
    (${sender}, ${identifier(`${marker}-credential-cutoff`, 'credential family')}, 'refresh',
        ${identifier(retentionDigest(plan, 'credential-cutoff'), 'credential digest')},
        ${reference} - interval '7 days', ${reference} - interval '7 days'),
    (${sender}, ${identifier(`${marker}-credential-past-cutoff`, 'credential family')}, 'refresh',
        ${identifier(retentionDigest(plan, 'credential-past-cutoff'), 'credential digest')},
        ${reference} - interval '7 days' - interval '1 second',
        ${reference} - interval '7 days' - interval '1 second'),
    (${sender}, ${identifier(`${marker}-live`, 'live family')}, 'refresh',
        ${liveDigest}, ${reference}, ${reference} + interval '1 day');

INSERT INTO pin_glide.identity_refresh_operations
    (sender_id, operation_id, attempt, parent_digest, expires_at, committed_at)
VALUES
    (${sender}, ${identifier(`${marker}-operation-cutoff`, 'operation')}, 0,
        ${identifier(retentionDigest(plan, 'operation-cutoff-parent'), 'operation digest')},
        ${reference} - interval '7 days', ${reference} - interval '8 days'),
    (${sender}, ${identifier(`${marker}-operation-past-cutoff`, 'operation')}, 0,
        ${identifier(retentionDigest(plan, 'operation-past-cutoff-parent'), 'operation digest')},
        ${reference} - interval '7 days' - interval '1 second', ${reference} - interval '8 days'),
    (${sender}, ${identifier(`${marker}-operation-live`, 'operation')}, 0,
        ${liveDigest}, ${reference} + interval '1 day', ${reference});

INSERT INTO pin_glide.pairing_sessions
    (id, mode, receiver_id, authorized_sender_id, state, token_digest, created_at,
        expires_at, closed_at)
VALUES
    (${identifier(`${marker}-session-cutoff`, 'Pairing Session')}, 'new_receiver', ${receiver},
        ${sender}, 'expired', ${identifier(retentionDigest(plan, 'session-cutoff'), 'session digest')},
        ${reference} - interval '8 days', ${reference} - interval '7 days',
        ${reference} - interval '7 days'),
    (${identifier(`${marker}-session-past-cutoff`, 'Pairing Session')}, 'new_receiver', ${receiver},
        ${sender}, 'expired',
        ${identifier(retentionDigest(plan, 'session-past-cutoff'), 'session digest')},
        ${reference} - interval '8 days', ${reference} - interval '7 days' - interval '1 second',
        ${reference} - interval '7 days' - interval '1 second');

INSERT INTO pin_glide.service_diagnostics
    (occurred_at, request_id, kind, method, code, duration_ms)
VALUES
    (${reference} - interval '30 days',
        ${identifier(`${marker}-diagnostic-cutoff`, 'diagnostic')}, 'http', 'GET', 200, 1),
    (${reference} - interval '30 days' - interval '1 second',
        ${identifier(`${marker}-diagnostic-past-cutoff`, 'diagnostic')}, 'http', 'GET', 200, 1);

INSERT INTO release.release_schema_audits
    (target_schema, operation, stdout, stderr, exit_code, executed_at, duration_ms, executed_by)
VALUES
    ('pin_glide', 'check', ${identifier(`${marker}-audit-cutoff`, 'audit output')}, '', 0,
        ${reference} - interval '30 days', 1, ${identifier(marker, 'audit owner')}),
    ('pin_glide', 'check', ${identifier(`${marker}-audit-past-cutoff`, 'audit output')}, '', 0,
        ${reference} - interval '30 days' - interval '1 second', 1,
        ${identifier(marker, 'audit owner')});

INSERT INTO cron.job_run_details
    (jobid, database, username, command, status, start_time, end_time)
VALUES
    (${job}, current_database(), current_user,
        ${identifier(`${marker}-cron-cutoff`, 'cron command')}, 'succeeded',
        ${reference} - interval '30 days', ${reference} - interval '30 days'),
    (${job}, current_database(), current_user,
        ${identifier(`${marker}-cron-past-cutoff`, 'cron command')}, 'succeeded',
        ${reference} - interval '30 days' - interval '1 second',
        ${reference} - interval '30 days' - interval '1 second');
`);
}
