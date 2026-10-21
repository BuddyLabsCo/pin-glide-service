import { renderExpireAccessSQL } from './fixtures.mjs';
import { pairReceiver } from './session_pairing_helpers.mjs';
import {
  deliveryItem,
  deliveryRowQuery,
  deliveryTarget,
  expectMetadata,
  expectNoMessage,
  expectResult,
  messageID,
  metadata,
  nextDelivery,
  openReceiver,
  sendReceipt,
  upload,
} from './session_delivery_support.mjs';

export async function scenarioAuthority(session, context) {
  const dedicated = await pairReceiver(session, 'a2');
  const live = deliveryTarget(session, 'a2', dedicated);
  const socket = await openReceiver(session, live.receiverToken);
  const message = messageID(session, 'authority');
  let removal;
  let accepted;
  try {
    accepted = upload(session, 'a2', message, [deliveryItem(live)]);
    const frame = await nextDelivery(socket, live);
    const removed = await session.request({
      method: 'DELETE',
      path: `/v1/self/pairings/${encodeURIComponent(live.pairingID)}`,
      token: session.token('a2', 'user_session'),
    });
    if (removed.status !== 204) throw new Error(`Association removal returned ${removed.status}`);
    sendReceipt(socket, frame);
    const admitted = await accepted;
    expectResult(admitted, 0, live.receiverID, 'received');

    const before = expectMetadata(await metadata(session, 'a2', message), [
      { receiverID: live.receiverID, status: 'received' },
    ]);
    const afterRemoval = await upload(session, 'a2', message, [deliveryItem(live)]);
    expectResult(afterRemoval, 0, live.receiverID, 'rejected', 'receiver_not_authorized');
    const history = expectMetadata(await metadata(session, 'a2', message), [{
      receiverID: live.receiverID,
      status: 'received',
      createdAt: before[live.receiverID].createdAt,
      completedAt: before[live.receiverID].completedAt,
    }]);
    removal = {
      admitted: admitted.body.results[0].status,
      later: afterRemoval.body.results[0].status,
      history: history[live.receiverID].status,
    };
  } finally {
    if (accepted) await accepted.catch(() => {});
    await socket.close();
  }

  // An admitted Delivery keeps the authority it was admitted with. Logging out
  // revokes every access credential without reaching back into that work, and
  // the later submission validates the current authority and is rejected.
  const logoutPairing = await pairReceiver(session, 'order');
  const logoutTarget = deliveryTarget(session, 'order', logoutPairing);
  const logoutSocket = await openReceiver(session, logoutTarget.receiverToken);
  const logoutMessage = messageID(session, 'authority-logout');
  let logout;
  let logoutUpload;
  try {
    logoutUpload = upload(session, 'order', logoutMessage, [deliveryItem(logoutTarget)]);
    const frame = await nextDelivery(logoutSocket, logoutTarget);
    // The Delivery is admitted and in flight before logout revokes the access
    // credentials, so its arrival still completes with the frozen authority.
    const logoutResponse = await session.request({
      method: 'POST',
      path: '/v1/auth/logout',
      body: { refreshToken: session.token('order', 'refresh') },
    });
    if (logoutResponse.status !== 204) throw new Error(`Logout returned ${logoutResponse.status}`);
    sendReceipt(logoutSocket, frame);
    const admittedLogout = await logoutUpload;
    expectResult(admittedLogout, 0, logoutTarget.receiverID, 'received');
    const later = await session.expectProblem({
      method: 'POST',
      path: '/v1/messages',
      token: session.token('order', 'uploader'),
      body: { messageId: logoutMessage, items: [deliveryItem(logoutTarget)] },
    }, 401, 'credential_invalid');
    const retained = await session.remote.sqlJSON(deliveryRowQuery(session, 'order', logoutMessage));
    if (Number(retained.count) !== 1 || retained.status !== 'received') {
      throw new Error('Logout changed the admitted Delivery history');
    }
    await expectNoMessage(logoutSocket, 'Logout caused a compensating delivery write');
    logout = {
      admitted: admittedLogout.body.results[0].status,
      later,
      retained: retained.status,
    };
  } finally {
    if (logoutUpload) await logoutUpload.catch(() => {});
    await logoutSocket.close();
  }

  // Expiring the access credential of an admitted Delivery has the same frozen
  // effect. A renewal recovers management access so the retained history stays
  // queryable, and the expired credential can never resubmit it.
  const aged = await pairReceiver(session, 'aging');
  const expiryTarget = deliveryTarget(session, 'aging', aged);
  const expirySocket = await openReceiver(session, expiryTarget.receiverToken);
  const expiryMessage = messageID(session, 'authority-expiry');
  let expiry;
  let expiryUpload;
  try {
    expiryUpload = upload(session, 'aging', expiryMessage, [deliveryItem(expiryTarget)]);
    const frame = await nextDelivery(expirySocket, expiryTarget);
    const before = expectMetadata(await metadata(session, 'aging', expiryMessage), [{
      receiverID: expiryTarget.receiverID,
      status: 'processing',
    }]);
    // The admitted Delivery stays in flight while the access credential
    // expires; it still reaches its own terminal outcome.
    await session.remote.sql(renderExpireAccessSQL(session.plan, 'aging'));
    sendReceipt(expirySocket, frame);
    expectResult(await expiryUpload, 0, expiryTarget.receiverID, 'received');
    const expiredUploader = session.token('aging', 'uploader');
    const later = await session.expectProblem({
      method: 'POST',
      path: '/v1/messages',
      token: expiredUploader,
      body: { messageId: expiryMessage, items: [deliveryItem(expiryTarget)] },
    }, 401, 'credential_expired');
    await session.refresh('aging', { operationID: session.newOperationID('delivery-expiry-renewal') });
    const after = expectMetadata(await metadata(session, 'aging', expiryMessage), [{
      receiverID: expiryTarget.receiverID,
      status: 'received',
      createdAt: before[expiryTarget.receiverID].createdAt,
    }]);
    await expectNoMessage(expirySocket, 'Access expiry caused a compensating delivery write');
    expiry = {
      admitted: 'received',
      later,
      retained: after[expiryTarget.receiverID].status,
    };
  } finally {
    if (expiryUpload) await expiryUpload.catch(() => {});
    await expirySocket.close();
  }

  return {
    removal,
    logout,
    expiry,
  };
}
