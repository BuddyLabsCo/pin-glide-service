import { addSenderToReceiver, pairReceiver } from './session_pairing_helpers.mjs';
import { runProtocolScenario } from './session_pairing_support.mjs';
import {
  concurrentUpgrade,
  frameClosures,
  receiverSurface,
} from './session_receiver_connection.mjs';
import {
  heartbeatLoss,
  invalidatedGeneration,
  issuanceGeneration,
  restartAndReconnect,
} from './session_receiver_faults.mjs';
import {
  coverageTests,
  pairingHTTP,
  receiverHTTP,
  runDeterministicTests,
  deterministicTestReferences,
} from './session_receiver_support.mjs';

export async function runReceiverScenarios(session, { root, runDeterministic = runDeterministicTests } = {}) {
  const primary = await pairReceiver(session, 'receiver-main');
  const shared = await addSenderToReceiver(session, 'receiver-shared', primary);
  const unrelated = await pairReceiver(session, 'receiver-other');
  const scenarios = [];
  scenarios.push(await runProtocolScenario(
    session,
    'Receiver credential scope, one-slot exclusivity and presence isolation',
    { http: ['PostPairingSessions', 'GetSelfReceivers', 'PostMessages'], wss: [] },
    () => receiverSurface(session, primary, shared, unrelated),
  ));
  scenarios.push(await runProtocolScenario(
    session,
    'concurrent first Receiver upgrades admit one connection',
    { http: receiverHTTP, wss: [] },
    () => concurrentUpgrade(session, primary),
  ));
  scenarios.push(await runProtocolScenario(
    session,
    'post-upgrade type, version, malformed, binary and encoding failures close and release the slot',
    { http: receiverHTTP, wss: ['receipt'] },
    () => frameClosures(session, primary),
  ));
  scenarios.push(await runProtocolScenario(
    session,
    'ordinary issuance retransmission supersedes the first Receiver generation',
    { http: pairingHTTP, wss: [] },
    () => issuanceGeneration(session),
  ));
  scenarios.push(await runProtocolScenario(
    session,
    'run-owned invalidated Receiver generation is rejected without touching another context',
    { http: pairingHTTP, wss: [] },
    () => invalidatedGeneration(session),
  ));
  scenarios.push(await runProtocolScenario(
    session,
    'service restart closes 1001, reports offline and reconnects without replay',
    { http: [...receiverHTTP, 'PostMessages'], wss: ['delivery', 'receipt'] },
    () => restartAndReconnect(session, primary),
  ));
  scenarios.push(await runProtocolScenario(
    session,
    'half-open heartbeat closes, reports offline and releases the slot',
    { http: pairingHTTP, wss: [] },
    () => heartbeatLoss(session),
  ));
  // Deterministic evidence runs last, so a failure here cannot hide the live
  // observations already recorded.
  const deterministic = await runDeterministic(root, deterministicTestReferences);
  const passed = new Set(deterministic.tests);
  const covering = coverage => coverageTests(deterministicTestReferences, passed, coverage);
  return {
    scenarios,
    requests: session.requestIds.length,
    receivers: session.receivers.size,
    policyClose: {
      live: 'not_run',
      reason: 'a live 1008 needs a generation change after possession, which only excluded owner recovery or global Receiver revocation performs',
      deterministic: covering('policy-close'),
      livePolicyViolationObserved: false,
    },
    recoverableBusinessError: {
      status: 'not_applicable',
      channel: 'pairing',
      liveScenario: 'new Receiver enrollment, opaque relay, reconnect and first credential',
      observes: 'pairing_not_ready frame with requestId and detail; the Receiver direction has no recoverable frame',
    },
    deterministic: {
      ...deterministic,
      failedUpgradeCleanup: covering('failed-upgrade-cleanup'),
      oldCloseIsolation: covering('old-close'),
    },
    deterministicTestReferences,
  };
}
