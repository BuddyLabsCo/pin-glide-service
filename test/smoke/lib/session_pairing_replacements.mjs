import { associations } from './session_pairing_helpers.mjs';
import {
  associationRow,
  expirePairingSession,
  receiverCredentialFacts,
} from './session_pairing_support.mjs';

async function replacement(session, context, alias) {
  const created = await session.createPairingSession({
    mode: 'add_sender',
    receiverID: context.currentPairing.receiverID,
    token: context.shared.receiverToken,
  });
  return session.claimPairing('pair2', created, alias === undefined ? {} : { alias });
}

async function activate(session, pairing) {
  const sender = await session.completePairingSide('pair2', pairing);
  if (sender.state !== 'awaiting_confirmation') {
    throw new Error('One replacement report committed the Pairing');
  }
  const receiver = await session.completePairingSide('pair2', pairing, pairing.token);
  if (receiver.state !== 'completed') {
    throw new Error('Both replacement reports did not activate the Pairing');
  }
  return receiver;
}

export async function replaceAssociation(session, context) {
  const original = context.currentPairing;
  const originalRow = await associationRow(session, original.pairingID);
  if (originalRow?.state !== 'active') throw new Error('Replacement began without an active Pairing');
  const credentialsBefore = await receiverCredentialFacts(session, original.receiverID);

  const canceled = await replacement(session, context, 'Canceled');
  const failedReport = await session.expectProblem(
    {
      method: 'POST',
      path: `/v1/pairing-sessions/${encodeURIComponent(canceled.sessionID)}/complete`,
      token: session.token('pair2', 'user_session'),
      body: { keyConfirmation: 'not_available' },
    },
    422,
    'validation_failed',
  );
  const canceledResponse = await session.request({
    method: 'DELETE',
    path: `/v1/pairing-sessions/${encodeURIComponent(canceled.sessionID)}`,
    token: session.token('pair2', 'user_session'),
  });
  if (canceledResponse.status !== 204) throw new Error(`Replacement cancellation returned ${canceledResponse.status}`);
  if ((await associationRow(session, original.pairingID))?.state !== 'active') {
    throw new Error('Canceled replacement changed the working Pairing');
  }

  const expired = await replacement(session, context, 'Expired');
  await expirePairingSession(session, expired.sessionID);
  const expiredSenderReport = await session.expectProblem(
    {
      method: 'POST',
      path: `/v1/pairing-sessions/${encodeURIComponent(expired.sessionID)}/complete`,
      token: session.token('pair2', 'user_session'),
      body: { keyConfirmation: 'confirmed' },
    },
    410,
    'pairing_session_expired',
  );
  const expiredReceiverReport = await session.expectProblem(
    {
      method: 'POST',
      path: `/v1/pairing-sessions/${encodeURIComponent(expired.sessionID)}/complete`,
      token: expired.token,
      body: { keyConfirmation: 'confirmed' },
    },
    401,
    'credential_expired',
  );
  if ((await associationRow(session, original.pairingID))?.state !== 'active') {
    throw new Error('Expired replacement changed the working Pairing');
  }

  const inherited = await replacement(session, context);
  const inheritedRow = await associationRow(session, inherited.pairingID);
  if (
    inheritedRow?.state !== 'pending'
    || inheritedRow.replacesPairingId !== original.pairingID
    || inheritedRow.alias !== originalRow.alias
  ) {
    throw new Error('Replacement did not preserve the current alias and expected Pairing');
  }
  const inheritedComplete = await activate(session, inherited);
  if ((await associationRow(session, original.pairingID))?.state !== 'removed') {
    throw new Error('Successful replacement did not retire the expected Pairing');
  }
  if ((await associationRow(session, inherited.pairingID))?.alias !== originalRow.alias) {
    throw new Error('Successful replacement did not preserve the alias');
  }

  context.currentPairing = inherited;
  const explicit = await replacement(session, context, 'Work');
  await activate(session, explicit);
  if ((await associationRow(session, inherited.pairingID))?.state !== 'removed') {
    throw new Error('Explicit replacement did not retire the previous Pairing');
  }
  const explicitRow = await associationRow(session, explicit.pairingID);
  if (explicitRow?.state !== 'active' || explicitRow.alias !== 'Work') {
    throw new Error('Explicit replacement did not install its new alias');
  }

  context.currentPairing = explicit;
  const left = await replacement(session, context, 'Left');
  const right = await replacement(session, context, 'Right');
  const leftComplete = await activate(session, left);
  const rightSender = await session.completePairingSide('pair2', right);
  if (rightSender.state !== 'awaiting_confirmation') {
    throw new Error('Stale replacement reported a premature terminal state');
  }
  const rightReceiver = await session.expectProblem(
    {
      method: 'POST',
      path: `/v1/pairing-sessions/${encodeURIComponent(right.sessionID)}/complete`,
      token: right.token,
      body: { keyConfirmation: 'confirmed' },
    },
    409,
    'operation_superseded',
  );
  if (await associationRow(session, right.pairingID)) {
    throw new Error('Superseded replacement kept its reservation');
  }
  const leftRow = await associationRow(session, left.pairingID);
  if (leftRow?.state !== 'active' || leftRow.alias !== 'Left') {
    throw new Error('Winning replacement is not the one current Pairing');
  }

  const credentialsAfter = await receiverCredentialFacts(session, original.receiverID);
  if (
    credentialsBefore.current !== credentialsAfter.current
    || credentialsBefore.generation !== credentialsAfter.generation
  ) {
    throw new Error('Re-pairing changed the shared Receiver credential');
  }
  const listed = await associations(session, 'pair2');
  if (listed.filter(item => item.receiverId === original.receiverID).length !== 1) {
    throw new Error('Re-pairing left more than one active association for one Receiver');
  }

  context.currentPairing = left;
  return {
    canceled: { failedReport, cancelStatus: canceledResponse.status },
    expired: { sender: expiredSenderReport, receiver: expiredReceiverReport },
    inheritedAlias: {
      before: originalRow.alias,
      after: inheritedRow.alias,
      state: inheritedComplete.state,
    },
    explicitAlias: { alias: explicitRow.alias, state: explicitRow.state },
    stale: {
      winner: leftRow.state,
      receiverReport: rightReceiver,
      loserReservationReleased: true,
    },
    receiverCredential: credentialsAfter,
  };
}
