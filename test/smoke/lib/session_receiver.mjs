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
  deterministicEvidence,
  pairingHTTP,
  receiverHTTP,
} from './session_receiver_support.mjs';

export async function runReceiverScenarios(session) {
  const primary = await pairReceiver(session, 'receiver-main');
  const shared = await addSenderToReceiver(session, 'receiver-shared', primary);
  const unrelated = await pairReceiver(session, 'receiver-other');
  const scenarios = [];
  scenarios.push(await runProtocolScenario(
    session,
    'Receiver credential scope, one-slot exclusivity and presence isolation',
    { http: ['PostPairingSessions', 'GetSelfReceivers'], wss: [] },
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
    'post-upgrade type, version, binary and encoding failures close and release the slot',
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
  return {
    scenarios,
    requests: session.requestIds.length,
    receivers: session.receivers.size,
    policyClose: {
      status: 'not_run',
      reason: 'owner recovery and global Receiver revocation are explicit smoke exclusions',
      deterministicEvidence: deterministicEvidence
        .filter(item => /policy|generation|old close/i.test(item.observes))
        .map(item => item.test),
    },
    recoverableBusinessError: {
      status: 'passed',
      channel: 'pairing',
      liveScenario: 'new Receiver enrollment, opaque relay, reconnect and first credential',
      observes: 'pairing_not_ready frame with requestId and detail; the Receiver direction has no recoverable frame',
    },
    deterministicEvidence,
  };
}
