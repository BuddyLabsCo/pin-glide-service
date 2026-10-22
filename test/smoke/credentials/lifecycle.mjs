import { createHash } from 'node:crypto';

import { renderExpireAccessSQL, renderExpireRefreshSQL } from './fixtures.mjs';
import {
  durableFacts,
  snapshot,
} from '../harness/scenario.mjs';
import {
  addSenderToReceiver,
  associations,
  pairReceiver,
  receiverState,
} from '../pairing/helpers.mjs';

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
    await session.expectProblem(
      {
        method: 'POST',
        path: '/v1/auth/refresh',
        body: {
          refreshToken: initial.user_session.token,
          operationId: session.newOperationID('scope-management-refresh'),
          attempt: 0,
        },
      },
      401,
      'credential_invalid',
    ),
    await session.expectProblem(
      {
        method: 'POST',
        path: '/v1/auth/refresh',
        body: {
          refreshToken: initial.uploader.token,
          operationId: session.newOperationID('scope-uploader-refresh'),
          attempt: 0,
        },
      },
      401,
      'credential_invalid',
    ),
  ];

  const initialFacts = await durableFacts(session, 'scope');
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
  for (const scope of ['user_session', 'uploader']) {
    const before = initialFacts.credentials.find(item => item.tokenDigest === initial[scope].digest);
    const after = rotationFacts.credentials.find(item => item.tokenDigest === initial[scope].digest);
    if (!before || !after || after.expiresAt !== before.expiresAt) {
      throw new Error(`Refresh changed the retained ${scope} expiry`);
    }
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
    overlapExpiriesUnchanged: true,
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
  if (facts.activeCredentials !== 0) {
    throw new Error('Ordinary logout left Sender credentials active');
  }

  return {
    pairing: { pairingId: pairing.pairingID, receiverId: pairing.receiverID, state: association.state },
    sourceValidation: { conflicting, missing, wrongBearerScope, wrongBodyScope },
    expiredManagement,
    logoutStatus: loggedOut.status,
    afterLogout,
    durable: {
      registrationState: facts.registrationState,
      activeCredentials: facts.activeCredentials,
      revokedCredentials: facts.revokedCredentials,
    },
  };
}

export async function accountSwitch(session) {
  const primary = await pairReceiver(session, 'switch');
  const survivor = await addSenderToReceiver(session, 'a2', primary);
  const switchedCredentials = snapshot(session.sender('switch'));
  const wrongAuthority = {
    missingManagement: await session.expectProblem(
      {
        method: 'POST',
        path: '/v1/auth/logout',
        body: {
          mode: 'switch_account',
          refreshToken: switchedCredentials.refresh.token,
        },
      },
      401,
      'credential_missing',
    ),
    uploader: await session.expectProblem(
      {
        method: 'POST',
        path: '/v1/auth/logout',
        token: switchedCredentials.uploader.token,
        body: { mode: 'switch_account' },
      },
      401,
      'credential_invalid',
    ),
    refresh: await session.expectProblem(
      {
        method: 'POST',
        path: '/v1/auth/logout',
        token: switchedCredentials.refresh.token,
        body: { mode: 'switch_account' },
      },
      401,
      'credential_invalid',
    ),
  };
  const rejectedFacts = await durableFacts(session, 'switch');
  const rejectedPairing = rejectedFacts.pairings.find(item => item.id === primary.pairingID);
  if (rejectedFacts.registrationState !== 'active' || rejectedPairing?.state !== 'active') {
    throw new Error('A rejected Account switch changed the Sender or its Pairing');
  }
  for (const scope of ['user_session', 'uploader', 'refresh']) {
    const credential = rejectedFacts.credentials.find(item => (
      item.tokenDigest === switchedCredentials[scope].digest
    ));
    if (!credential || credential.revokedAt !== null) {
      throw new Error(`A rejected Account switch revoked the ${scope} credential`);
    }
  }
  const switched = await session.request({
    method: 'POST',
    path: '/v1/auth/logout',
    token: switchedCredentials.user_session.token,
    body: { mode: 'switch_account' },
  });
  if (switched.status !== 204) throw new Error(`Account switch returned ${switched.status}`);
  if (switched.cacheControl !== 'no-store') throw new Error('Account switch success was cacheable');
  const primaryRevoked = {
    management: await session.expectProblem(
      { path: '/v1/self/receivers', token: switchedCredentials.user_session.token },
      401,
      'credential_invalid',
    ),
    uploader: await session.expectProblem(
      {
        path: `/v1/messages/smoke-switched-${session.plan.runId}`,
        token: switchedCredentials.uploader.token,
      },
      401,
      'credential_invalid',
    ),
    refresh: await session.expectProblem(
      {
        method: 'POST',
        path: '/v1/auth/refresh',
        body: {
          refreshToken: switchedCredentials.refresh.token,
          operationId: session.newOperationID('switched-refresh'),
          attempt: 0,
        },
      },
      401,
      'credential_revoked',
    ),
  };
  const survivorManagement = await session.verifyManagement(session.token('a2', 'user_session'));
  const survivorUploader = await session.verifyUploader(session.token('a2', 'uploader'));
  const survivorAssociations = await associations(session, 'a2');
  if (!survivorAssociations.some(item => item.pairingId === survivor.pairingID)) {
    throw new Error('Account switch removed another Sender association');
  }
  const survivorOperation = session.newOperationID('switch-survivor-refresh');
  const survivorRotated = await session.refresh('a2', {
    operationID: survivorOperation,
    attempt: 0,
  });
  await session.verifyManagement(survivorRotated.userSession.token);
  await session.verifyUploader(survivorRotated.uploader.token);
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
  if (primaryFacts.activeCredentials !== 0) {
    throw new Error('Account switch left Sender credentials active');
  }

  return {
    receiverId: primary.receiverID,
    switchedPairing: removed,
    survivorPairing: retained,
    receiverState: receiver,
    wrongAuthority,
    rejectedSwitchPreservedPairing: rejectedPairing,
    primaryRevoked,
    survivor: {
      management: survivorManagement.status,
      uploader: survivorUploader.status,
      operationId: survivorOperation,
      attempt: 0,
    },
  };
}

export { scopeAndExpiry };
