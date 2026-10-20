import { pairReceiver, associations } from './session_pairing_helpers.mjs';
import {
  associationRow,
  completePairingReports,
  expectNoFrame,
  receiverCredentialFacts,
  waitForPresence,
} from './session_pairing_support.mjs';

export async function attachSecondSender(session, context) {
  const { pairing: held, receiverToken, delivery } = context.enrollment;
  const other = await pairReceiver(session, 'pair2');
  const before = await receiverCredentialFacts(session, held.receiverID);

  const missingCredential = await session.expectProblem(
    {
      method: 'POST',
      path: '/v1/pairing-sessions',
      body: { mode: 'add_sender', receiverId: held.receiverID },
    },
    401,
    'credential_missing',
  );
  const wrongTarget = await session.expectProblem(
    {
      method: 'POST',
      path: '/v1/pairing-sessions',
      token: receiverToken,
      body: { mode: 'add_sender', receiverId: other.receiverID },
    },
    404,
    'resource_not_found',
  );

  const offered = await session.createPairingSession({
    mode: 'add_sender',
    receiverID: held.receiverID,
    token: receiverToken,
  });
  const foreignClaim = await session.expectProblem(
    {
      method: 'POST',
      path: `/v1/pairing-sessions/${encodeURIComponent(offered.sessionID)}/authorize`,
      token: session.token('foreign', 'user_session'),
      body: {},
    },
    404,
    'resource_not_found',
  );
  const claimed = await session.claimPairing('pair2', offered, { alias: 'Laptop' });
  const completed = await completePairingReports(session, 'pair2', claimed);
  if (completed.receiver.pairingId !== claimed.pairingID) {
    throw new Error('Both add_sender reports did not activate the shared association');
  }

  const stored = await associationRow(session, claimed.pairingID);
  if (stored?.state !== 'active' || stored.alias !== 'Laptop' || stored.receiverId !== held.receiverID) {
    throw new Error('add_sender did not create the expected second Sender association');
  }
  const forbiddenIssue = await session.expectProblem(
    {
      method: 'POST',
      path: `/v1/pairing-sessions/${encodeURIComponent(offered.sessionID)}/receiver-token`,
      token: offered.token,
      body: { operationId: session.newOperationID('add-sender-issue'), attempt: 0 },
    },
    409,
    'mode_forbids_credential_issue',
  );

  const after = await receiverCredentialFacts(session, held.receiverID);
  if (after.current !== before.current || after.generation !== before.generation) {
    throw new Error('add_sender changed the shared Receiver credential');
  }
  const firstListed = await associations(session, 'pair1');
  const secondListed = await associations(session, 'pair2');
  const firstShared = firstListed.find(item => item.pairingId === held.pairingID);
  const secondShared = secondListed.find(item => item.pairingId === claimed.pairingID);
  if (firstShared?.presence !== 'online' || secondShared?.presence !== 'online') {
    throw new Error('The shared Receiver connection was not reported online to both Senders');
  }
  if (!secondListed.some(item => item.pairingId === other.pairingID && item.receiverId === other.receiverID)) {
    throw new Error('add_sender disturbed another Pairing of the joining Sender');
  }
  await expectNoFrame(delivery, 'add_sender disturbed the shared Receiver connection');

  context.shared = {
    pairing: claimed,
    otherPairing: other,
    receiverToken,
    delivery,
  };
  return {
    receiver: { receiverId: held.receiverID, credentialGeneration: after.generation },
    firstPairing: { pairingId: held.pairingID, state: 'active', presence: firstShared.presence },
    addedPairing: { pairingId: claimed.pairingID, alias: stored.alias, state: stored.state },
    otherPairing: other.pairingID,
    creationRejections: { missingCredential, wrongTarget, foreignClaim },
    forbiddenIssue,
  };
}
