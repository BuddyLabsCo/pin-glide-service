import { withFailureJoiningCleanup } from '../support/cleanup.mjs';
import { escapeSQLString, sqlTextArray } from '../support/sql.mjs';
import { renderCleanupSQL, retentionMarker } from './fixtures.mjs';

export const cleanupVerificationCategories = Object.freeze([
  'accounts',
  'senders',
  'credentials',
  'refreshOperations',
  'receivers',
  'receiverCredentials',
  'pairings',
  'pairingSessions',
  'deliveries',
  'diagnostics',
  'schemaAudits',
  'cronJobs',
  'cronRuns',
]);

export function renderCleanupVerificationSQL(
  plan,
  receiverIDs = [],
  requestIds = [],
  subjects = [],
) {
  const accountIDs = sqlTextArray(plan.accounts.map(account => account.id));
  const senderIDs = sqlTextArray(plan.senders.map(sender => sender.id));
  const receivers = sqlTextArray(receiverIDs);
  const requests = sqlTextArray(requestIds);
  const ownedSubjects = sqlTextArray(subjects);
  const marker = escapeSQLString(`${retentionMarker(plan)}%`);
  return `
SELECT json_build_object(
  'accounts', (
    SELECT count(*) FROM pin_glide.identity_accounts WHERE id = ANY(${accountIDs})
  ),
  'senders', (
    SELECT count(*) FROM pin_glide.identity_sender_registrations WHERE id = ANY(${senderIDs})
  ),
  'credentials', (
    SELECT count(*) FROM pin_glide.identity_credentials
    WHERE sender_id = ANY(${senderIDs}) OR family_id LIKE ${marker}
  ),
  'refreshOperations', (
    SELECT count(*) FROM pin_glide.identity_refresh_operations
    WHERE sender_id = ANY(${senderIDs}) OR operation_id LIKE ${marker}
  ),
  'receivers', (
    SELECT count(*) FROM pin_glide.identity_receiver_registrations
    WHERE account_id = ANY(${accountIDs}) OR id = ANY(${receivers})
  ),
  'receiverCredentials', (
    SELECT count(*) FROM pin_glide.identity_receiver_credentials
    WHERE receiver_id = ANY(${receivers})
       OR receiver_id IN (
         SELECT id FROM pin_glide.identity_receiver_registrations
         WHERE account_id = ANY(${accountIDs})
       )
  ),
  'pairings', (
    SELECT count(*) FROM pin_glide.pairing_pairings
    WHERE sender_id = ANY(${senderIDs}) OR receiver_id = ANY(${receivers})
  ),
  'pairingSessions', (
    SELECT count(*) FROM pin_glide.pairing_sessions
    WHERE authorized_sender_id = ANY(${senderIDs})
       OR receiver_id = ANY(${receivers})
       OR id LIKE ${marker}
  ),
  'deliveries', (
    SELECT count(*) FROM pin_glide.delivery_deliveries
    WHERE sender_id = ANY(${senderIDs})
       OR receiver_id = ANY(${receivers})
       OR message_id LIKE ${marker}
  ),
  'diagnostics', (
    SELECT count(*) FROM pin_glide.service_diagnostics
    WHERE request_id = ANY(${requests})
       OR subject = ANY(${ownedSubjects})
       OR request_id LIKE ${marker}
  ),
  'schemaAudits', (
    SELECT count(*) FROM release.release_schema_audits
    WHERE executed_by LIKE ${marker} OR stdout LIKE ${marker} OR stderr LIKE ${marker}
  ),
  'cronJobs', (
    SELECT count(*) FROM cron.job WHERE jobname LIKE ${escapeSQLString(`smoke-${plan.runId}-retention-probe-%`)}
  ),
  'cronRuns', (
    SELECT count(*) FROM cron.job_run_details WHERE command LIKE ${marker}
  )
) AS cleanup;
`;
}

export async function cleanupSession(session) {
  await withFailureJoiningCleanup(
    () => session.retentionCleanup?.(session.plan, [...session.retentionRunIDs]),
    () => session.remote.sql(renderCleanupSQL(
      session.plan,
      [...session.receivers],
      session.requestIds,
      [...session.subjects],
    )),
    'Retention fixtures and the ordinary smoke fixture cleanup both failed',
  );
  const remaining = await session.remote.sqlJSON(renderCleanupVerificationSQL(
    session.plan,
    [...session.receivers],
    session.requestIds,
    [...session.subjects],
  ));
  const actualCategories = Object.keys(remaining).sort();
  const expectedCategories = [...cleanupVerificationCategories].sort();
  if (
    actualCategories.length !== expectedCategories.length
    || actualCategories.some((category, index) => category !== expectedCategories[index])
  ) {
    throw new Error(
      `Run-owned fixture cleanup returned an incomplete verification: ${actualCategories.join(', ')}`,
    );
  }
  const leftovers = Object.entries(remaining).filter(([, count]) => Number(count) !== 0);
  if (leftovers.length > 0) {
    throw new Error(`Run-owned fixture cleanup left rows: ${JSON.stringify(remaining)}`);
  }
  session.receivers.clear();
  session.subjects.clear();
  session.requestIds.length = 0;
  session.errorBodies.length = 0;
  session.retentionRunIDs.clear();
}
