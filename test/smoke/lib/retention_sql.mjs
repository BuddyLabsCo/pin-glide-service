import { createHash } from 'node:crypto';

import { escapeSQLString, retentionMarker } from './fixtures.mjs';

export { retentionMarker };

export function retentionDigest(plan, suffix) {
  return createHash('sha256')
    .update(`${retentionMarker(plan)}:${suffix}`)
    .digest('hex');
}

export function markerPattern(plan) {
  return escapeSQLString(`${retentionMarker(plan)}%`);
}

export function timestamp(value) {
  const parsed = new Date(value);
  if (!Number.isFinite(parsed.getTime())) throw new Error('Retention reference time is invalid');
  return `${escapeSQLString(parsed.toISOString())}::timestamptz`;
}

export function identifier(value, label) {
  if (typeof value !== 'string' || value === '') throw new Error(`Retention ${label} is missing`);
  return escapeSQLString(value);
}

export function positiveInteger(value, label) {
  if (!Number.isSafeInteger(value) || value <= 0) throw new Error(`Retention ${label} must be positive`);
  return String(value);
}

export function guardedTransaction(body, label = 'retention fixture', terminator = 'COMMIT') {
  if (!['COMMIT', 'ROLLBACK'].includes(terminator)) {
    throw new Error('Retention transaction terminator must be COMMIT or ROLLBACK');
  }
  const refusal = escapeSQLString(`refusing smoke ${label} outside pin_glide`);
  return `BEGIN;
SET LOCAL lock_timeout = '10s';
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION ${refusal};
    END IF;
END
$$;
${body.trim()}
${terminator};
`;
}

export function deleteOwnedBody(plan) {
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
