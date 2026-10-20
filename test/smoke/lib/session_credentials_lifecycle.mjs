import { createHash } from 'node:crypto';

import { renderExpireAccessSQL, renderExpireRefreshSQL } from './fixtures.mjs';
import {
  addSenderToReceiver,
  associations,
  durableFacts,
  pairReceiver,
  receiverState,
  snapshot,
} from './session_credential_support.mjs';

async function scopeAndExpiry(session) {
  const sender = session.sender('scope');
  const initial = snapshot(sender);
  await session.verifyManagement(initial.user_session.token);
  await session.verifyUploader(initial.uploader.token);
  const wrongScopes = [
    await session.expectProblem(
      { path: '/v1/self/receivers', token: initial.refresh.token },
      401,
      'credential_invalid',
    ),
    await session.expectProblem(
      { path: `/v1/messages/smoke-wrong-scope-${session.plan.runId}`, token: initial.user_session.token },
      401,
      'credential_invalid',
    ),
    await session.expectProblem(
      { path: '/v1/self/receivers', token: initial.uploader.token },
      401,
      'credential_invalid',
    ),
  ];

  const firstOperation = session.newOperationID('scope-slide');
  const rotated = await session.refresh('scope', { operationID: firstOperation, attempt: 0 });
  await session.verifyManagement(initial.user_session.token);
  await session.verifyUploader(initial.uploader.token);
  await session.verifyManagement(rotated.userSession.token);
  await session.verifyUploader(rotated.uploader.token);
  const rotationFacts = await durableFacts(session, 'scope', [firstOperation]);
  const rotatedDigest = createHash('sha256').update(rotated.refresh.token).digest('hex');
  const initialRefresh = rotationFacts.credentials.find(item => item.tokenDigest === initial.refresh.digest);
  const rotatedRefresh = rotationFacts.credentials.find(item => item.tokenDigest === rotatedDigest);
  if (!initialRefresh || !rotatedRefresh
    || Date.parse(rotatedRefresh.expiresAt) <= Date.parse(initialRefresh.expiresAt)) {
    throw new Error('Refresh did not advance the sliding refresh expiry');
  }

  await session.remote.sql(renderExpireAccessSQL(session.plan, 'scope'));
  const expiredManagement = await session.expectProblem(
    { path: '/v1/self/receivers', token: rotated.userSession.token },
    401,
    'credential_expired',
  );
  const expiredUploader = await session.expectProblem(
    { path: `/v1/messages/smoke-expired-${session.plan.runId}`, token: rotated.uploader.token },
    401,
    'credential_expired',
  );
  const secondOperation = session.newOperationID('scope-with-expired-access');
  const renewed = await session.refresh('scope', { operationID: secondOperation, attempt: 0 });
  await session.verifyManagement(renewed.userSession.token);
  await session.verifyUploader(renewed.uploader.token);
  await session.remote.sql(renderExpireRefreshSQL(session.plan, 'scope'));
  const expiredRefresh = await session.expectProblem(
    {
      method: 'POST',
      path: '/v1/auth/refresh',
      body: {
        refreshToken: renewed.refresh.token,
        operationId: session.newOperationID('scope-expired-refresh'),
        attempt: 0,
      },
    },
    401,
    'credential_expired',
  );
  await session.verifyManagement(renewed.userSession.token);
  const facts = await durableFacts(session, 'scope', [firstOperation, secondOperation]);
  if (rotationFacts.badLifetimes !== 0) throw new Error('Refresh issued a credential with the wrong lifetime');

  return {
    scopes: { management: 200, uploader: 404, wrongScopes },
    accessOverlap: true,
    slidingRefreshExpiryAdvanced: true,
    expiredAccess: { management: expiredManagement, uploader: expiredUploader },
    expiredRefresh,
    refreshedWithExpiredAccess: true,
    facts: {
      activeCredentials: facts.activeCredentials,
      badLifetimesBeforeFixtureExpiry: rotationFacts.badLifetimes,
      operations: facts.operations,
    },
  };
}

export async function ordinaryLogout(session) {
  const pairing = await pairReceiver(session, 'a1');
  const initial = snapshot(session.sender('a1'));
  const conflicting = await session.expectProblem(
    {
      method: 'POST',
      path: '/v1/auth/logout',
      token: initial.user_session.token,
      body: { refreshToken: initial.refresh.token },
    },
    422,
    'validation_failed',
  );
  const missing = await session.expectProblem(
    { method: 'POST', path: '/v1/auth/logout' },
    401,
    'credential_missing',
  );
  const wrongBearerScope = await session.expectProblem(
    { method: 'POST', path: '/v1/auth/logout', token: initial.refresh.token },
    401,
    'credential_invalid',
  );
  const wrongBodyScope = await session.expectProblem(
    {
      method: 'POST',
      path: '/v1/auth/logout',
      body: { refreshToken: initial.uploader.token },
    },
    401,
    'credential_invalid',
  );
  await session.verifyManagement(initial.user_session.token);

  await session.remote.sql(renderExpireAccessSQL(session.plan, 'a1'));
  const expiredManagement = await session.expectProblem(
    { path: '/v1/self/receivers', token: initial.user_session.token },
    401,
    'credential_expired',
  );
  const loggedOut = await session.request({
    method: 'POST',
    path: '/v1/auth/logout',
    body: { refreshToken: initial.refresh.token },
  });
  if (loggedOut.status !== 204) throw new Error(`Logout returned ${loggedOut.status}`);
  if (loggedOut.cacheControl !== 'no-store') throw new Error('Logout success was cacheable');

  const afterLogout = {
    management: await session.expectProblem(
      { path: '/v1/self/receivers', token: initial.user_session.token },
      401,
      'credential_invalid',
    ),
    uploader: await session.expectProblem(
      { path: `/v1/messages/smoke-revoked-${session.plan.runId}`, token: initial.uploader.token },
      401,
      'credential_invalid',
    ),
    refresh: await session.expectProblem(
      {
        method: 'POST',
        path: '/v1/auth/refresh',
        body: {
          refreshToken: initial.refresh.token,
          operationId: session.newOperationID('logout-revoked-refresh'),
          attempt: 0,
        },
      },
      401,
      'credential_revoked',
    ),
  };
  const facts = await durableFacts(session, 'a1');
  const association = facts.pairings.find(item => item.id === pairing.pairingID);
  if (!association || association.state !== 'active') {
    throw new Error('Ordinary logout did not preserve the active Pairing');
  }
  if (facts.registrationState !== 'active') {
    throw new Error('Ordinary logout permanently closed the Sender registration');
  }

  return {
    pairing: { pairingId: pairing.pairingID, receiverId: pairing.receiverID, state: association.state },
    sourceValidation: { conflicting, missing, wrongBearerScope, wrongBodyScope },
    expiredManagement,
    logoutStatus: loggedOut.status,
    afterLogout,
    durable: { registrationState: facts.registrationState, revokedCredentials: facts.revokedCredentials },
  };
}

export async function accountSwitch(session) {
  const primary = await pairReceiver(session, 'switch');
  const survivor = await addSenderToReceiver(session, 'a2', primary);
  const switched = await session.request({
    method: 'POST',
    path: '/v1/auth/logout',
    token: session.token('switch', 'user_session'),
    body: { mode: 'switch_account' },
  });
  if (switched.status !== 204) throw new Error(`Account switch returned ${switched.status}`);
  if (switched.cacheControl !== 'no-store') throw new Error('Account switch success was cacheable');
  const switchedRevoked = await session.expectProblem(
    { path: '/v1/self/receivers', token: session.token('switch', 'user_session') },
    401,
    'credential_invalid',
  );
  await session.verifyManagement(session.token('a2', 'user_session'));
  const survivorAssociations = await associations(session, 'a2');
  if (!survivorAssociations.some(item => item.pairingId === survivor.pairingID)) {
    throw new Error('Account switch removed another Sender association');
  }
  const primaryFacts = await durableFacts(session, 'switch');
  const survivorFacts = await durableFacts(session, 'a2');
  const removed = primaryFacts.pairings.find(item => item.id === primary.pairingID);
  const retained = survivorFacts.pairings.find(item => item.id === survivor.pairingID);
  if (removed?.state !== 'removed' || retained?.state !== 'active') {
    throw new Error('Account switch changed the wrong association');
  }
  const receiver = await receiverState(session, primary.receiverID);
  if (receiver !== 'active') throw new Error('Account switch removed the shared Receiver registration');
  if (primaryFacts.registrationState !== 'active') {
    throw new Error('Account switch permanently closed the Sender registration');
  }

  return {
    receiverId: primary.receiverID,
    switchedPairing: removed,
    survivorPairing: retained,
    receiverState: receiver,
    switchedCredential: switchedRevoked,
    survivorManagement: 200,
  };
}

export { scopeAndExpiry };
