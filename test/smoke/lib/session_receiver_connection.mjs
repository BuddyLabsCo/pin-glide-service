import { settle } from './session_credential_support.mjs';
import {
  closeMeaning,
  expectNoFrame,
  openDelivery,
  waitForPresence,
} from './session_pairing_support.mjs';
import {
  expectedProblem,
  exactProblem,
  presenceWait,
  refusal,
  rejectedUpgrade,
  sharedPresence,
} from './session_receiver_support.mjs';

export async function receiverSurface(session, primary, shared, unrelated) {
  const primaryToken = primary.receiverToken;
  const initial = await waitForPresence(session, 'receiver-main', primary.pairingID, 'offline', presenceWait);
  const missing = await rejectedUpgrade(session, {
    path: '/v1/receiver/events',
  }, 401, 'credential_missing');
  const malformed = await rejectedUpgrade(session, {
    path: '/v1/receiver/events',
    token: 'not-a-receiver-credential',
  }, 401, 'credential_invalid');
  const noProtocol = await rejectedUpgrade(session, {
    path: '/v1/receiver/events',
    token: primaryToken,
    protocol: '',
  }, 400, 'invalid_request');
  const wrongProtocol = await rejectedUpgrade(session, {
    path: '/v1/receiver/events',
    token: primaryToken,
    protocol: 'pin-glide.v2',
  }, 400, 'invalid_request');
  const wrongScopeManagement = await rejectedUpgrade(session, {
    path: '/v1/receiver/events',
    token: session.token('receiver-main', 'user_session'),
  }, 401, 'credential_invalid');
  const wrongScopeRefresh = await rejectedUpgrade(session, {
    path: '/v1/receiver/events',
    token: session.token('receiver-main', 'refresh'),
  }, 401, 'credential_invalid');
  const managementSurface = await exactProblem(
    session,
    { path: '/v1/self/receivers', token: primaryToken },
    401,
    'credential_invalid',
  );
  const uploadSurface = await exactProblem(
    session,
    {
      method: 'POST',
      path: '/v1/messages',
      token: primaryToken,
      body: { messageId: session.newOperationID('receiver-upload-scope'), items: [] },
    },
    401,
    'credential_invalid',
  );
  const foreignTarget = await exactProblem(
    session,
    {
      method: 'POST',
      path: '/v1/pairing-sessions',
      token: primaryToken,
      body: { mode: 'add_sender', receiverId: unrelated.receiverID },
    },
    404,
    'resource_not_found',
  );
  const foreignSender = await exactProblem(
    session,
    {
      method: 'POST',
      path: '/v1/pairing-sessions',
      token: unrelated.receiverToken,
      body: { mode: 'add_sender', receiverId: primary.receiverID },
    },
    404,
    'resource_not_found',
  );

  let online;
  let unrelatedOnline;
  let duplicate;
  let unrelatedOffline;
  const delivery = await openDelivery(session, primaryToken);
  try {
    online = await sharedPresence(session, {
      'receiver-main': primary.pairingID,
      'receiver-shared': shared.pairingID,
    }, 'online', presenceWait);
    if (online['receiver-main'].stateChangedAt !== online['receiver-shared'].stateChangedAt) {
      throw new Error('same-Account Pairings reported different presence for one Receiver');
    }
    if (!(Date.parse(online['receiver-main'].stateChangedAt) > Date.parse(initial.stateChangedAt))) {
      throw new Error('Receiver online stateChangedAt did not advance');
    }

    duplicate = await rejectedUpgrade(session, {
      path: '/v1/receiver/events',
      token: primaryToken,
    }, 409, 'receiver_already_connected');
    const stillOnline = await waitForPresence(session, 'receiver-main', primary.pairingID, 'online', presenceWait);
    if (stillOnline.stateChangedAt !== online['receiver-main'].stateChangedAt) {
      throw new Error('a rejected second upgrade changed the held connection presence');
    }

    const unrelatedSocket = await openDelivery(session, unrelated.receiverToken);
    try {
      unrelatedOnline = await waitForPresence(session, 'receiver-other', unrelated.pairingID, 'online', presenceWait);
      const sharedStillOnline = await waitForPresence(session, 'receiver-main', primary.pairingID, 'online', presenceWait);
      if (unrelatedOnline.presence !== 'online' || sharedStillOnline.presence !== 'online') {
        throw new Error('one Receiver context failed to report its own online presence');
      }
      await expectNoFrame(delivery, 'an unrelated Receiver disturbed this connection');
    } finally {
      await unrelatedSocket.close();
    }
    unrelatedOffline = await waitForPresence(session, 'receiver-other', unrelated.pairingID, 'offline', presenceWait);
    if (!(Date.parse(unrelatedOffline.stateChangedAt) > Date.parse(unrelatedOnline.stateChangedAt))) {
      throw new Error('unrelated Receiver offline stateChangedAt did not follow its online state');
    }
    await expectNoFrame(delivery, 'an unrelated Receiver close disturbed the held connection');
  } finally {
    await delivery.close();
  }

  const offline = await sharedPresence(session, {
    'receiver-main': primary.pairingID,
    'receiver-shared': shared.pairingID,
  }, 'offline', presenceWait);
  if (offline['receiver-main'].stateChangedAt !== offline['receiver-shared'].stateChangedAt) {
    throw new Error('closing the shared Receiver left inconsistent presence');
  }

  return {
    preUpgrade: {
      missing,
      malformed,
      noProtocol,
      wrongProtocol,
      wrongScopeManagement,
      wrongScopeRefresh,
      managementSurface,
      uploadSurface,
      foreignTarget,
      foreignSender,
    },
    online: {
      primary: online['receiver-main'].stateChangedAt,
      shared: online['receiver-shared'].stateChangedAt,
    },
    duplicate,
    offline: {
      primary: offline['receiver-main'].stateChangedAt,
      shared: offline['receiver-shared'].stateChangedAt,
    },
    unrelated: {
      receiverId: unrelated.receiverID,
      offlineStateChangedAt: unrelatedOffline.stateChangedAt,
    },
  };
}

export async function concurrentUpgrade(session, pairing) {
  const outcomes = await Promise.all([
    settle(openDelivery(session, pairing.receiverToken)),
    settle(openDelivery(session, pairing.receiverToken)),
  ]);
  const admitted = outcomes.filter(outcome => outcome.value);
  const refused = outcomes.filter(outcome => expectedProblem(409, 'receiver_already_connected')(outcome.error));
  if (admitted.length !== 1 || refused.length !== 1) {
    await Promise.allSettled(outcomes.map(outcome => outcome.value?.close()));
    throw new Error('concurrent Receiver upgrades did not admit exactly one connection');
  }
  let online;
  let duplicate;
  try {
    online = await waitForPresence(session, 'receiver-main', pairing.pairingID, 'online', presenceWait);
    duplicate = refusal(refused[0].error);
  } finally {
    await admitted[0].value.close();
    await waitForPresence(session, 'receiver-main', pairing.pairingID, 'offline', presenceWait);
  }
  // The refused handshake must not have kept a reservation: once the admitted
  // connection closes, the same credential can take the slot again.
  const next = await openDelivery(session, pairing.receiverToken);
  try {
    await waitForPresence(session, 'receiver-main', pairing.pairingID, 'online', presenceWait);
  } finally {
    await next.close();
    await waitForPresence(session, 'receiver-main', pairing.pairingID, 'offline', presenceWait);
  }
  return {
    admitted: 1,
    refused: duplicate,
    stateChangedAt: online.stateChangedAt,
    slotReusableAfterClose: true,
  };
}

async function closeByFrame(session, pairing, send, code, reason) {
  const socket = await openDelivery(session, pairing.receiverToken);
  try {
    await waitForPresence(session, 'receiver-main', pairing.pairingID, 'online', presenceWait);
    send(socket);
    const closed = await socket.waitForClose(5_000);
    if (closed.code !== code || closed.reason !== reason) {
      throw new Error(`Receiver frame closed with ${closed.code} ${closed.reason}, expected ${code} ${reason}`);
    }
    await waitForPresence(session, 'receiver-main', pairing.pairingID, 'offline', presenceWait);
    return { code: closed.code, reason: closed.reason };
  } finally {
    await socket.close();
  }
}

export async function frameClosures(session, pairing) {
  const missingCorrelation = await closeByFrame(
    session,
    pairing,
    socket => socket.sendText('{"version":1,"type":"receipt"}'),
    1002,
    closeMeaning[1002],
  );
  const unknownType = await closeByFrame(
    session,
    pairing,
    socket => socket.sendText('{"version":1,"type":"delivery"}'),
    1002,
    closeMeaning[1002],
  );
  const wrongVersion = await closeByFrame(
    session,
    pairing,
    socket => socket.sendText('{"version":2,"type":"receipt","senderId":"s","messageId":"m"}'),
    1002,
    closeMeaning[1002],
  );
  const binary = await closeByFrame(
    session,
    pairing,
    socket => socket.send(2, Buffer.from([1, 2])),
    1002,
    closeMeaning[1002],
  );
  const malformedJson = await closeByFrame(
    session,
    pairing,
    socket => socket.sendText('{"version":1,"type":"receipt"'),
    1002,
    closeMeaning[1002],
  );
  const invalidEncoding = await closeByFrame(
    session,
    pairing,
    socket => socket.sendText(Buffer.from([0xff])),
    1007,
    closeMeaning[1007],
  );

  const tolerant = await openDelivery(session, pairing.receiverToken);
  try {
    await waitForPresence(session, 'receiver-main', pairing.pairingID, 'online', presenceWait);
    tolerant.sendJSON({
      version: 1,
      type: 'receipt',
      senderId: 'sender-future',
      messageId: 'message-future',
      futureField: { nested: true },
    });
    await expectNoFrame(tolerant, 'an unknown tolerated field closed the Receiver channel');
  } finally {
    const normal = await tolerant.close();
    if (normal.code !== 1000) {
      throw new Error(`normal Receiver close reported ${normal.code} ${normal.reason}`);
    }
  }
  await waitForPresence(session, 'receiver-main', pairing.pairingID, 'offline', presenceWait);

  return {
    missingCorrelation,
    unknownType,
    wrongVersion,
    binary,
    malformedJson,
    invalidEncoding,
    normalClose: 1000,
    unknownFieldTolerated: true,
  };
}
