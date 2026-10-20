import { SmokeProblem } from './http.mjs';
import { settle } from './session_credential_support.mjs';
import {
  associationRow,
  expirePairingSession,
} from './session_pairing_support.mjs';

function limitProblem(outcome) {
  return outcome.error instanceof SmokeProblem
    && outcome.error.status === 409
    && outcome.error.code === 'receiver_limit_reached';
}

export async function exerciseCapacity(session) {
  const reservations = [];
  for (let index = 0; index < 5; index++) {
    const created = await session.createPairingSession({ mode: 'new_receiver' });
    reservations.push(await session.claimPairing('pair1', created));
  }

  const concurrent = [];
  for (let index = 0; index < 2; index++) {
    concurrent.push(await session.createPairingSession({ mode: 'new_receiver' }));
  }
  const concurrentOutcomes = await Promise.all(
    concurrent.map(created => settle(session.claimPairing('pair1', created))),
  );
  if (!concurrentOutcomes.every(limitProblem)) {
    throw new Error('Concurrent sixth-distinct-Receiver claims were not both refused');
  }

  const canceled = await session.request({
    method: 'DELETE',
    path: `/v1/pairing-sessions/${encodeURIComponent(reservations[0].sessionID)}`,
    token: session.token('pair1', 'user_session'),
  });
  if (canceled.status !== 204) throw new Error(`Pending reservation cancellation returned ${canceled.status}`);
  const afterCancel = await session.claimPairing('pair1', concurrent[0]);

  const activated = reservations[1];
  const senderReport = await session.completePairingSide('pair1', activated);
  if (senderReport.state !== 'awaiting_confirmation') {
    throw new Error('Capacity Pairing activated before its Receiver report');
  }
  const ready = await session.completePairingSide('pair1', activated, activated.token);
  if (ready.state !== 'credential_ready') throw new Error('Capacity Pairing did not activate');
  const issued = await session.redeemReceiver(activated);

  const replacement = await session.createPairingSession({
    mode: 'add_sender',
    receiverID: activated.receiverID,
    token: issued.receiverToken,
  });
  const sameReceiver = await session.claimPairing('pair1', replacement);
  const pendingReplacement = await associationRow(session, sameReceiver.pairingID);
  if (
    pendingReplacement?.state !== 'pending'
    || pendingReplacement.replacesPairingId !== activated.pairingID
  ) {
    throw new Error('Re-pairing did not reserve its own Pairing ID at the capacity limit');
  }

  const sixth = await session.createPairingSession({ mode: 'new_receiver' });
  const stillFull = await session.expectProblem(
    {
      method: 'POST',
      path: `/v1/pairing-sessions/${encodeURIComponent(sixth.sessionID)}/authorize`,
      token: session.token('pair1', 'user_session'),
      body: {},
    },
    409,
    'receiver_limit_reached',
  );

  await expirePairingSession(session, reservations[2].sessionID);
  const afterExpiry = await session.claimPairing('pair1', sixth);

  return {
    reservations: {
      initialDistinct: reservations.length,
      concurrentSixth: concurrentOutcomes.map(outcome => outcome.error.code),
      canceledReleased: afterCancel.pairingID,
      expiredReleased: afterExpiry.pairingID,
    },
    sameReceiver: {
      receiverId: sameReceiver.receiverID,
      pairingId: sameReceiver.pairingID,
      state: pendingReplacement.state,
    },
    stillFull,
    credentialIssue: { receiverId: issued.receiverId, issued: true },
  };
}
