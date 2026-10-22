import assert from 'node:assert/strict';
import test from 'node:test';

import { modelConfiguration } from './configuration.mjs';
import {
  cronCommand,
  cronJobs,
  retention,
  retentionJobName,
  validateDatabaseFacts,
} from './database.mjs';
import { SmokeValidationError } from '../support/errors.mjs';
import {
  parseReleaseSupport,
  publishedUpgradeOrigin,
  releaseSupport,
  scriptDigests,
  validatePublishedRelease,
} from './release.mjs';
import { TARGET } from '../harness/target.mjs';

test('database facts require the selected release, approved model, complete ledgers and scheduled cleanup state', () => {
  const expectedJob = cronJobs.get(retentionJobName);
  const checksum = 'b'.repeat(64);
  const model = {
    id: 'default',
    workspaceId: 'ws-kea2u5v9va6wsdvh',
    model: 'qwen3.7-flash',
    enabled: true,
  };
  const facts = {
    database: 'pin_glide',
    release: '1.2.3',
    history: ['1.2.2', '1.2.3'],
    model: {
      id: 'default',
      workspaceId: 'ws-kea2u5v9va6wsdvh',
      model: 'qwen3.7-flash',
      enabled: true,
    },
    retention: [
      { schema: 'pin_glide', table: 'delivery_deliveries', days: 7, column: 'created_at' },
      { schema: 'pin_glide', table: 'identity_credentials', days: 7, column: 'expires_at' },
      { schema: 'pin_glide', table: 'identity_refresh_operations', days: 7, column: 'expires_at' },
      { schema: 'pin_glide', table: 'pairing_sessions', days: 7, column: 'expires_at' },
      { schema: 'pin_glide', table: 'service_diagnostics', days: 30, column: 'occurred_at' },
      { schema: 'release', table: 'release_schema_audits', days: 30, column: 'executed_at' },
      { schema: 'cron', table: 'job_run_details', days: 30, column: 'end_time' },
    ],
    cron: [{
      jobname: retentionJobName,
      username: expectedJob.username,
      database: expectedJob.database,
      schedule: expectedJob.schedule,
      active: true,
      command: cronCommand,
    }],
    cronTimezone: 'GMT',
    audits: {
      'release:apply': 0,
      'release:check': 0,
      'pin_glide:apply': 0,
      'pin_glide:check': 0,
    },
    dataScripts: [{ filename: 'issue_33_1_model_configuration.sql', checksum }],
    migrationScripts: [{ schema: 'pin_glide', filename: 'issue_77_1.sql', checksum }],
  };
  const expectedScripts = {
    data: { 'issue_33_1_model_configuration.sql': checksum },
    migrations: { 'pin_glide/issue_77_1.sql': checksum },
  };
  const origins = {
    '1.1.0': [],
    '1.2.2': ['1.1.0'],
    '1.2.3': ['1.2.2'],
  };
  const summary = validateDatabaseFacts(facts, {
    release: '1.2.3',
    scripts: expectedScripts,
    origins,
    upgradeOrigin: '1.2.2',
    model,
  });
  assert.equal(summary.release, '1.2.3');
  assert.equal(summary.model.workspaceId, 'ws-kea2u5v9va6wsdvh');

  const wrongTimezone = structuredClone(facts);
  wrongTimezone.cronTimezone = 'Asia/Shanghai';
  assert.throws(() => validateDatabaseFacts(wrongTimezone, {
    release: '1.2.3',
    scripts: expectedScripts,
    origins,
    upgradeOrigin: '1.2.2',
    model,
  }), /timezone.*not UTC/);

  const wrongModel = structuredClone(facts);
  wrongModel.model.model = 'another-model';
  assert.throws(() => validateDatabaseFacts(wrongModel, {
    release: '1.2.3',
    scripts: expectedScripts,
    origins,
    upgradeOrigin: '1.2.2',
    model,
  }), /release Data Script/);

  const staleAudit = structuredClone(facts);
  staleAudit.audits['pin_glide:check'] = 1;
  assert.throws(() => validateDatabaseFacts(staleAudit, {
    release: '1.2.3',
    scripts: expectedScripts,
    origins,
    upgradeOrigin: '1.2.2',
    model,
  }), /schema check/);

  const incomplete = structuredClone(facts);
  incomplete.dataScripts = [];
  assert.throws(() => validateDatabaseFacts(incomplete, {
    release: '1.2.3',
    scripts: expectedScripts,
    origins,
    upgradeOrigin: '1.2.2',
    model,
  }), /data script.*missing/i);

  const unsupported = structuredClone(facts);
  unsupported.history = ['1.1.0', '1.2.3'];
  assert.throws(() => validateDatabaseFacts(unsupported, {
    release: '1.2.3',
    scripts: expectedScripts,
    origins,
    upgradeOrigin: '1.2.2',
    model,
  }), /origin 1\.1\.0 is not supported/);

  const unsupportedChain = structuredClone(facts);
  unsupportedChain.history = ['1.1.0', '1.2.2', '1.2.3'];
  assert.throws(() => validateDatabaseFacts(unsupportedChain, {
    release: '1.2.3',
    scripts: expectedScripts,
    origins: {
      '1.2.2': ['1.2.1'],
      '1.2.3': ['1.2.2'],
    },
    upgradeOrigin: '1.2.2',
    model,
  }), /transition 1\.1\.0 to 1\.2\.2 is not declared/);

  const sameVersionRetry = structuredClone(facts);
  sameVersionRetry.history = ['1.2.2', '1.2.3', '1.2.3'];
  assert.equal(validateDatabaseFacts(sameVersionRetry, {
    release: '1.2.3',
    scripts: expectedScripts,
    origins,
    upgradeOrigin: '1.2.2',
    model,
  }).release, '1.2.3');

  const fresh = structuredClone(facts);
  fresh.history = ['1.2.3'];
  assert.equal(validateDatabaseFacts(fresh, {
    release: '1.2.3',
    scripts: expectedScripts,
    origins,
    upgradeOrigin: '1.2.2',
    model,
  }).installation, 'fresh');

  const unsupportedBefore = structuredClone(facts);
  unsupportedBefore.history = ['1.2.2', '1.2.3'];
  assert.throws(() => validateDatabaseFacts(unsupportedBefore, {
    release: '1.2.3',
    scripts: expectedScripts,
    origins,
    upgradeOrigin: null,
    model,
  }), /expected fresh installation/);

  const legacyCronShape = structuredClone(facts);
  legacyCronShape.cron = legacyCronShape.cron[0];
  assert.throws(() => validateDatabaseFacts(legacyCronShape, {
    release: '1.2.3',
    scripts: expectedScripts,
    origins,
    upgradeOrigin: '1.2.2',
    model,
  }), /not an array/);
});

test('the approved model configuration comes from the release Data Script', async () => {
  assert.deepEqual(await modelConfiguration(process.cwd()), {
    id: 'default',
    workspaceId: 'ws-kea2u5v9va6wsdvh',
    model: 'qwen3.7-flash',
    enabled: true,
  });
});

test('published upgrade origin is the newest stable release before the target', async () => {
  const origin = await publishedUpgradeOrigin({
    version: '1.2.3',
    releaseRepo: 'BuddyLabsCo/pin-glide-service',
    allowedOrigins: ['1.2.2', '1.2.1'],
    run: async () => ({
      stdout: JSON.stringify([
        { tagName: 'v1.2.4', isDraft: false, isPrerelease: false },
        { tagName: 'v1.2.2', isDraft: false, isPrerelease: false },
        { tagName: 'v1.2.1', isDraft: false, isPrerelease: false },
        { tagName: 'v1.1.9', isDraft: true, isPrerelease: false },
        { tagName: 'v1.1.8', isDraft: false, isPrerelease: true },
      ]),
    }),
  });
  assert.equal(origin, '1.2.2');

  const declared = await publishedUpgradeOrigin({
    version: '1.2.3',
    releaseRepo: 'BuddyLabsCo/pin-glide-service',
    allowedOrigins: ['1.2.1'],
    run: async () => ({
      stdout: JSON.stringify([
        { tagName: 'v1.2.2', isDraft: false, isPrerelease: false },
        { tagName: 'v1.2.1', isDraft: false, isPrerelease: false },
      ]),
    }),
  });
  assert.equal(declared, '1.2.1');
});

test('release support declares upgrade origins explicitly', async () => {
  const support = await releaseSupport(process.cwd());
  assert.deepEqual(support.origins['0.1.5'], []);
  assert.deepEqual(support.origins['0.1.6'], ['0.1.5']);
});

test('release support rejects malformed or invented upgrade origins', () => {
  assert.throws(() => parseReleaseSupport({ origins: { '1.2.3': ['1.2.2', '1.2.2'] } }), /duplicated/);
  assert.throws(() => parseReleaseSupport({ origins: { '1.2.3': ['1.3.0'] } }), /earlier/);
  assert.throws(() => parseReleaseSupport({ origins: [] }), /origins object/);
});

test('published release validation refuses an undeclared target version', async () => {
  const head = 'a'.repeat(40);
  await assert.rejects(() => validatePublishedRelease({
    target: { ...TARGET, release: '1.2.3' },
    root: process.cwd(),
    runContracts: false,
    run: async (command, args) => {
      if (command === 'git' && args[0] === 'rev-parse') return { stdout: `${head}\n`, stderr: '' };
      if (command === 'git' && args[0] === 'status') return { stdout: '', stderr: '' };
      if (command === 'gh' && args[0] === 'api') return { stdout: `${head}\n`, stderr: '' };
      if (command === 'gh' && args[0] === 'release') {
        return {
          stdout: JSON.stringify({
            tagName: 'v1.2.3',
            isDraft: false,
            isPrerelease: false,
            assets: [],
          }),
          stderr: '',
        };
      }
      return { stdout: '', stderr: '' };
    },
  }), /no declared upgrade support/);
});

test('SQL digests retain the immutable packaged order manifest', async () => {
  const scripts = await scriptDigests(process.cwd());
  assert(scripts.manifest.length > 0);
  assert.equal(new Set(scripts.manifest).size, scripts.manifest.length);
  assert.equal(
    Object.keys(scripts.data).length + Object.keys(scripts.migrations).length,
    scripts.manifest.length,
  );
  assert(scripts.manifest.includes('db/scripts/issue_33_1_model_configuration.sql'));
});

test('malformed release payloads fail validation while command failures stay blocked infrastructure', async () => {
  await assert.rejects(
    () => publishedUpgradeOrigin({
      version: '1.2.3',
      releaseRepo: 'BuddyLabsCo/pin-glide-service',
      allowedOrigins: ['1.2.2'],
      run: async () => ({ stdout: 'not-json' }),
    }),
    error => error instanceof SmokeValidationError && /not valid JSON/.test(error.message),
  );
  await assert.rejects(
    () => publishedUpgradeOrigin({
      version: '1.2.3',
      releaseRepo: 'BuddyLabsCo/pin-glide-service',
      allowedOrigins: ['1.2.2'],
      run: async () => { throw new Error('gh auth failed'); },
    }),
    error => !(error instanceof SmokeValidationError) && /gh auth failed/.test(error.message),
  );
});
test('published release checks refuse a dirty worktree before contacting the target', async () => {
  const calls = [];
  await assert.rejects(
    () => validatePublishedRelease({
      target: { ...TARGET, release: '0.0.0' },
      root: process.cwd(),
      runContracts: false,
      run: async (command, args) => {
        calls.push([command, ...args]);
        if (command === 'git' && args[0] === 'rev-parse') return { stdout: `${'a'.repeat(40)}\n`, stderr: '' };
        if (command === 'git' && args[0] === 'status') return { stdout: ' M changed\n', stderr: '' };
        throw new Error(`unexpected command ${command}`);
      },
    }),
    /source worktree must be clean/,
  );
  assert(!calls.some(([command]) => command === 'gh'));
});

test('published release checks accept test-harness commits on a clean worktree while binding to the released source', async () => {
  const head = 'a'.repeat(40);
  const releaseCommit = 'b'.repeat(40);
  const calls = [];
  const run = async (command, args) => {
    calls.push([command, ...args]);
    if (command === 'git' && args[0] === 'rev-parse') return { stdout: `${head}\n`, stderr: '' };
    if (command === 'git' && args[0] === 'status') return { stdout: '', stderr: '' };
    if (command === 'gh' && args[0] === 'api') return { stdout: `${releaseCommit}\n`, stderr: '' };
    if (command === 'gh' && args[0] === 'release' && args[1] === 'list') {
      return { stdout: JSON.stringify([{ tagName: 'v0.1.5', isDraft: false, isPrerelease: false }]), stderr: '' };
    }
    if (command === 'gh' && args[0] === 'release') {
      return {
        stdout: JSON.stringify({
          tagName: 'v0.1.6',
          isDraft: false,
          isPrerelease: false,
          assets: [],
        }),
        stderr: '',
      };
    }
    return { stdout: '', stderr: '' };
  };
  const validated = await validatePublishedRelease({
    target: { ...TARGET, release: '0.1.6' },
    root: process.cwd(),
    run,
    runContracts: false,
  });
  assert.equal(validated.sourceCommit, releaseCommit);
  assert.equal(validated.checkedOutCommit, head);
  assert.equal(validated.tag, 'v0.1.6');
  assert.equal(validated.upgradeOrigin, '0.1.5');
  assert(calls.some(call => call[0] === 'git' && call[1] === 'status'));
});
