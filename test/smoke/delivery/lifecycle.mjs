import {
  renderPurgeDeliverySQL,
  renderRevokeReceiverRegistrationSQL,
} from './fixtures.mjs';
import { pairReceiver } from '../pairing/helpers.mjs';
import {
  deliveryItem,
  deliveryRowQuery,
  deliveryTarget,
  expectMetadata,
  expectNoMessage,
  expectResult,
  expectSocketClose,
  liveTarget,
  messageID,
  metadata,
  nextDelivery,
  openReceiver,
  sendReceipt,
  upload,
} from './support.mjs';

export async function scenarioMixed(session, context) {
  const live = deliveryTarget(session, 'pair2', context.currentPairing);
  const receiverToken = context.enrollment.receiverToken;
  const offline = deliveryTarget(session, 'pair2', context.shared.otherPairing);
  const unpairedSession = await session.createPairingSession({ mode: 'new_receiver' });
  const unpaired = await session.claimPairing('pair2', unpairedSession);
  const foreign = await pairReceiver(session, 'foreign');
  const revoked = await pairReceiver(session, 'a2');
  // An authorized Receiver is what makes the duplicated entries an ambiguity
  // about ciphertext rather than another authority rejection.
  const duplicated = await pairReceiver(session, 'pair2');

  // Admit one Delivery before the Receiver is revoked, so the retained history
  // of that prepared revocation stays queryable after the registration closes.
  const revokedMessage = messageID(session, 'revoked-history');
  const revokedConnection = deliveryTarget(session, 'a2', revoked);
  const revokedSocket = await openReceiver(session, revokedConnection.receiverToken);
  let revokedResponse;
  try {
    revokedResponse = upload(session, 'a2', revokedMessage, [deliveryItem(revokedConnection)]);
    sendReceipt(revokedSocket, await nextDelivery(revokedSocket, revokedConnection));
    expectResult(await revokedResponse, 0, revoked.receiverID, 'received');
  } finally {
    if (revokedResponse) await revokedResponse.catch(() => {});
    await revokedSocket.close();
  }
  const revokedBefore = expectMetadata(await metadata(session, 'a2', revokedMessage), [{
    receiverID: revoked.receiverID,
    status: 'received',
  }]);
  await session.remote.sql(renderRevokeReceiverRegistrationSQL(revoked.receiverID));

  const message = messageID(session, 'mixed');
  const socket = await openReceiver(session, receiverToken);
  let response;
  let responsePromise;
  const started = Date.now();
  const liveItem = deliveryItem(live);
  try {
    responsePromise = upload(session, 'pair2', message, [
      liveItem,
      deliveryItem(live, { receiverId: '', nonce: 'not-base64' }),
      deliveryItem(live, { receiverId: 'receiver-unknown' }),
      deliveryItem(foreign),
      deliveryItem(unpaired),
      deliveryItem(offline),
      deliveryItem(revoked),
      'not-an-object',
      deliveryItem(live, { receiverId: 'receiver-unsupported', cryptoVersion: 2 }),
      deliveryItem(duplicated),
      deliveryItem(duplicated, { ciphertext: Buffer.alloc(48, 33).toString('base64') }),
    ]);
    const frame = await nextDelivery(socket, live, { messageId: message, item: liveItem });
    sendReceipt(socket, frame);
    response = await responsePromise;
  } finally {
    if (responsePromise) await responsePromise.catch(() => {});
    await socket.close();
  }
  const batchElapsedMs = Date.now() - started;
  if (batchElapsedMs > 15_500) {
    throw new Error(`Mixed Delivery batch took ${batchElapsedMs}ms, beyond its 15-second budget`);
  }
  if (response.status !== 207) throw new Error(`Mixed upload returned ${response.status}`);
  if (!Array.isArray(response.body?.results) || response.body.results.length !== 11) {
    throw new Error(`Mixed upload returned results ${JSON.stringify(response.body?.results)}, want 11`);
  }

  expectResult(response, 0, live.receiverID, 'received');
  expectResult(response, 5, offline.receiverID, 'offline', 'offline');
  const rejected = [
    [1, '', 'invalid_delivery_item'],
    [2, 'receiver-unknown', 'receiver_not_authorized'],
    [3, foreign.receiverID, 'receiver_not_authorized'],
    [4, unpaired.receiverID, 'receiver_not_authorized'],
    [6, revoked.receiverID, 'receiver_not_authorized'],
    [7, '', 'invalid_delivery_item'],
    [8, 'receiver-unsupported', 'unsupported_crypto_version'],
    [9, duplicated.receiverID, 'duplicate_receiver'],
    [10, duplicated.receiverID, 'duplicate_receiver'],
  ].map(([index, receiverID, code]) => (
    expectResult(response, index, receiverID, 'rejected', code).error.code
  ));

  const views = expectMetadata(await metadata(session, 'pair2', message), [
    { receiverID: live.receiverID, status: 'received' },
    { receiverID: offline.receiverID, status: 'offline', code: 'offline' },
  ]);
  const revokedHistory = expectMetadata(await metadata(session, 'a2', revokedMessage), [{
    receiverID: revoked.receiverID,
    status: 'received',
    createdAt: revokedBefore[revoked.receiverID].createdAt,
    completedAt: revokedBefore[revoked.receiverID].completedAt,
  }]);
  const retained = {
    createdAt: views[live.receiverID].createdAt,
    completedAt: views[live.receiverID].completedAt,
  };

  // A Receiver that was offline during the batch reconnects to new traffic
  // only; the batch entry was not queued for it.
  const offlineSocket = await openReceiver(session, offline.receiverToken);
  let offlineReconnect;
  let later;
  try {
    await expectNoMessage(offlineSocket, 'Offline Receiver was sent a queued Delivery');
    const laterMessage = messageID(session, 'offline-reconnect');
    later = upload(session, 'pair2', laterMessage, [deliveryItem(offline)]);
    sendReceipt(offlineSocket, await nextDelivery(offlineSocket, offline));
    offlineReconnect = expectResult(await later, 0, offline.receiverID, 'received').status;
  } finally {
    if (later) await later.catch(() => {});
    await offlineSocket.close();
  }

  for (const [status, code] of [[422, 'validation_failed'], [400, 'invalid_request']]) {
    const result = await session.expectProblem({
      method: 'POST',
      path: '/v1/messages',
      token: session.token('pair2', 'uploader'),
      ...(status === 422 ? { body: { items: [] } } : { rawBody: '{"messageId":' }),
    }, status, code);
    if (!result.requestId) throw new Error('Whole-envelope failure carried no requestId');
  }
  return {
    indexed: response.body.results.length,
    received: live.receiverID,
    offline: offline.receiverID,
    rejected,
    preparedRevocation: revokedHistory[revoked.receiverID].status,
    offlineReconnect,
    retained,
    batchElapsedMs,
  };
}

export async function scenarioDuplicates(session, context) {
  const target = liveTarget(session, context);
  const socket = await openReceiver(session, target.receiverToken);
  const message = messageID(session, 'duplicate');
  const firstItem = deliveryItem(target);
  const secondItem = deliveryItem(target, {
    ciphertext: Buffer.alloc(48, 91).toString('base64'),
  });
  let first;
  let second;
  try {
    first = upload(session, 'pair2', message, [firstItem]);
    second = upload(session, 'pair2', message, [secondItem]);
    const frame = await nextDelivery(socket, target, [
      { messageId: message, item: firstItem },
      { messageId: message, item: secondItem },
    ]);
    sendReceipt(socket, frame);
    const [left, right] = await Promise.all([first, second]);
    for (const response of [left, right]) {
      if (response.status !== 207) throw new Error(`Duplicate upload returned ${response.status}`);
      expectResult(response, 0, target.receiverID, 'received');
    }
    await expectNoMessage(socket, 'Concurrent duplicate uploads sent more than one delivery frame');

    const row = await session.remote.sqlJSON(deliveryRowQuery(session, 'pair2', message));
    if (Number(row.count) !== 1 || row.status !== 'received') {
      throw new Error(`Concurrent duplicate upload retained ${JSON.stringify(row)}`);
    }

    const retained = await upload(session, 'pair2', message, [
      deliveryItem(target, { ciphertext: Buffer.alloc(48, 92).toString('base64') }),
    ]);
    expectResult(retained, 0, target.receiverID, 'received');
    await expectNoMessage(socket, 'A retained duplicate sent another delivery frame');
    const after = await session.remote.sqlJSON(deliveryRowQuery(session, 'pair2', message));
    if (Number(after.count) !== 1 || after.completedAt !== row.completedAt) {
      throw new Error('A retained duplicate rewrote the Delivery history');
    }
    return {
      concurrentResponses: [left.requestId, right.requestId],
      retainedTerminalResult: true,
      storedRows: Number(after.count),
      terminalTimeStable: true,
    };
  } finally {
    await Promise.allSettled([first, second].filter(Boolean));
    await socket.close();
  }
}


export async function scenarioOutcomes(session, context) {
  const live = deliveryTarget(session, 'pair2', context.currentPairing);
  const receiverToken = context.enrollment.receiverToken;
  const socket = await openReceiver(session, receiverToken);
  let unrelated;
  const message = messageID(session, 'receipts');
  let timedOut;
  let history;
  let processing;
  let elapsedMs;
  const response = upload(session, 'pair2', message, [deliveryItem(live)]);
  try {
    unrelated = await openReceiver(session, context.shared.otherPairing.receiverToken);
    const started = Date.now();
    const frame = await nextDelivery(socket, live);
    processing = expectMetadata(await metadata(session, 'pair2', message), [{
      receiverID: live.receiverID,
      status: 'processing',
    }]);
    socket.sendJSON({ version: 1, type: 'receipt', senderId: 'another-sender', messageId: message });
    socket.sendJSON({ version: 1, type: 'receipt', senderId: live.senderID, messageId: `${message}-wrong` });
    // A Receipt from another authenticated Receiver naming this Sender and
    // Message must not acknowledge this connection's Delivery.
    sendReceipt(unrelated, { senderId: live.senderID, messageId: message });
    await expectNoMessage(unrelated, 'An unrelated Receiver received this Delivery');
    timedOut = await response;
    elapsedMs = Date.now() - started;
    expectResult(timedOut, 0, live.receiverID, 'ack_timeout', 'ack_timeout');
    if (elapsedMs < 4_500) throw new Error(`Ack timeout completed in ${elapsedMs}ms, before its five-second bound`);
    if (elapsedMs > 6_000) throw new Error(`Ack timeout completed in ${elapsedMs}ms, beyond its five-second bound`);
    sendReceipt(socket, frame);
    sendReceipt(socket, frame);
    await new Promise(resolve => setTimeout(resolve, 250));
    history = expectMetadata(await metadata(session, 'pair2', message), [{
      receiverID: live.receiverID,
      status: 'ack_timeout',
      code: 'ack_timeout',
    }]);
  } finally {
    await response.catch(() => {});
    await socket.close();
    await unrelated?.close();
  }
  const corruptPayloads = [
    { label: 'missing-correlation', json: { version: 1, type: 'receipt' } },
    { label: 'unparseable-json', raw: '{"version":1,"type":"receipt"' },
  ];
  const corruptCloses = [];
  for (const payload of corruptPayloads) {
    const corruptSocket = await openReceiver(session, receiverToken);
    try {
      if (payload.json) corruptSocket.sendJSON(payload.json);
      else corruptSocket.send(1, Buffer.from(payload.raw));
      corruptCloses.push({ payload: payload.label, code: (await expectSocketClose(corruptSocket, 1002)).code });
    } finally {
      await corruptSocket.close();
    }
  }
  return {
    processingObserved: true,
    wrongCorrelation: timedOut.body.results[0],
    duplicateLateReceiptStatus: history[live.receiverID].status,
    processingStatus: processing[live.receiverID].status,
    ackTimeoutElapsedMs: elapsedMs,
    corruptReceipts: corruptCloses,
  };
}

export async function scenarioRetainedLookup(session, context) {
  const target = liveTarget(session, context);
  const socket = await openReceiver(session, target.receiverToken);
  const message = messageID(session, 'lookup');
  let response;
  try {
    response = upload(session, 'pair2', message, [deliveryItem(target)]);
    const frame = await nextDelivery(socket, target);
    sendReceipt(socket, frame);
    expectResult(await response, 0, target.receiverID, 'received');
    const own = await metadata(session, 'pair2', message);
    const foreign = await session.expectProblem({
      path: `/v1/messages/${encodeURIComponent(message)}`,
      token: session.token('foreign', 'uploader'),
    }, 404, 'resource_not_found');
    const rejectedMessage = messageID(session, 'rejected-only');
    expectResult(await upload(session, 'pair2', rejectedMessage, ['not-an-object']),
      0, '', 'rejected', 'invalid_delivery_item');
    const rejected = await session.expectProblem({
      path: `/v1/messages/${encodeURIComponent(rejectedMessage)}`,
      token: session.token('pair2', 'uploader'),
    }, 404, 'resource_not_found');
    const reread = expectMetadata(await metadata(session, 'pair2', message), [{
      receiverID: target.receiverID,
      status: 'received',
      createdAt: own.body.deliveries[0].createdAt,
      completedAt: own.body.deliveries[0].completedAt,
    }]);
    // Reading retained metadata granted no forwarding authority: once the
    // Pairing is removed, the same entry is rejected on its current authority.
    const removed = await session.request({
      method: 'DELETE',
      path: `/v1/self/pairings/${encodeURIComponent(target.pairingID)}`,
      token: session.token('pair2', 'user_session'),
    });
    if (removed.status !== 204) throw new Error(`Association removal returned ${removed.status}`);
    const noAuthority = await upload(session, 'pair2', message, [deliveryItem(target)]);
    expectResult(noAuthority, 0, target.receiverID, 'rejected', 'receiver_not_authorized');
    await session.remote.sql(renderPurgeDeliverySQL(session.plan, 'pair2', message));
    const purged = await session.expectProblem({
      path: `/v1/messages/${encodeURIComponent(message)}`,
      token: session.token('pair2', 'uploader'),
    }, 404, 'resource_not_found');
    return {
      own: own.body.deliveries.length,
      foreign,
      rejected,
      noAuthority: noAuthority.body.results[0].status,
      purged,
      noRetentionExtension: reread[target.receiverID].createdAt === own.body.deliveries[0].createdAt,
      noPayload: !/ciphertext|nonce/.test(JSON.stringify(own.body)),
    };
  } finally {
    if (response) await response.catch(() => {});
    await socket.close();
  }
}
