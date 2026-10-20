import { manageAssociations } from './session_pairing_associations.mjs';
import { exerciseCapacity } from './session_pairing_capacity.mjs';
import { enrollReceiver } from './session_pairing_enrollment.mjs';
import { expireRestrictedCredential } from './session_pairing_expiry.mjs';
import { corruptPairingFrame, expireClaimedReplacement } from './session_pairing_protocol.mjs';
import { replaceAssociation } from './session_pairing_replacements.mjs';
import { attachSecondSender } from './session_pairing_shared.mjs';
import { runPairingScenario } from './session_pairing_support.mjs';

const pairingHTTP = [
  'PostPairingSessions',
  'PostPairingSessionAuthorize',
  'PostPairingSessionComplete',
  'PostPairingSessionReceiverToken',
];
const associationHTTP = ['GetSelfReceivers', 'PatchSelfPairing', 'DeleteSelfPairing'];
const pairingWSS = ['pairingState', 'pairingRelayIn', 'pairingRelayOut', 'credentialReady', 'error'];

export async function runPairingScenarios(session) {
  const context = {};
  const scenarios = [];
  try {
    scenarios.push(await runPairingScenario(
      session,
      'new Receiver enrollment, opaque relay, reconnect and first credential',
      { http: [...pairingHTTP, 'GetSelfReceivers'], wss: pairingWSS },
      () => enrollReceiver(session, context),
    ));
    scenarios.push(await runPairingScenario(
      session,
      'B expiry preserves the activated Pairing and shared connection',
      { http: ['GetSelfReceivers'], wss: ['pairingState'] },
      () => expireRestrictedCredential(session, context),
    ));
    scenarios.push(await runPairingScenario(
      session,
      'add_sender preserves the shared Receiver credential, connection and other Pairings',
      { http: pairingHTTP, wss: ['pairingState', 'pairingRelayIn', 'pairingRelayOut'] },
      () => attachSecondSender(session, context),
    ));
    scenarios.push(await runPairingScenario(
      session,
      'own association list, alias scope and pairing_removed notification',
      { http: associationHTTP, wss: ['pairingRemoved'] },
      () => manageAssociations(session, context),
    ));
    scenarios.push(await runPairingScenario(
      session,
      'atomic re-pair replacement, alias preservation and stale supersession',
      { http: pairingHTTP, wss: ['pairingState'] },
      () => replaceAssociation(session, context),
    ));
    scenarios.push(await runPairingScenario(
      session,
      'distinct Receiver capacity, pending reservations and same-Receiver replacement',
      { http: pairingHTTP, wss: ['pairingState'] },
      () => exerciseCapacity(session),
    ));
    scenarios.push(await runPairingScenario(
      session,
      'corrupt Pairing protocol frame closes with a protocol error',
      { http: ['PostPairingSessions', 'PostPairingSessionAuthorize'], wss: ['pairingState', 'pairingRelayIn'] },
      () => corruptPairingFrame(session),
    ));
    scenarios.push(await runPairingScenario(
      session,
      'expired claimed Pairing Session releases its reservation and refuses relay',
      {
        http: ['PostPairingSessions', 'PostPairingSessionAuthorize', 'PostPairingSessionComplete'],
        wss: ['pairingState', 'pairingRelayIn'],
      },
      () => expireClaimedReplacement(session),
    ));
    return {
      scenarios,
      requests: session.requestIds.length,
      receivers: session.receivers.size,
      context,
    };
  } finally {
    await context.enrollment?.delivery?.close();
  }
}
