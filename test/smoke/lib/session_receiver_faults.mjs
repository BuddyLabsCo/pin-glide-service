import { renderInvalidateReceiverCredentialSQL } from './receiver_fixtures.mjs';
import { pairReceiver } from './session_pairing_helpers.mjs';
import {
  closeMeaning,
  expectNoFrame,
  openDelivery,
  receiverCredentialFacts,
  waitForPresence,
} from './session_pairing_support.mjs';
import {
  deliverOnce,
  presenceWait,
  rejectedUpgrade,
  waitForHealth,
  withApiRestart,
} from './session_receiver_support.mjs';

export async function issuanceGeneration(session) {
  const created = await session.createPairingSession({ mode: 'new_receiver' });
  const claimed = await session.claimPairing('receiver-shared', created);
  await session.completePairingSide('receiver-shared', claimed);
  await session.completePairingSide('receiver-shared', claimed, claimed.token);

  const operationID = session.newOperationID('receiver-generation');
  const original = await session.redeemReceiver(claimed, { operationID, attempt: 0 });
  const current = await session.redeemReceiver(claimed, { operationID, attempt: 1 });
  const stale = await rejectedUpgrade(session, {
    path: '/v1/receiver/events',
    token: original.receiverToken,
  }, 401, 'credential_invalid');

  const socket = await openDelivery(session, current.receiverToken);
  try {
    const online = await waitForPresence(session, 'receiver-shared', claimed.pairingID, 'online', presenceWait);
    return {
      receiverId: current.receiverId,
      operationId: operationID,
      attempts: [0, 1],
      stale,
      online: online.stateChangedAt,
    };
  } finally {
    await socket.close();
    await waitForPresence(session, 'receiver-shared', claimed.pairingID, 'offline', presenceWait);
  }
}

export async function invalidatedGeneration(session) {
  const invalidated = await pairReceiver(session, 'receiver-main');
  const unaffected = await pairReceiver(session, 'receiver-shared');
  const unaffectedSocket = await openDelivery(session, unaffected.receiverToken);
  try {
    await waitForPresence(session, 'receiver-shared', unaffected.pairingID, 'online', presenceWait);
    await session.remote.sql(
      renderInvalidateReceiverCredentialSQL(session.plan, invalidated.receiverID),
    );
    const rejected = await rejectedUpgrade(session, {
      path: '/v1/receiver/events',
      token: invalidated.receiverToken,
    }, 401, 'credential_invalid');
    const facts = await receiverCredentialFacts(session, invalidated.receiverID);
    if (facts.current !== 0) throw new Error('Receiver credential invalidation left a current generation');
    await expectNoFrame(unaffectedSocket, 'invalidating another Receiver disturbed this connection');
    await unaffectedSocket.close();
    await waitForPresence(session, 'receiver-shared', unaffected.pairingID, 'offline', presenceWait);
    const reauthorized = await openDelivery(session, unaffected.receiverToken);
    try {
      await waitForPresence(session, 'receiver-shared', unaffected.pairingID, 'online', presenceWait);
      await expectNoFrame(reauthorized, 'invalidating another Receiver revoked its credential');
    } finally {
      await reauthorized.close();
    }
    return {
      workflow: 'run-owned invalidated credential rejection, not recovery or global revocation',
      rejected,
      receiverId: invalidated.receiverID,
      currentCredentials: facts.current,
      unaffected: {
        receiverId: unaffected.receiverID,
        credentialStillAuthorizes: true,
      },
    };
  } finally {
    await unaffectedSocket.close();
    await waitForPresence(session, 'receiver-shared', unaffected.pairingID, 'offline', presenceWait);
  }
}

export async function restartAndReconnect(session, pairing) {
  const socket = await openDelivery(session, pairing.receiverToken);
  try {
    await waitForPresence(session, 'receiver-main', pairing.pairingID, 'online', presenceWait);
    const delivered = await deliverOnce(session, 'receiver-main', pairing, socket, 'restart-replay');
    let shutdown;
    await withApiRestart(session.remote, async () => {
      const closed = await socket.waitForClose(20_000);
      if (closed.code !== 1001 || closed.reason !== closeMeaning[1001]) {
        throw new Error(`API shutdown closed the Receiver with ${closed.code} ${closed.reason}`);
      }
      shutdown = { code: closed.code, reason: closed.reason };
    });
    const healthRequestId = await waitForHealth(session);
    const offline = await waitForPresence(session, 'receiver-main', pairing.pairingID, 'offline', {
      attempts: 100,
      intervalMs: 200,
    });

    const reconnected = await openDelivery(session, pairing.receiverToken);
    try {
      const online = await waitForPresence(session, 'receiver-main', pairing.pairingID, 'online', presenceWait);
      if (!(Date.parse(online.stateChangedAt) > Date.parse(offline.stateChangedAt))) {
        throw new Error('reconnect did not publish a later online state');
      }
      await expectNoFrame(reconnected, 'a restarted Receiver replayed old traffic');
    } finally {
      await reconnected.close();
    }
    const finalOffline = await waitForPresence(session, 'receiver-main', pairing.pairingID, 'offline', presenceWait);
    return {
      shutdown,
      offline: offline.stateChangedAt,
      healthRequestId,
      deliveredBeforeRestart: delivered,
      reconnected: true,
      finalOffline: finalOffline.stateChangedAt,
    };
  } finally {
    await socket.close();
  }
}

export async function heartbeatLoss(session) {
  const paired = await pairReceiver(session, 'receiver-other');
  const socket = await session.openSocket({
    path: '/v1/receiver/events',
    token: paired.receiverToken,
    answerPings: false,
  });
  // The service starts its heartbeat at the committed upgrade, so the
  // measurement begins when the 101 reaches this peer.
  const connectedAt = Date.now();
  try {
    await waitForPresence(session, 'receiver-other', paired.pairingID, 'online', presenceWait);
    const offline = await waitForPresence(session, 'receiver-other', paired.pairingID, 'offline', {
      attempts: 180,
      intervalMs: 250,
    });
    await socket.waitForClose(5_000);
    if (socket.pings < 1) throw new Error('the service sent no Receiver heartbeat Ping');
    const firstPingAfterMs = socket.firstPingAt - connectedAt;
    if (firstPingAfterMs < 13_500 || firstPingAfterMs > 16_500) {
      throw new Error(`the first Receiver Ping arrived after only ${firstPingAfterMs}ms`);
    }
    const closeAfterPingMs = socket.closedAt - socket.firstPingAt;
    if (closeAfterPingMs < 4_500 || closeAfterPingMs > 6_500) {
      throw new Error(`the unanswered Receiver Ping closed after only ${closeAfterPingMs}ms`);
    }

    const reconnected = await openDelivery(session, paired.receiverToken);
    try {
      const online = await waitForPresence(session, 'receiver-other', paired.pairingID, 'online', presenceWait);
      if (!(Date.parse(online.stateChangedAt) > Date.parse(offline.stateChangedAt))) {
        throw new Error('heartbeat reconnect did not publish a later online state');
      }
      const pingDeadline = Date.now() + 20_000;
      while (reconnected.pings < 1 && Date.now() < pingDeadline) {
        await new Promise(resolve => setTimeout(resolve, 100));
      }
      if (reconnected.pings < 1) throw new Error('the responsive Receiver received no heartbeat Ping');
      await new Promise(resolve => setTimeout(resolve, 6_000));
      if (reconnected.closeInfo) {
        throw new Error(`the responsive Receiver closed with ${reconnected.closeInfo.code}`);
      }
      const stillOnline = await waitForPresence(session, 'receiver-other', paired.pairingID, 'online');
      if (stillOnline.stateChangedAt !== online.stateChangedAt) {
        throw new Error('answering a heartbeat Ping did not keep the Receiver online');
      }
    } finally {
      await reconnected.close();
    }
    return {
      pingsObserved: socket.pings,
      firstPingAfterMs,
      closeAfterPingMs,
      responsivePingsObserved: 1,
      offline: offline.stateChangedAt,
      reconnected: true,
    };
  } finally {
    await socket.close();
  }
}
