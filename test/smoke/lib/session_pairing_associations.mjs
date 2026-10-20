import { associations, receiverState } from './session_pairing_helpers.mjs';
import {
  expectNoFrame,
  waitForPresence,
} from './session_pairing_support.mjs';

export async function manageAssociations(session, context) {
  const { pairing: first } = context.enrollment;
  const { receiverToken, delivery } = context.shared;
  const pair1 = session.sender('pair1');

  const uploaderScope = await session.expectProblem(
    { path: '/v1/self/receivers', token: session.token('pair2', 'uploader') },
    401,
    'credential_invalid',
  );
  const receiverScope = await session.expectProblem(
    { path: '/v1/self/receivers', token: receiverToken },
    401,
    'credential_invalid',
  );
  const otherSenderRename = await session.expectProblem(
    {
      method: 'PATCH',
      path: `/v1/self/pairings/${encodeURIComponent(first.pairingID)}`,
      token: session.token('pair2', 'user_session'),
      body: { alias: 'Taken' },
    },
    404,
    'resource_not_found',
  );
  const foreignRename = await session.expectProblem(
    {
      method: 'PATCH',
      path: `/v1/self/pairings/${encodeURIComponent(first.pairingID)}`,
      token: session.token('foreign', 'user_session'),
      body: { alias: 'Taken' },
    },
    404,
    'resource_not_found',
  );

  const renamed = await session.request({
    method: 'PATCH',
    path: `/v1/self/pairings/${encodeURIComponent(first.pairingID)}`,
    token: session.token('pair1', 'user_session'),
    body: { alias: 'Work phone' },
  });
  if (renamed.status !== 200 || renamed.body.alias !== 'Work phone') {
    throw new Error('The owning Sender could not rename its own alias');
  }
  const firstListed = await associations(session, 'pair1');
  if (firstListed.length !== 1 || firstListed[0].alias !== 'Work phone') {
    throw new Error('Renamed alias was absent from the owning Sender list');
  }

  const otherSenderRemoval = await session.expectProblem(
    {
      method: 'DELETE',
      path: `/v1/self/pairings/${encodeURIComponent(first.pairingID)}`,
      token: session.token('pair2', 'user_session'),
    },
    404,
    'resource_not_found',
  );
  const removed = await session.request({
    method: 'DELETE',
    path: `/v1/self/pairings/${encodeURIComponent(first.pairingID)}`,
    token: session.token('pair1', 'user_session'),
  });
  if (removed.status !== 204) throw new Error(`Pairing removal returned ${removed.status}`);

  const notification = await delivery.next();
  if (
    notification.json?.type !== 'pairing_removed'
    || notification.json.senderId !== pair1.id
    || notification.json.pairingId !== first.pairingID
  ) {
    throw new Error('Receiver did not receive the removed Sender and Pairing');
  }

  const firstAfter = await associations(session, 'pair1');
  const secondAfter = await associations(session, 'pair2');
  const sharedAfter = secondAfter.find(item => item.pairingId === context.shared.pairing.pairingID);
  if (firstAfter.length !== 0) throw new Error('Removed Pairing remained in its Sender list');
  if (sharedAfter?.presence !== 'online' || sharedAfter.receiverId !== first.receiverID) {
    throw new Error('Removal did not leave the other Sender association and connection working');
  }
  await waitForPresence(session, 'pair2', context.shared.pairing.pairingID, 'online');
  if (await receiverState(session, first.receiverID) !== 'active') {
    throw new Error('Removal changed the shared Receiver registration');
  }
  await expectNoFrame(delivery, 'Pairing removal closed or polluted the shared Receiver connection');

  context.currentPairing = context.shared.pairing;
  return {
    scope: { uploaderScope, receiverScope },
    ownership: { otherSenderRename, foreignRename, otherSenderRemoval },
    renamed: { pairingId: first.pairingID, alias: renamed.body.alias },
    removed: {
      pairingId: first.pairingID,
      notification: {
        senderId: notification.json.senderId,
        pairingId: notification.json.pairingId,
      },
    },
    survivor: {
      pairingId: sharedAfter.pairingId,
      receiverId: sharedAfter.receiverId,
      presence: sharedAfter.presence,
    },
  };
}
