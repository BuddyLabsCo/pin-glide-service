import { createHash } from 'node:crypto';

import {
  renderAgeRefreshOperationSQL,
  renderExpireRefreshParentSQL,
} from './fixtures.mjs';
import { SmokeProblem } from './http.mjs';
import {
  durableFacts,
  settle,
  snapshot,
} from './session_credential_support.mjs';

function credentialsUnchanged(before, after) {
  return JSON.stringify(before.credentials) === JSON.stringify(after.credentials);
}

async function orderedAttempts(session) {
  const initial = snapshot(session.sender('order'));
  const firstOperation = session.newOperationID('order-attempt-first');
  const first = await session.refresh('order', { operationID: firstOperation, attempt: 1 });
  const afterFirst = await durableFacts(session, 'order', [firstOperation]);
  const delayedOriginal = await session.expectProblem(
    {
      method: 'POST',
      path: '/v1/auth/refresh',
      body: {
        refreshToken: initial.refresh.token,
        operationId: firstOperation,
        attempt: 0,
      },
    },
    409,
    'operation_superseded',
  );
  const afterDelayedOriginal = await durableFacts(session, 'order', [firstOperation]);
  if (!credentialsUnchanged(afterFirst, afterDelayedOriginal)) {
    throw new Error('A superseded original attempt changed the credential family');
  }

  const secondOperation = session.newOperationID('order-later');
  const second = await session.refresh('order', {
    operationID: secondOperation,
    attempt: 0,
    token: first.refresh.token,
  });
  const beforeStale = await durableFacts(session, 'order', [firstOperation, secondOperation]);
  const stale = [];
  for (const attempt of [0, 1]) {
    stale.push(await session.expectProblem(
      {
        method: 'POST',
        path: '/v1/auth/refresh',
        body: {
          refreshToken: initial.refresh.token,
          operationId: firstOperation,
          attempt,
        },
      },
      409,
      'operation_superseded',
    ));
  }
  const unsupported = await session.expectProblem(
    {
      method: 'POST',
      path: '/v1/auth/refresh',
      body: {
        refreshToken: second.refresh.token,
        operationId: session.newOperationID('order-unsupported'),
        attempt: 2,
      },
    },
    422,
    'validation_failed',
  );
  const afterStale = await durableFacts(session, 'order', [firstOperation, secondOperation]);
  if (!credentialsUnchanged(beforeStale, afterStale)) {
    throw new Error('A rejected stale attempt changed the credential family');
  }
  await session.verifyManagement(second.userSession.token);

  const thirdOperation = session.newOperationID('order-continued');
  const third = await session.refresh('order', {
    operationID: thirdOperation,
    attempt: 0,
    token: second.refresh.token,
  });
  await session.verifyManagement(third.userSession.token);
  const facts = await durableFacts(session, 'order', [
    firstOperation,
    secondOperation,
    thirdOperation,
  ]);
  const attempts = Object.fromEntries(facts.operations.map(item => [item.operationId, item.attempt]));
  if (attempts[firstOperation] !== 1 || attempts[secondOperation] !== 0 || attempts[thirdOperation] !== 0) {
    throw new Error('Refresh operation guards were not persisted at their accepted attempts');
  }

  return {
    attemptOneFirst: true,
    delayedOriginal,
    stale,
    unsupported,
    persistedAttempts: attempts,
    latestCheckpoint: session.sender('order').latestRefresh,
    rejectedAttemptsIssuedNothing: true,
  };
}

export async function droppedResponseRecovery(session) {
  const initial = snapshot(session.sender('recover'));
  const operationID = session.newOperationID('recover-lost-response');
  const dropped = await session.refresh('recover', {
    operationID,
    attempt: 0,
    dropped: true,
  });
  if (dropped.status !== 200) throw new Error(`Dropped refresh returned ${dropped.status}`);
  if (dropped.cacheControl !== 'no-store') throw new Error('Dropped refresh response was cacheable');
  if (session.sender('recover').latestRefresh !== undefined) {
    throw new Error(
      `An uncertain refresh response advanced the runner checkpoint: ${JSON.stringify(
        session.sender('recover').latestRefresh,
      )}`,
    );
  }
  const recovered = await session.refresh('recover', {
    operationID,
    attempt: 1,
    token: initial.refresh.token,
  });
  await session.verifyManagement(recovered.userSession.token);
  await session.verifyUploader(recovered.uploader.token);
  const parentDigest = createHash('sha256').update(initial.refresh.token).digest('hex');
  const facts = await durableFacts(session, 'recover', [operationID]);
  const operation = facts.operations.find(item => item.operationId === operationID);
  if (operation?.attempt !== 1 || operation.parentDigest !== parentDigest) {
    throw new Error('Lost-response recovery did not preserve the operation predecessor and attempt');
  }
  if (recovered.refresh.token === initial.refresh.token) {
    throw new Error('Lost-response recovery reused the consumed refresh credential');
  }

  return {
    dropped: { status: dropped.status, requestId: dropped.requestId },
    recoveredAttempt: operation.attempt,
    predecessorDigestMatched: true,
    latestCheckpoint: session.sender('recover').latestRefresh,
  };
}

export async function reuseIsolation(session) {
  const initial = snapshot(session.sender('window'));
  const operationID = session.newOperationID('window-expired-recovery');
  const rotated = await session.refresh('window', { operationID, attempt: 0 });
  await session.remote.sql(renderAgeRefreshOperationSQL(session.plan, 'window', operationID));
  const outsideWindow = await session.expectProblem(
    {
      method: 'POST',
      path: '/v1/auth/refresh',
      body: {
        refreshToken: initial.refresh.token,
        operationId: operationID,
        attempt: 1,
      },
    },
    401,
    'refresh_reuse_detected',
  );
  const successorRevoked = await session.expectProblem(
    {
      method: 'POST',
      path: '/v1/auth/refresh',
      body: {
        refreshToken: rotated.refresh.token,
        operationId: session.newOperationID('window-successor'),
        attempt: 0,
      },
    },
    401,
    'credential_revoked',
  );
  const reusedFamily = {
    management: await session.expectProblem(
      { path: '/v1/self/receivers', token: rotated.userSession.token },
      401,
      'credential_invalid',
    ),
    uploader: await session.expectProblem(
      {
        path: `/v1/messages/smoke-reuse-${session.plan.runId}`,
        token: rotated.uploader.token,
      },
      401,
      'credential_invalid',
    ),
    refresh: successorRevoked,
  };
  const siblingOperation = session.newOperationID('reuse-sibling');
  const sibling = await session.refresh('order', {
    operationID: siblingOperation,
    attempt: 0,
  });
  await session.verifyManagement(sibling.userSession.token);
  await session.verifyUploader(sibling.uploader.token);
  const foreignOperation = session.newOperationID('foreign-isolation');
  const foreign = await session.refresh('foreign', {
    operationID: foreignOperation,
    attempt: 0,
  });
  await session.verifyManagement(foreign.userSession.token);
  await session.verifyUploader(foreign.uploader.token);
  const facts = await durableFacts(session, 'window', [operationID]);
  if (
    facts.registrationState !== 'active'
    || facts.activeCredentials !== 0
    || facts.revokedCredentials < 3
  ) {
    throw new Error('Refresh reuse did not revoke only its Sender family');
  }

  return {
    outsideWindow,
    reusedFamily,
    revokedOnlyOwnFamily: true,
    sameAccountSibling: {
      senderId: session.sender('order').id,
      operationId: siblingOperation,
      attempt: 0,
    },
    unrelatedSender: {
      senderId: session.sender('foreign').id,
      operationId: foreignOperation,
      attempt: 0,
    },
  };
}

export async function parentExpiry(session) {
  const initial = snapshot(session.sender('aging'));
  const oldOperation = session.newOperationID('aging-parent');
  const rotated = await session.refresh('aging', { operationID: oldOperation, attempt: 0 });
  await session.remote.sql(renderExpireRefreshParentSQL(session.plan, 'aging', oldOperation));
  const expiredParent = await session.expectProblem(
    {
      method: 'POST',
      path: '/v1/auth/refresh',
      body: {
        refreshToken: initial.refresh.token,
        operationId: oldOperation,
        attempt: 1,
      },
    },
    401,
    'credential_expired',
  );
  const continuedOperation = session.newOperationID('aging-continued');
  const continued = await session.refresh('aging', {
    operationID: continuedOperation,
    attempt: 0,
    token: rotated.refresh.token,
  });
  await session.verifyManagement(continued.userSession.token);
  await session.verifyUploader(continued.uploader.token);
  const facts = await durableFacts(session, 'aging', [oldOperation, continuedOperation]);
  if (!facts.operations.some(item => item.operationId === continuedOperation && item.attempt === 0)) {
    throw new Error('Expired predecessor recovery revoked the current family');
  }

  return {
    expiredParent,
    currentFamilyContinued: true,
    latestCheckpoint: session.sender('aging').latestRefresh,
  };
}

export async function refreshLogoutRace(session) {
  const initial = snapshot(session.sender('race'));
  const operationID = session.newOperationID('race-refresh');
  const [refreshed, loggedOut] = await Promise.all([
    settle(session.refreshAttempt('race', {
      operationID,
      attempt: 0,
      token: initial.refresh.token,
    })),
    settle(session.request({
      method: 'POST',
      path: '/v1/auth/logout',
      token: initial.user_session.token,
    })),
  ]);
  if (loggedOut.error) throw loggedOut.error;
  if (loggedOut.value.status !== 204) {
    throw new Error(`Logout racing refresh returned ${loggedOut.value.status}`);
  }
  if (loggedOut.value.cacheControl !== 'no-store') {
    throw new Error('Logout racing refresh was cacheable');
  }
  let current;
  if (refreshed.error) {
    if (!(refreshed.error instanceof SmokeProblem)
      || refreshed.error.status !== 401
      || refreshed.error.code !== 'credential_revoked') {
      throw refreshed.error;
    }
  } else {
    if (refreshed.value.status !== 200) {
      throw new Error(`Refresh racing logout returned ${refreshed.value.status}`);
    }
    current = session.acceptRefresh('race', refreshed.value.body, { operationID, attempt: 0 });
  }

  const revoked = {
    management: await session.expectProblem(
      { path: '/v1/self/receivers', token: initial.user_session.token },
      401,
      'credential_invalid',
    ),
    refresh: await session.expectProblem(
      {
        method: 'POST',
        path: '/v1/auth/refresh',
        body: {
          refreshToken: initial.refresh.token,
          operationId: session.newOperationID('race-after-logout'),
          attempt: 0,
        },
      },
      401,
      'credential_revoked',
    ),
  };
  if (current) {
    revoked.rotatedManagement = await session.expectProblem(
      { path: '/v1/self/receivers', token: current.userSession.token },
      401,
      'credential_invalid',
    );
    revoked.rotatedUploader = await session.expectProblem(
      {
        path: `/v1/messages/smoke-race-${session.plan.runId}`,
        token: current.uploader.token,
      },
      401,
      'credential_invalid',
    );
    revoked.rotatedRefresh = await session.expectProblem(
      {
        method: 'POST',
        path: '/v1/auth/refresh',
        body: {
          refreshToken: current.refresh.token,
          operationId: session.newOperationID('race-after-rotated-logout'),
          attempt: 0,
        },
      },
      401,
      'credential_revoked',
    );
  }
  const facts = await durableFacts(session, 'race', current ? [operationID] : []);
  if (facts.registrationState !== 'active') {
    throw new Error('Refresh/logout race permanently closed the Sender registration');
  }
  if (facts.activeCredentials !== 0) {
    throw new Error('Refresh/logout race left Sender credentials active');
  }

  return {
    refreshCommittedFirst: Boolean(current),
    logoutStatus: loggedOut.value.status,
    refreshRejected: refreshed.error ? {
      status: refreshed.error.status,
      code: refreshed.error.code,
    } : null,
    revoked,
  };
}

export { orderedAttempts };
