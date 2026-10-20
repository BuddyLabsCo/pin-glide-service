import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

// The CLI tests fake GitHub and cannot observe checkout's persisted credentials.
test('release pushes and PR commands use the generated GitHub App token', () => {
  const workflow = readFileSync(new URL('../../.github/workflows/auto-release-pr.yml', import.meta.url), 'utf8');
  const steps = workflow.split(/^      - /m).slice(1);
  const appToken = steps.findIndex((step) => /uses: actions\/create-github-app-token@/.test(step));
  const checkout = steps.findIndex((step) => /uses: actions\/checkout@/.test(step));
  const release = steps.find((step) => /run: node scripts\/release\/auto-release-pr\.mjs/.test(step));

  assert.ok(appToken >= 0 && checkout > appToken, 'Generate the App token before checkout');
  assert.match(steps[appToken], /^        id: app-token$/m);
  assert.match(steps[checkout], /^        with:\n(?:          .*\n)*?          token: \$\{\{ steps\.app-token\.outputs\.token \}\}$/m);
  assert.match(steps[checkout], /^          fetch-depth: 0$/m);
  assert.match(release, /^          GH_TOKEN: \$\{\{ steps\.app-token\.outputs\.token \}\}$/m);
});
