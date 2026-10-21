import assert from 'node:assert/strict';
import test from 'node:test';

import { createFixturePlan } from './lib/fixtures.mjs';
import { upload } from './lib/session_delivery_support.mjs';
import {
  runScheduledCleanupProbe,
  jsonFromOutput,
  stableRetentionJob,
  unknownWriterBlocksSnapshot,
} from './lib/session_retention.mjs';
import { cronCommand } from './lib/database.mjs';
import { verifyDiagnosticRedaction } from './lib/session_diagnostics.mjs';

const referenceTime = '2026-10-21T04:00:00.000Z';

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

test('privacy verification records large Delivery payloads for redaction scans', async () => {
  const nonce = 'AAAAAAAAAAAAAAAA';
  const ciphertext = Buffer.alloc(700 * 1024, 7).toString('base64');
  const secrets = [];
  let request;
  const session = {
    addSecret(values) {
      secrets.push(...values);
    },
    token() {
      return 'uploader-token';
    },
    async request(options) {
      request = options;
      return { status: 200, body: { results: [] } };
    },
  };

  await upload(session, 'pair2', 'large-message', [{
    receiverId: 'receiver-1',
    pairingId: 'pairing-1',
    cryptoVersion: 1,
    nonce,
    ciphertext,
  }]);

  assert.deepEqual(secrets, [nonce, ciphertext]);
  assert.equal(request.body.items[0].ciphertext, ciphertext);
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
