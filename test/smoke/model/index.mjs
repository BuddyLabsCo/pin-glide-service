import { randomInt } from 'node:crypto';

import { escapeSQLString } from '../support/sql.mjs';
import {
  renderExpireAccessSQL,
  renderRevokeSenderRegistrationSQL,
} from '../credentials/fixtures.mjs';
import { sameJSON } from '../support/json.mjs';
import { ModelFaults } from './faults.mjs';
import { invalidParentKey } from '../environment/provider-faults.mjs';
import {
  BailianPeer,
  BailianProblem,
  requestedLifetimeSeconds,
  validateModelCredential,
  waitForProviderExpiry,
} from './provider.mjs';
import { runScenario } from '../harness/scenario.mjs';

const credentialsPath = '/v1/model/credentials';

function expectedModelRow(model) {
  return {
    id: model.id,
    workspaceId: model.workspaceId,
    model: model.model,
    enabled: model.enabled,
  };
}

async function readLiveModel(remote, model) {
  const row = await remote.sqlJSON(`
SELECT json_build_object(
  'id', id,
  'workspaceId', workspace_id,
  'model', model,
  'enabled', enabled
)
FROM pin_glide.model_configurations
WHERE id = ${escapeSQLString(model.id)};
`);
  if (!sameJSON(row, expectedModelRow(model))) {
    throw new Error('The deployed model configuration is not the enabled release snapshot');
  }
  return row;
}

async function issueModelCredential({ session, model, now }) {
  const issuedAt = now();
  const response = await session.request({
    method: 'POST',
    path: credentialsPath,
    token: session.token('model', 'uploader'),
  });
  if (response.status !== 200) {
    throw new Error(`Model credential issuance returned ${response.status}`);
  }
  if (response.cacheControl !== 'no-store') {
    throw new Error('Model credential response was cacheable');
  }
  const issued = validateModelCredential(response.body, model, issuedAt);
  session.addSecret(issued.apiKey);
  return { ...issued, requestId: response.requestId, issuedAt: issuedAt.toISOString() };
}

function credentialRefusal(session, label, scope, status, code) {
  return session.expectProblem(
    {
      method: 'POST',
      path: credentialsPath,
      token: session.token(label, scope),
    },
    status,
    code,
  );
}

async function rejectExpiredInference(peer, issued, otpSMS) {
  try {
    await peer.infer({
      apiKey: issued.apiKey,
      configuration: issued.configuration,
      sms: otpSMS,
    });
  } catch (error) {
    if (error instanceof BailianProblem && [401, 403].includes(error.status)) {
      return { status: error.status, requestId: error.requestId };
    }
    throw error;
  }
  throw new Error('Bailian accepted a temporary key after its actual expiry');
}

export async function runModelScenarios({
  session,
  model,
  peer = new BailianPeer(),
  faults = new ModelFaults({ remote: session.remote, model }),
  otp = String(randomInt(100000, 1_000_000)),
  now = () => new Date(),
  wait = waitForProviderExpiry,
}) {
  let initial;
  let recovered;
  let initialRow;
  let expiry;
  const otpSMS = `Pin Glide smoke: your verification code is ${otp}. Do not share it.`;
  const noOTPSMS = 'Pin Glide smoke: this reminder contains no verification code.';
  for (const value of [invalidParentKey, otp, otpSMS, noOTPSMS]) session.addSecret(value);

  await runScenario(
    session,
    'model credential snapshot and configured provider profile',
    async () => {
      initialRow = await readLiveModel(session.remote, model);
      initial = await issueModelCredential({ session, model, now });
      return {
        liveConfiguration: initialRow,
        requestId: initial.requestId,
        provider: initial.configuration.provider,
        region: initial.configuration.region,
        baseUrl: initial.configuration.baseUrl,
        model: initial.configuration.inference.model,
        enableThinking: initial.configuration.inference.enable_thinking,
        requestedLifetimeSeconds,
        actualExpiresAt: initial.expiresAt,
        actualLifetimeSeconds: initial.lifetimeSeconds,
      };
    },
  );

  await runScenario(
    session,
    'real Bailian text-only strict OTP inference',
    async () => {
      const extracted = await peer.infer({
        apiKey: initial.apiKey,
        configuration: initial.configuration,
        sms: otpSMS,
      });
      if (extracted.otp !== otp || !otpSMS.includes(extracted.otp)) {
        throw new Error('Bailian output did not contain the original synthetic OTP');
      }
      const absent = await peer.infer({
        apiKey: initial.apiKey,
        configuration: initial.configuration,
        sms: noOTPSMS,
      });
      if (absent.otp !== null) {
        throw new Error('Bailian did not return null for an SMS without an OTP');
      }
      return {
        otp: {
          requestId: extracted.requestId,
          finishReason: extracted.finishReason,
          belongsToOriginalSMS: true,
        },
        noOTP: {
          requestId: absent.requestId,
          finishReason: absent.finishReason,
          returnedNull: true,
        },
        textOnly: true,
        enableThinking: false,
        strictJSONSchema: true,
        messagesUploaded: 0,
      };
    },
  );

  await runScenario(
    session,
    'model credential authority refusals',
    async () => {
      const management = await credentialRefusal(
        session,
        'model',
        'user_session',
        401,
        'credential_invalid',
      );
      const refresh = await credentialRefusal(
        session,
        'model',
        'refresh',
        401,
        'credential_invalid',
      );
      await session.remote.sql(renderExpireAccessSQL(session.plan, 'model-expired'));
      const expired = await credentialRefusal(
        session,
        'model-expired',
        'uploader',
        401,
        'credential_expired',
      );
      await session.remote.sql(renderRevokeSenderRegistrationSQL(session.plan, 'model-revoked'));
      const revoked = await credentialRefusal(
        session,
        'model-revoked',
        'uploader',
        401,
        'credential_revoked',
      );
      return { management, refresh, expired, revoked };
    },
  );

  await runScenario(
    session,
    'model configuration and provider fault restoration',
    async () => {
      const disabled = await faults.withDisabled(() => credentialRefusal(
        session,
        'model',
        'uploader',
        503,
        'model_configuration_unavailable',
      ));
      const missing = await faults.withMissing(() => credentialRefusal(
        session,
        'model',
        'uploader',
        503,
        'model_configuration_unavailable',
      ));
      const invalidParent = await faults.withInvalidParentKey(() => credentialRefusal(
        session,
        'model',
        'uploader',
        502,
        'model_provider_error',
      ));
      const connectivity = await faults.withConnectivity(() => credentialRefusal(
        session,
        'model',
        'uploader',
        502,
        'model_provider_error',
      ));
      const restoredRow = await readLiveModel(session.remote, model);
      recovered = await issueModelCredential({ session, model, now });
      const usable = await peer.infer({
        apiKey: recovered.apiKey,
        configuration: recovered.configuration,
        sms: otpSMS,
      });
      if (usable.otp !== otp) {
        throw new Error('The restored temporary key could not perform configured inference');
      }
      return {
        disabled,
        missing,
        invalidParent,
        connectivity,
        restoredConfiguration: sameJSON(restoredRow, initialRow),
        parentKeyRestored: true,
        providerReachableAfterFault: true,
        recoveryInferenceRequestId: usable.requestId,
        recoveryRequestId: recovered.requestId,
      };
    },
  );

  await runScenario(
    session,
    'actual temporary-key expiry',
    async () => {
      const timing = await wait(recovered.expiresAt);
      const refusal = await rejectExpiredInference(peer, recovered, otpSMS);
      expiry = { ...refusal, ...timing };
      return {
        ...timing,
        requestedLifetimeSeconds,
        actualLifetimeSeconds: recovered.lifetimeSeconds,
        refusal,
        providerClockChanged: false,
      };
    },
  );

  return {
    provider: 'bailian',
    region: 'cn-beijing',
    workspaceId: model.workspaceId,
    model: model.model,
    requests: session.requestIds.length,
    messagesUploaded: 0,
    expiry,
  };
}
