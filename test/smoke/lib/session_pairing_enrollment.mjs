import assert from 'node:assert/strict';

import { SmokeProblem } from './http.mjs';
import { associations } from './session_pairing_helpers.mjs';
import {
  assertNoSecret,
  drainPairingUntilClosed,
  expectNoFrame,
  expectPairingState,
  expectRelay,
  opaqueRelayPayload,
  openDelivery,
  sessionPath,
} from './session_pairing_support.mjs';

export async function enrollReceiver(session, context) {
  const label = 'pair1';
  const sender = session.sender(label);
  const created = await session.createPairingSession({ mode: 'new_receiver' });
  const lifetime = Date.parse(created.expiresAt) - Date.now();
  if (!Number.isFinite(lifetime) || lifetime <= 0 || lifetime > 5 * 60 * 1000 + 5_000) {
    throw new Error('Pairing Session was not created with its five-minute absolute lifetime');
  }
  if (created.mode !== 'new_receiver' || created.state !== 'awaiting_authorization') {
    throw new Error('new_receiver creation did not return its fixed unclaimed context');
  }

  const other = await session.createPairingSession({ mode: 'new_receiver' });
  const foreignClaim = await session.expectProblem(
    {
      method: 'POST',
      path: `/v1/pairing-sessions/${encodeURIComponent(created.sessionID)}/authorize`,
      token: session.token('foreign', 'user_session'),
      body: {},
    },
    404,
    'resource_not_found',
  );
  const claimed = await session.claimPairing(label, created);
  const secondClaim = await session.expectProblem(
    {
      method: 'POST',
      path: `/v1/pairing-sessions/${encodeURIComponent(created.sessionID)}/authorize`,
      token: session.token('pair2', 'user_session'),
      body: {},
    },
    409,
    'pairing_session_claimed',
  );

  let receiverSocket;
  let senderSocket;
  let readySocket;
  let delivery;
  let keepDelivery = false;
  try {
    await assert.rejects(
      () => session.openSocket({ path: sessionPath(other), token: created.token }),
      error => error instanceof SmokeProblem
        && error.status === 401
        && error.code === 'credential_invalid',
    );
    await assert.rejects(
      () => session.openSocket({
        path: sessionPath(created),
        token: session.token('foreign', 'user_session'),
      }),
      error => error instanceof SmokeProblem
        && error.status === 404
        && error.code === 'resource_not_found',
    );

    receiverSocket = await session.openSocket({ path: sessionPath(created), token: created.token });
    senderSocket = await session.openSocket({
      path: sessionPath(created),
      token: session.token(label, 'user_session'),
    });
    const receiverInitial = await expectPairingState(receiverSocket, created);
    const senderInitial = await expectPairingState(senderSocket, created);
    assertNoSecret(senderInitial, created.token, 'Sender Pairing state');
    if (senderInitial.session.senderId !== sender.id || receiverInitial.session.state !== 'awaiting_confirmation') {
      throw new Error('Pairing channel initial states do not identify the claimed participants');
    }

    const senderInit = opaqueRelayPayload('hpke_init', 11);
    senderSocket.sendJSON({
      version: 1,
      type: 'pairing_relay',
      sessionId: created.sessionID,
      payload: senderInit,
    });
    await expectRelay(receiverSocket, created, 'sender', senderInit);

    const receiverConfirmation = opaqueRelayPayload('key_confirmation', 21);
    receiverSocket.sendJSON({
      version: 1,
      type: 'pairing_relay',
      sessionId: created.sessionID,
      payload: receiverConfirmation,
    });
    await expectRelay(senderSocket, created, 'receiver', receiverConfirmation);

    await receiverSocket.close();
    senderSocket.sendJSON({
      version: 1,
      type: 'pairing_relay',
      sessionId: created.sessionID,
      payload: opaqueRelayPayload('key_confirmation', 31),
    });
    const absentPeer = await senderSocket.next();
    if (absentPeer.json?.type !== 'error' || absentPeer.json?.error?.code !== 'pairing_not_ready') {
      throw new Error('Relay without a connected peer did not report pairing_not_ready');
    }

    receiverSocket = await session.openSocket({ path: sessionPath(created), token: created.token });
    const reconnected = await expectPairingState(receiverSocket, created);
    if (reconnected.session.expiresAt !== created.expiresAt) {
      throw new Error('Pairing reconnect extended the session lifetime');
    }
    await expectNoFrame(receiverSocket, 'Pairing reconnect replayed handshake bytes');

    const liveAfterReconnect = opaqueRelayPayload('key_confirmation', 41);
    receiverSocket.sendJSON({
      version: 1,
      type: 'pairing_relay',
      sessionId: created.sessionID,
      payload: liveAfterReconnect,
    });
    await expectRelay(senderSocket, created, 'receiver', liveAfterReconnect);
    await Promise.allSettled([receiverSocket.close(), senderSocket.close()]);
    receiverSocket = undefined;
    senderSocket = undefined;

    const notReady = await session.expectProblem(
      {
        method: 'POST',
        path: `/v1/pairing-sessions/${encodeURIComponent(created.sessionID)}/receiver-token`,
        token: created.token,
        body: { operationId: session.newOperationID('issuance-before-activation'), attempt: 0 },
      },
      409,
      'pairing_not_ready',
    );
    const oneReport = await session.completePairingSide(label, claimed);
    if (oneReport.state !== 'awaiting_confirmation' || !oneReport.senderConfirmed || oneReport.receiverConfirmed) {
      throw new Error('One authenticated completion report activated the Pairing');
    }
    const repeatedSenderReport = await session.completePairingSide(label, claimed);
    if (repeatedSenderReport.state !== 'awaiting_confirmation') {
      throw new Error('Repeated Sender confirmation changed the Pairing Session');
    }
    const activated = await session.completePairingSide(label, claimed, claimed.token);
    if (
      activated.state !== 'credential_ready'
      || !activated.senderConfirmed
      || !activated.receiverConfirmed
      || activated.pairingId !== claimed.pairingID
    ) {
      throw new Error('Both authenticated reports did not activate the Pairing');
    }

    const operationID = session.newOperationID('receiver-issuance');
    const managementRedemption = await session.expectProblem(
      {
        method: 'POST',
        path: `/v1/pairing-sessions/${encodeURIComponent(created.sessionID)}/receiver-token`,
        token: session.token(label, 'user_session'),
        body: { operationId: operationID, attempt: 0 },
      },
      401,
      'credential_invalid',
    );
    const dropped = await session.redeemReceiver(claimed, {
      operationID,
      attempt: 0,
      dropped: true,
    });
    if (dropped.status !== 200) throw new Error('Dropped issuance response did not commit');
    const retransmitted = await session.redeemReceiver(claimed, { operationID, attempt: 1 });
    if (retransmitted.receiverId !== created.receiverID || !retransmitted.receiverToken) {
      throw new Error('Issuance retransmission did not return this Receiver credential');
    }
    const staleAttempt = await session.expectProblem(
      {
        method: 'POST',
        path: `/v1/pairing-sessions/${encodeURIComponent(created.sessionID)}/receiver-token`,
        token: created.token,
        body: { operationId: operationID, attempt: 0 },
      },
      409,
      'operation_superseded',
    );

    const listed = await associations(session, label);
    assertNoSecret(listed, retransmitted.receiverToken, 'Sender association list');
    assertNoSecret(activated, retransmitted.receiverToken, 'Sender completion response');
    if (!listed.some(item => item.pairingId === claimed.pairingID && item.presence === 'offline')) {
      throw new Error('Activated Pairing was absent from the Sender list before Receiver possession');
    }

    readySocket = await session.openSocket({ path: sessionPath(created), token: created.token });
    const beforePossession = await expectPairingState(readySocket, created);
    if (!['credential_ready', 'credential_issued'].includes(beforePossession.session.state)) {
      throw new Error(`Pairing channel resumed in ${beforePossession.session.state}`);
    }

    delivery = await openDelivery(session, retransmitted.receiverToken);
    const pairedStates = await drainPairingUntilClosed(readySocket);
    readySocket = undefined;
    if (!pairedStates.some(state => state.state === 'completed')) {
      throw new Error('Receiver possession did not complete the issuance session');
    }
    const lateRedemption = await session.expectProblem(
      {
        method: 'POST',
        path: `/v1/pairing-sessions/${encodeURIComponent(created.sessionID)}/receiver-token`,
        token: created.token,
        body: { operationId: operationID, attempt: 1 },
      },
      410,
      'pairing_session_closed',
    );
    keepDelivery = true;
    context.enrollment = {
      pairing: claimed,
      receiverToken: retransmitted.receiverToken,
      delivery,
    };

    return {
      session: {
        mode: created.mode,
        receiverId: created.receiverID,
        expiresAt: created.expiresAt,
        tokenExpiresAt: created.tokenExpiresAt,
      },
      claims: { foreign: foreignClaim, secondSender: secondClaim },
      relay: { senderToReceiver: true, receiverToSender: true, reconnectWithoutReplay: true },
      activation: {
        oneReport: oneReport.state,
        bothReports: activated.state,
        finalStates: pairedStates.map(state => state.state),
      },
      issuance: {
        notReady,
        operationId: operationID,
        droppedRequestId: dropped.requestId,
        retransmittedAttempt: 1,
        staleAttempt,
        managementRedemption,
        lateRedemption,
      },
    };
  } finally {
    await Promise.allSettled([
      receiverSocket?.close(),
      senderSocket?.close(),
      readySocket?.close(),
      ...(keepDelivery ? [] : [delivery?.close()]),
    ]);
  }
}
