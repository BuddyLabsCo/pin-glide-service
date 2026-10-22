import assert from 'node:assert/strict';
import test from 'node:test';

import { Evidence } from './lib/evidence.mjs';
import { escapeSQLString } from './lib/fixtures.mjs';

test('smoke evidence redacts every raw credential before serialization', () => {
  const secret = 'smoke-run-uploader-secret';
  const evidence = new Evidence({ runId: 'test-run', secrets: [secret] });
  evidence.record({
    scenario: 'credential check',
    status: 'passed',
    requestId: 'request-1',
    details: {
      token: secret,
      reflected: `prefix-${secret}-suffix`,
      nested: [{ authorization: `Bearer ${secret}` }],
    },
  });
  const serialized = JSON.stringify(evidence.report());
  assert(!serialized.includes(secret));
  assert.match(serialized, /REDACTED/);
  assert.equal(evidence.report().scenarios[0].status, 'passed');
  assert.equal(evidence.report().scenarios[0].evidenceType, 'smoke');
  assert.throws(
    () => evidence.record({ scenario: 'unknown evidence', status: 'passed', evidenceType: 'excluded' }),
    /evidence type must be smoke or deterministic/,
  );
});

test('fixture values escape SQL string boundaries', () => {
  assert.equal(
    escapeSQLString("'; DROP TABLE pin_glide.identity_accounts; --"),
    "'''; DROP TABLE pin_glide.identity_accounts; --'",
  );
});
