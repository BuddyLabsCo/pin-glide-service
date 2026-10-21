import assert from 'node:assert/strict';
import test from 'node:test';

import { createFixturePlan } from './lib/fixtures.mjs';
import {
  cleanupCallSQL,
  renderConfigurationDeleteProbeSQL,
  renderRetentionCleanupSQL,
  renderRetentionInterruptedSQL,
  retentionStateQuery,
  runOwnedPrivacyQuery,
} from './lib/retention_checks.mjs';
import {
  renderRetentionAgingSQL,
  renderRetentionBoundarySQL,
  retentionMarker,
} from './lib/retention_fixtures.mjs';
import {
  retentionProbeCommand,
  runScheduledCleanupProbe,
  jsonFromOutput,
  stableRetentionJob,
  unknownWriterBlocksSnapshot,
} from './lib/session_retention.mjs';
import { cronCommand } from './lib/database.mjs';
import { verifyDiagnosticRedaction } from './lib/session_diagnostics.mjs';

const referenceTime = '2026-10-21T04:00:00.000Z';
const liveTokenDigest = 'a'.repeat(64);
const fixture = {
  referenceTime,
  senderID: 'smoke-fixture-sender-pair2',
  receiverID: 'smoke-fixture-receiver',
  pairingID: 'smoke-fixture-pairing',
  jobID: 17,
  liveTokenDigest,
};

test('retention fixtures are database guarded and use one run-owned marker', () => {
  const plan = createFixturePlan({ runId: 'retention-sql-run' });
  const marker = retentionMarker(plan);
  const aging = renderRetentionAgingSQL(plan, fixture);
  const boundary = renderRetentionBoundarySQL(plan, fixture);

  for (const statement of [aging, boundary]) {
    assert.match(statement, /current_database\(\) = 'pin_glide'/);
    assert.match(statement, /BEGIN;/);
    assert.match(statement, /COMMIT;/);
  }
  for (const suffix of [
    'delivery-old',
    'credential-old',
    'operation-old',
    'session-old',
    'diagnostic-old',
    'audit-old',
    'cron-old',
    'cron-running',
  ]) {
    assert.match(aging, new RegExp(`${marker}-${suffix}`));
  }
  for (const suffix of [
    'delivery-cutoff',
    'delivery-past-cutoff',
    'credential-cutoff',
    'credential-past-cutoff',
    'operation-cutoff',
    'operation-past-cutoff',
    'session-cutoff',
    'session-past-cutoff',
    'diagnostic-cutoff',
    'diagnostic-past-cutoff',
    'audit-cutoff',
    'audit-past-cutoff',
    'cron-cutoff',
    'cron-past-cutoff',
  ]) {
    assert.match(boundary, new RegExp(`${marker}-${suffix}`));
  }
  assert.match(boundary, new RegExp(liveTokenDigest));
  assert.doesNotMatch(boundary, /retention-live-token/);
});

test('retention cleanup removes only fixtures and cron runs owned by the run', () => {
  const plan = createFixturePlan({ runId: 'retention-cleanup-run' });
  const marker = retentionMarker(plan);
  const cleanup = renderRetentionCleanupSQL(plan, [41, 42]);
  const state = retentionStateQuery(plan, fixture);

  assert.match(cleanup, /current_database\(\) = 'pin_glide'/);
  assert.match(cleanup, /cron\.unschedule\(jobname\)/);
  assert.match(cleanup, /jobname LIKE/);
  assert.match(cleanup, /runid = ANY\(ARRAY\[41, 42\]::bigint\[\]\)/);
  assert.match(cleanup, new RegExp(`${marker}%`));
  assert.doesNotMatch(cleanup, /DELETE FROM pin_glide\.identity_accounts/);
  assert.doesNotMatch(cleanup, /DELETE FROM pin_glide\.pairing_pairings/);
  assert.match(state, /deliveryOld/);
  assert.match(state, /durable/);
  assert.match(state, /pin-glide-retention-cleanup/);
});

test('the privacy query reads every run-owned application, release and cron surface', () => {
  const plan = createFixturePlan({ runId: 'retention-privacy-run' });
  const query = runOwnedPrivacyQuery(plan, {
    receiverIDs: ['receiver-1'],
    requestIDs: ['request-1'],
    subjects: ['session-1'],
  });
  for (const table of [
    'identity_accounts',
    'identity_sender_registrations',
    'identity_credentials',
    'identity_refresh_operations',
    'identity_receiver_registrations',
    'identity_receiver_credentials',
    'pairing_pairings',
    'pairing_sessions',
    'delivery_deliveries',
    'service_diagnostics',
    'release_schema_audits',
    'job_run_details',
    'release_data_scripts',
    'release_deploy_history',
    'release_migration_scripts',
  ]) {
    assert.match(query, new RegExp(table));
  }
});

test('scheduled cleanup probe waits for a committed run and removes its probe job', async () => {
  const calls = [];
  let poll = 0;
  const session = {
    plan: createFixturePlan({ runId: 'retention-probe-run' }),
    retentionRunIDs: new Set(),
    remote: {
      async sqlJSON(statement) {
        calls.push(statement);
        if (statement.includes('maxRunID')) return { maxRunID: 10 };
        if (statement.includes('cron.schedule')) return { jobID: 23 };
        if (statement.includes('cron.job_run_details')) {
          poll++;
          return poll === 1
            ? { runs: [{ runID: 11, status: 'running', endTime: null }] }
            : { runs: [{ runID: 11, status: 'succeeded', endTime: referenceTime }] };
        }
        throw new Error(`unexpected query ${statement}`);
      },
      async sql(statement) {
        calls.push(statement);
        return { stdout: '', stderr: '' };
      },
    },
  };

  const result = await runScheduledCleanupProbe(session, {
    pollMs: 0,
    timeoutMs: 100,
    sleep: async () => {},
  });
  assert.equal(result.jobID, 23);
  assert.deepEqual(result.runIDs, [11]);
  assert.equal(result.status, 'succeeded');
  assert.equal(result.selfRemoving, true);
  assert.deepEqual([...session.retentionRunIDs], [11]);
  assert(calls.some(statement => statement.includes("'1 second'")));
  assert(calls.some(statement => statement.includes('DO $smoke$')));
  assert(calls.some(statement => statement.includes('cron.unschedule')));
});

test('the scheduled cleanup probe removes itself even if the runner exits', () => {
  const command = retentionProbeCommand('smoke-retention-probe');
  assert.match(command, /pin_glide_fn_cleanup_expired_rows\(now\(\)\)/);
  assert.match(command, /cron\.unschedule\('smoke-retention-probe'\)/);
  assert.throws(() => retentionProbeCommand('invalid name'), /job name is invalid/);
});

test('the live unknown-writer probe blocks and terminates only its own connection', async () => {
  let resolveHolder;
  let terminated = false;
  const statements = [];
  const remote = {
    compose: 'compose',
    ssh() {
      return new Promise(resolve => {
        resolveHolder = resolve;
      });
    },
    async sqlJSON() {
      return { connections: terminated ? 0 : 1 };
    },
    async assertNoExternalWriters() {
      if (!terminated) throw new Error('Another database writer is still connected');
    },
    async sql(statement) {
      statements.push(statement);
      terminated = true;
      resolveHolder({ stdout: '', stderr: '' });
    },
  };
  const result = await unknownWriterBlocksSnapshot(
    remote,
    'smoke-retention-writer-run-writer',
    async () => {},
  );
  assert.equal(result.blocked, true);
  assert.match(statements[0], /pg_terminate_backend/);
  assert.match(statements[0], new RegExp(result.application));
});

test('the deployed retention job identity is exact', async () => {
  const job = {
    jobID: 9,
    jobname: 'pin-glide-retention-cleanup',
    database: 'pin_glide',
    username: 'pin_glide',
    schedule: '0 19 * * *',
    command: cronCommand,
    active: true,
  };
  const session = { remote: { async sqlJSON() { return job; } } };
  assert.equal(await stableRetentionJob(session), job);
  await assert.rejects(
    () => stableRetentionJob({
      remote: { async sqlJSON() { return { ...job, active: false }; } },
    }),
    /does not match/,
  );
});

test('privacy verification scans diagnostics, errors, logs and public artifacts', async () => {
  const recorded = [];
  const session = {
    startedAt: referenceTime,
    requestIds: ['request-1'],
    subjects: new Set(),
    errorBodies: [{ requestId: 'request-1', detail: 'safe' }],
    secretValues: () => ['secret-canary'],
    remote: {
      async sqlJSON() {
        return [{ requestId: 'request-1', request_id: 'request-1', detail: 'safe' }];
      },
      async apiLogsSince() {
        return { stdout: 'safe log\n', stderr: '' };
      },
    },
    evidence: {
      record(entry) {
        recorded.push(entry);
      },
    },
  };

  const result = await verifyDiagnosticRedaction(session, {
    settleMs: 0,
    includeLogs: true,
    databaseRows: {
      accounts: [{ id: 'account-1' }],
      credentials: [{ token_digest: 'digest' }],
    },
    artifacts: {
      'openapi.yaml': 'examples: safe',
      'db/scripts/example.sql': 'SELECT 1;',
    },
  });
  assert.deepEqual(result, {
    records: 1,
    scannedRequestIds: 1,
    missingRequestIds: 0,
    logsScanned: true,
    artifactsScanned: 2,
    errorBodiesScanned: 1,
    databaseRowsScanned: 2,
  });

  session.remote.apiLogsSince = async () => ({ stdout: 'secret-canary', stderr: '' });
  await assert.rejects(
    () => verifyDiagnosticRedaction(session, {
      settleMs: 0,
      includeLogs: true,
      artifacts: {},
    }),
    /service logs/,
  );
});

test('cleanup calls and configuration deletion probe stay inside one explicit transaction', () => {
  const plan = createFixturePlan({ runId: 'retention-transaction-run' });
  const cleanup = cleanupCallSQL(referenceTime);
  const configuration = renderConfigurationDeleteProbeSQL(plan, fixture);

  assert.match(cleanup, /json_build_object\(\s*'removed'/);
  assert.match(cleanup, /pin_glide_fn_cleanup_expired_rows/);
  assert.match(configuration, /BEGIN;/);
  assert.match(configuration, /DELETE FROM pin_glide\.model_configurations/);
  assert.match(configuration, /ROLLBACK;/);
  assert.doesNotMatch(configuration, /COMMIT;/);
});

test('configuration probe output survives psql command tags', () => {
  assert.deepEqual(
    jsonFromOutput({
      stdout: 'BEGIN\nDELETE 1\n{"modelConfigurations":0}\nROLLBACK\n',
      stderr: '',
    }, 'configuration probe'),
    { modelConfigurations: 0 },
  );
});
