import {
  renderRevokeSenderRegistrationSQL,
  renderSupersedeFamilySQL,
} from './fixtures.mjs';
import { durableFacts, snapshot } from './session_credential_support.mjs';

export async function preExistingRevokedFamily(session) {
  const sender = session.sender('superseded');
  const prior = snapshot(sender);
  await session.remote.sql(renderSupersedeFamilySQL(session.plan, 'superseded'));
  sender.credentials = sender.replacementCredentials;
  session.addSecret(sender.credentials.map(item => item.token));

  const priorManagement = await session.expectProblem(
    { path: '/v1/self/receivers', token: prior.user_session.token },
    401,
    'credential_invalid',
  );
  const priorRefresh = await session.expectProblem(
    {
      method: 'POST',
      path: '/v1/auth/refresh',
      body: {
        refreshToken: prior.refresh.token,
        operationId: session.newOperationID('superseded-prior'),
        attempt: 0,
      },
    },
    401,
    'credential_revoked',
  );
  const currentOperation = session.newOperationID('superseded-current');
  const current = await session.refresh('superseded', {
    operationID: currentOperation,
    attempt: 0,
  });
  await session.verifyManagement(current.userSession.token);
  await session.verifyUploader(current.uploader.token);
  const facts = await durableFacts(session, 'superseded', [currentOperation]);
  if (facts.registrationState !== 'active' || facts.revokedCredentials < 3) {
    throw new Error('Superseded-family fixture did not revoke only the prior family');
  }

  return {
    priorManagement,
    priorRefresh,
    currentFamilyAccepted: true,
    currentOperation: { operationId: currentOperation, attempt: 0 },
    durable: {
      registrationState: facts.registrationState,
      revokedCredentials: facts.revokedCredentials,
      activeCredentials: facts.activeCredentials,
    },
  };
}
export async function permanentAndFailureOutcomes(session) {
  await session.remote.sql(renderRevokeSenderRegistrationSQL(session.plan, 'permanent'));
  const permanent = {
    management: await session.expectProblem(
      { path: '/v1/self/receivers', token: session.token('permanent', 'user_session') },
      401,
      'credential_revoked',
    ),
    uploader: await session.expectProblem(
      { path: `/v1/messages/smoke-permanent-${session.plan.runId}`, token: session.token('permanent', 'uploader') },
      401,
      'credential_revoked',
    ),
    refresh: await session.expectProblem(
      {
        method: 'POST',
        path: '/v1/auth/refresh',
        body: {
          refreshToken: session.token('permanent', 'refresh'),
          operationId: session.newOperationID('permanent-refresh'),
          attempt: 0,
        },
      },
      401,
      'credential_revoked',
    ),
  };
  const invalid = await session.expectProblem(
    { path: '/v1/self/receivers', token: 'credential-that-was-never-issued' },
    401,
    'credential_invalid',
  );
  const foreignOperation = session.newOperationID('foreign-after-permanent');
  const foreign = await session.refresh('foreign', {
    operationID: foreignOperation,
    attempt: 0,
  });
  await session.verifyManagement(foreign.userSession.token);
  await session.verifyUploader(foreign.uploader.token);
  const facts = await durableFacts(session, 'permanent');
  if (facts.registrationState !== 'revoked' || facts.activeCredentials !== 0) {
    throw new Error('Permanent registration fixture did not close every credential');
  }

  return {
    permanent,
    invalid,
    unrelatedSenderPreserved: {
      senderId: session.sender('foreign').id,
      operationId: foreignOperation,
      attempt: 0,
    },
    permanentFacts: {
      registrationState: facts.registrationState,
      activeCredentials: facts.activeCredentials,
      revokedCredentials: facts.revokedCredentials,
    },
  };
}
