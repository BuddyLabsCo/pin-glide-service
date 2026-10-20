import { withFailureJoiningCleanup } from './cleanup.mjs';
import { SmokeProblem } from './http.mjs';
import { settle } from './session_credential_support.mjs';
import { waitForPresence } from './session_pairing_support.mjs';

export const pairingHTTP = [
  'PostPairingSessions',
  'PostPairingSessionAuthorize',
  'PostPairingSessionComplete',
  'PostPairingSessionReceiverToken',
  'GetSelfReceivers',
];
export const receiverHTTP = ['GetSelfReceivers'];
export const closeMeaning = {
  1000: '',
  1001: 'service_shutdown',
  1002: 'protocol_error',
  1007: 'invalid_payload',
};
export const deterministicEvidence = [
  {
    test: 'TestConcurrentFirstConnectionsAdmitOneReceiver',
    source: 'test/integration/pairing/possession_test.go',
    level: 'integration',
    observes: 'concurrent upgrades admit exactly one 101 and one 409',
  },
  {
    test: 'TestNewerGenerationOwnsPresenceAcrossOldClose',
    source: 'internal/api/wss/receiver_test.go',
    level: 'unit',
    observes: 'an old close cannot remove a newer connection or its presence',
  },
  {
    test: 'TestGenerationChangeDuringUpgradePreventsTheConnection',
    source: 'internal/api/wss/receiver_test.go',
    level: 'unit',
    observes: 'a generation change cancelling a committing upgrade',
  },
  {
    test: 'TestShutdownEndsAnUpgradeStillCommitting',
    source: 'internal/api/wss/receiver_test.go',
    level: 'unit',
    observes: 'shutdown cancelling an upgrade while its final commit is in flight',
  },
  {
    test: 'TestRecoveryRedemptionAndOldUpgradeCommitOneGeneration',
    source: 'test/integration/pairing/recovery_access_test.go',
    level: 'integration',
    observes: 'the excluded recovery race that ordinary smoke cannot observe',
  },
  {
    test: 'TestOwnerRevocationClosesOneReceiverForEverySender',
    source: 'test/integration/pairing/receiver_revocation_test.go',
    level: 'integration',
    observes: 'the excluded owner-revocation control and policy close across shared Pairings',
  },
  {
    test: 'TestReceiverRevocationSerializesWithRedemptionAndUpgrade',
    source: 'test/integration/pairing/receiver_revocation_test.go',
    level: 'integration',
    observes: 'revocation serialized with redemption and an upgrade still committing',
  },
  {
    test: 'TestDeviceRevokedReportsTheControlThenCloses',
    source: 'internal/api/wss/receiver_test.go',
    level: 'unit',
    observes: 'the excluded policy close that ends a revoked Receiver',
  },
];

export function expectedProblem(status, code) {
  return error => {
    if (!(error instanceof SmokeProblem)) return false;
    if (error.status !== status || error.code !== code) return false;
    return error.type === `/problems/${code}`
      && Boolean(error.title)
      && Boolean(error.detail)
      && error.bodyStatus === status
      && Boolean(error.bodyRequestId)
      && Boolean(error.requestId);
  };
}

export function refusal(problem) {
  return {
    status: problem.status,
    code: problem.code,
    type: problem.type,
    title: problem.title,
    detail: problem.detail,
    requestId: problem.requestId,
  };
}

export function delay(milliseconds) {
  return new Promise(resolve => setTimeout(resolve, milliseconds));
}

export async function waitForHealth(session) {
  let last;
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    try {
      const health = await session.request({ path: '/health' });
      if (health.body?.status === 'ok') return health.requestId;
      last = new Error(`health reported ${health.body?.status || 'no status'}`);
    } catch (error) {
      last = error;
    }
    await delay(500);
  }
  throw new Error(`API did not become healthy after restart: ${last?.message || 'no answer'}`);
}

// deliverOnce uploads one synthetic ciphertext and waits for the live delivery
// frame and its Receipt, so a later reconnect can prove no payload was kept.
export async function deliverOnce(session, label, pairing, socket, tag) {
  const messageID = session.newOperationID(`receiver-delivery-${tag}`);
  const nonce = 'AAAAAAAAAAAAAAAA';
  const ciphertext = 'AQIDBAUGBwgJCgsMDQ4PEBESExQ=';
  const uploading = settle(session.request({
    method: 'POST',
    path: '/v1/messages',
    token: session.token(label, 'uploader'),
    body: {
      messageId: messageID,
      items: [{
        receiverId: pairing.receiverID,
        pairingId: pairing.pairingID,
        cryptoVersion: 1,
        nonce,
        ciphertext,
      }],
    },
  }));
  const frame = await socket.next(10_000);
  if (
    frame.json?.type !== 'delivery'
    || frame.json.senderId !== session.sender(label).id
    || frame.json.receiverId !== pairing.receiverID
    || frame.json.pairingId !== pairing.pairingID
    || frame.json.messageId !== messageID
    || frame.json.nonce !== nonce
    || frame.json.ciphertext !== ciphertext
  ) {
    throw new Error('live Delivery did not preserve its published correlation and bytes');
  }
  socket.sendJSON({
    version: 1,
    type: 'receipt',
    senderId: frame.json.senderId,
    messageId: frame.json.messageId,
  });
  const outcome = await uploading;
  if (outcome.error) throw outcome.error;
  const uploaded = outcome.value;
  const result = uploaded.body?.results?.[0];
  if (uploaded.status !== 207 || result?.status !== 'received') {
    throw new Error(`live Delivery reported ${uploaded.status} ${result?.status || '(no result)'}`);
  }
  return { messageId: messageID, status: result.status, requestId: uploaded.requestId };
}

export async function rejectedUpgrade(session, options, status, code) {
  try {
    const socket = await session.openSocket(options);
    await socket.close();
  } catch (error) {
    if (!expectedProblem(status, code)(error)) throw error;
    return refusal(error);
  }
  throw new Error(`Receiver upgrade unexpectedly succeeded, expected ${status} ${code}`);
}

export async function sharedPresence(session, pairings, wanted, options) {
  const observed = {};
  for (const [label, pairingID] of Object.entries(pairings)) {
    observed[label] = await waitForPresence(session, label, pairingID, wanted, options);
  }
  return observed;
}

export async function withApiRestart(remote, callback) {
  return withFailureJoiningCleanup(
    async () => {
      await remote.stopApi();
      return callback();
    },
    () => remote.startApi(),
    'API restart and restoration both failed',
  );
}
