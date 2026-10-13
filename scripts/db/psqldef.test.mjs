import assert from 'node:assert/strict';
import test from 'node:test';
import { toolFixture } from '../quality/tool-fixture.mjs';

const fixture = (t) => toolFixture(t, {
  copy: ['scripts/db/psqldef.sh', 'scripts/db/postgres.sh', 'scripts/db/postgres-image.sh'],
  files: {
    'db/schema/release/00_extensions.sql': 'CREATE SCHEMA release;\n',
    'db/schema/pin_glide/00_extensions.sql': 'CREATE SCHEMA pin_glide;\n',
  },
  tools: ['docker', 'go'],
});

test('the disposable loader applies release and pin_glide as separate target schemas', (t) => {
  const f = fixture(t), result = f.run('bash', ['scripts/db/psqldef.sh', 'bash', '-c', 'exit 0']);
  assert.equal(result.status, 0, result.stderr);
  const applies = f.calls('go').filter((call) => call.args[1] === 'psqldef');
  assert.deepEqual(applies.map((call) => call.config), ['target_schema: release\n', 'target_schema: pin_glide\n']);
  assert.deepEqual(applies.map((call) => call.sql), ['CREATE SCHEMA release;\n', 'CREATE SCHEMA pin_glide;\n']);
  assert.ok(applies.every((call) => call.args.includes('pin_glide')), result.stderr);
  assert.deepEqual(f.calls('docker').at(-1).args, ['rm', '-f', 'disposable-postgres']);
});

for (const [failure, code] of [['apply', 43], ['child', 17]]) {
  test(`the disposable loader preserves ${failure} failure and removes its container`, (t) => {
    const f = fixture(t), result = f.run('bash', ['scripts/db/psqldef.sh', 'bash', '-c', 'exit 17'], { TOOL_FAIL: failure });
    assert.equal(result.status, code, result.stderr);
    assert.deepEqual(f.calls('docker').at(-1).args, ['rm', '-f', 'disposable-postgres']);
  });
}
