import {
  runScenario,
  verifyCheckpoints,
} from '../harness/scenario.mjs';
import { verifyDiagnosticRedaction } from '../harness/diagnostics.mjs';
import {
  accountSwitch,
  ordinaryLogout,
  scopeAndExpiry,
} from './lifecycle.mjs';
import {
  droppedResponseRecovery,
  orderedAttempts,
  parentExpiry,
  refreshLogoutRace,
  reuseIsolation,
} from './refresh.mjs';
import {
  permanentAndFailureOutcomes,
  preExistingRevokedFamily,
} from './failures.mjs';

export async function runSenderCredentialScenarios(session) {
  const scenarios = [];
  scenarios.push(await runScenario(
    session,
    'M/U/F scope, expiry, sliding renewal and access overlap',
    () => scopeAndExpiry(session),
  ));
  scenarios.push(await runScenario(
    session,
    'ordinary logout, exactly-one-source validation and Pairing preservation',
    () => ordinaryLogout(session),
  ));
  scenarios.push(await runScenario(
    session,
    'switch_account removes only the current Sender associations',
    () => accountSwitch(session),
  ));
  scenarios.push(await runScenario(
    session,
    'ordered refresh attempts and stale-operation guards',
    () => orderedAttempts(session),
  ));
  scenarios.push(await runScenario(
    session,
    'dropped refresh response and immediate-predecessor replacement',
    () => droppedResponseRecovery(session),
  ));
  scenarios.push(await runScenario(
    session,
    'out-of-window refresh rejection and family isolation',
    () => reuseIsolation(session),
  ));
  scenarios.push(await runScenario(
    session,
    'original refresh predecessor expiry rejection',
    () => parentExpiry(session),
  ));
  scenarios.push(await runScenario(
    session,
    'refresh and logout serialization',
    () => refreshLogoutRace(session),
  ));
  scenarios.push(await runScenario(
    session,
    'pre-existing revoked family and current family isolation',
    () => preExistingRevokedFamily(session),
  ));
  scenarios.push(await runScenario(
    session,
    'invalid, expired, revoked and permanent registration outcomes',
    () => permanentAndFailureOutcomes(session),
  ));
  scenarios.push(await runScenario(
    session,
    'runner refresh checkpoints and diagnostic redaction',
    async () => ({
      ...(await verifyCheckpoints(session)),
      diagnostics: await verifyDiagnosticRedaction(session),
    }),
  ));
  return {
    scenarios,
    accounts: session.plan.accounts.length,
    senders: session.plan.senders.length,
    requests: session.requestIds.length,
  };
}
