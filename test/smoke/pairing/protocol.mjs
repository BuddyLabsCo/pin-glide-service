import {
  expirePairingSession,
  expectPairingState,
  expectSocketClose,
  sessionPath,
} from './support.mjs';

export async function corruptPairingFrame(session) {
  const created = await session.createPairingSession({ mode: 'new_receiver' });
  const claimed = await session.claimPairing('pair2', created);
  const socket = await session.openSocket({ path: sessionPath(claimed), token: claimed.token });
  try {
    await expectPairingState(socket, claimed);
    socket.sendJSON({
      version: 1,
      type: 'pairing_relay',
      sessionId: claimed.sessionID,
    });
    const closed = await expectSocketClose(socket, 1002);
    return {
      validInitialState: true,
      closeCode: closed.code,
      closeReason: closed.reason,
    };
  } finally {
    await socket.close();
  }
}

export async function expireClaimedReplacement(session) {
  const created = await session.createPairingSession({ mode: 'new_receiver' });
  const claimed = await session.claimPairing('pair2', created);
  const socket = await session.openSocket({ path: sessionPath(claimed), token: claimed.token });
  try {
    await expectPairingState(socket, claimed);
    await expirePairingSession(session, claimed.sessionID);
    socket.sendJSON({
      version: 1,
      type: 'pairing_relay',
      sessionId: claimed.sessionID,
      payload: Buffer.from('expired').toString('base64'),
    });
    const closed = await expectSocketClose(socket, 1008);
    const sender = await session.expectProblem(
      {
        method: 'POST',
        path: `/v1/pairing-sessions/${encodeURIComponent(claimed.sessionID)}/complete`,
        token: session.token('pair2', 'user_session'),
        body: { keyConfirmation: 'confirmed' },
      },
      410,
      'pairing_session_expired',
    );
    const receiver = await session.expectProblem(
      {
        method: 'POST',
        path: `/v1/pairing-sessions/${encodeURIComponent(claimed.sessionID)}/complete`,
        token: claimed.token,
        body: { keyConfirmation: 'confirmed' },
      },
      401,
      'credential_expired',
    );
    return {
      closeCode: closed.code,
      senderExpiry: sender,
      receiverExpiry: receiver,
    };
  } finally {
    await socket.close();
  }
}
