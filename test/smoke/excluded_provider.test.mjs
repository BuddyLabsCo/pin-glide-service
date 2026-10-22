import assert from 'node:assert/strict';
import test from 'node:test';

import { SmokeBlocked } from './lib/http.mjs';
import {
  excludedProviderTestReferences,
  runExcludedProviderEvidence,
} from './lib/excluded_provider.mjs';

test('excluded real-provider paths are reported only as deterministic evidence', async () => {
  const calls = [];
  const recorded = [];
  const session = {
    requestIds: [],
    evidence: {
      record(entry) {
        recorded.push(entry);
      },
    },
  };

  const outcome = await runExcludedProviderEvidence({
    session,
    root: '/repository',
    run: async (command, args) => {
      calls.push({ command, args });
      if (command === 'docker') return { stdout: '29.0.0' };
      const packagePath = args.at(-1);
      return {
        stdout: excludedProviderTestReferences
          .filter(reference => reference.package === packagePath)
          .map(reference => `--- PASS: ${reference.test} (0.00s)`)
          .join('\n'),
      };
    },
  });

  assert.equal(outcome.status, 'passed');
  assert.equal(recorded.length, 1);
  assert.equal(recorded[0].status, 'passed');
  assert.equal(recorded[0].evidenceType, 'deterministic');
  assert.equal(recorded[0].details.smokeStatus, 'excluded');
  assert.deepEqual(
    recorded[0].details.tests,
    excludedProviderTestReferences.map(reference => reference.test),
  );
  assert.deepEqual(
    recorded[0].details.references,
    excludedProviderTestReferences.map(({ test, package: packagePath, level, covers, observes }) => ({
      test,
      package: packagePath,
      level,
      covers,
      observes,
    })),
  );
  assert.equal(calls.filter(call => call.command === 'go').length, 2);
});

test('an unavailable deterministic prerequisite is blocked, not reported as a smoke pass', async () => {
  const recorded = [];
  const session = {
    requestIds: [],
    evidence: {
      record(entry) {
        recorded.push(entry);
      },
    },
  };

  await assert.rejects(
    () => runExcludedProviderEvidence({
      session,
      root: '/repository',
      run: async command => {
        if (command === 'docker') throw new Error('Docker daemon unavailable');
        return { stdout: '' };
      },
    }),
    SmokeBlocked,
  );
  assert.equal(recorded.length, 1);
  assert.equal(recorded[0].status, 'blocked');
  assert.equal(recorded[0].evidenceType, 'deterministic');
  assert.match(recorded[0].scenario, /excluded real-provider/);
});
