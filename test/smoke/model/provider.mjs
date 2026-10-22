import { randomUUID } from 'node:crypto';

import { sameJSON } from '../support/json.mjs';

const responseFormat = Object.freeze({
  type: 'json_schema',
  json_schema: {
    name: 'otp_extraction',
    strict: true,
    schema: {
      type: 'object',
      properties: { otp: { type: ['string', 'null'] } },
      required: ['otp'],
      additionalProperties: false,
    },
  },
});

export const requestedLifetimeSeconds = 600;
const minimumLifetimeSeconds = 590;
const maximumLifetimeSeconds = requestedLifetimeSeconds + 5;

function exactKeys(value, expected, label) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`${label} is not an object`);
  }
  const actual = Object.keys(value).sort();
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    throw new Error(`${label} does not have the published fields`);
  }
}

export function validateModelCredential(payload, model, issuedAt = new Date()) {
  exactKeys(payload, ['configuration', 'credential'], 'Model credential');
  exactKeys(payload.credential, ['apiKey', 'expiresAt'], 'Model provider key');
  exactKeys(
    payload.configuration,
    ['baseUrl', 'inference', 'provider', 'region'],
    'Model configuration',
  );
  exactKeys(
    payload.configuration.inference,
    ['enable_thinking', 'model', 'response_format'],
    'Model inference',
  );
  exactKeys(
    payload.configuration.inference.response_format,
    ['json_schema', 'type'],
    'Model response format',
  );
  exactKeys(
    payload.configuration.inference.response_format.json_schema,
    ['name', 'schema', 'strict'],
    'Model JSON Schema wrapper',
  );
  exactKeys(
    payload.configuration.inference.response_format.json_schema.schema,
    ['additionalProperties', 'properties', 'required', 'type'],
    'Model OTP schema',
  );
  exactKeys(
    payload.configuration.inference.response_format.json_schema.schema.properties,
    ['otp'],
    'Model OTP properties',
  );
  exactKeys(
    payload.configuration.inference.response_format.json_schema.schema.properties.otp,
    ['type'],
    'Model OTP field',
  );

  if (typeof payload.credential.apiKey !== 'string' || payload.credential.apiKey === '') {
    throw new Error('Model provider key is empty');
  }
  if (
    model?.enabled !== true
    || payload.configuration.provider !== 'bailian'
    || payload.configuration.region !== 'cn-beijing'
    || payload.configuration.baseUrl
      !== `https://${model.workspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1`
    || payload.configuration.inference.model !== model.model
    || payload.configuration.inference.enable_thinking !== false
    || !sameJSON(payload.configuration.inference.response_format, responseFormat)
  ) {
    throw new Error('Model credential does not carry the one enabled provider snapshot');
  }

  const expiry = Date.parse(payload.credential.expiresAt);
  const lifetimeSeconds = (expiry - issuedAt.getTime()) / 1000;
  if (
    !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/.test(
      payload.credential.expiresAt,
    )
    || !Number.isFinite(expiry)
    || expiry <= issuedAt.getTime()
    || lifetimeSeconds < minimumLifetimeSeconds
    || lifetimeSeconds > maximumLifetimeSeconds
  ) {
    throw new Error('Model provider key does not carry its actual 600-second lifetime');
  }

  return {
    apiKey: payload.credential.apiKey,
    expiresAt: new Date(expiry).toISOString(),
    lifetimeSeconds,
    configuration: structuredClone(payload.configuration),
  };
}

export class BailianProblem extends Error {
  constructor(message, { status = 0, requestId = '' } = {}) {
    super(message);
    this.name = 'BailianProblem';
    this.status = status;
    this.requestId = requestId;
  }
}

function extractionPrompt(sms) {
  return [
    'Extract the verification code from the SMS below.',
    'Return the one-time password as a string, or null when the SMS contains no verification code.',
    'Do not infer or invent a code.',
    '',
    'SMS:',
    sms,
  ].join('\n');
}

function parseOTP(content, sms) {
  let output;
  try {
    output = JSON.parse(content);
  } catch {
    throw new BailianProblem('Bailian returned non-JSON extraction output');
  }
  try {
    exactKeys(output, ['otp'], 'Bailian extraction output');
  } catch {
    throw new BailianProblem('Bailian returned output outside the strict OTP schema');
  }
  if (output.otp !== null && (typeof output.otp !== 'string' || output.otp === '')) {
    throw new BailianProblem('Bailian returned an invalid OTP field');
  }
  if (typeof output.otp === 'string' && !sms.includes(output.otp)) {
    throw new BailianProblem('Bailian returned an OTP outside the original SMS');
  }
  return output.otp;
}

export class BailianPeer {
  constructor({ fetchImpl = fetch, timeoutMs = 120_000 } = {}) {
    this.fetchImpl = fetchImpl;
    this.timeoutMs = timeoutMs;
  }

  async infer({ apiKey, configuration, sms }) {
    if (typeof sms !== 'string' || sms === '') {
      throw new BailianProblem('Bailian inference needs original SMS text');
    }
    const endpoint = `${configuration.baseUrl.replace(/\/$/, '')}/chat/completions`;
    const requestID = randomUUID();
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    timer.unref();
    try {
      let response;
      try {
        response = await this.fetchImpl(endpoint, {
          method: 'POST',
          headers: {
            accept: 'application/json',
            authorization: `Bearer ${apiKey}`,
            'content-type': 'application/json',
            'x-request-id': requestID,
          },
          body: JSON.stringify({
            model: configuration.inference.model,
            enable_thinking: false,
            response_format: configuration.inference.response_format,
            messages: [{ role: 'user', content: extractionPrompt(sms) }],
          }),
          signal: controller.signal,
        });
      } catch (error) {
        throw new BailianProblem(`Bailian inference could not be reached: ${error.message}`);
      }

      let providerRequestID = response.headers.get('x-request-id') || requestID;
      if (!response.ok) {
        throw new BailianProblem(
          `Bailian inference returned HTTP ${response.status}`,
          { status: response.status, requestId: providerRequestID },
        );
      }

      let payload;
      try {
        payload = JSON.parse(await response.text());
      } catch {
        throw new BailianProblem(
          'Bailian inference returned a non-JSON response',
          { status: response.status, requestId: providerRequestID },
        );
      }
      if (
        !response.headers.get('x-request-id')
        && typeof payload?.id === 'string'
        && payload.id !== ''
      ) {
        providerRequestID = payload.id;
      }
      const choice = payload?.choices?.[0];
      if (
        payload?.model !== configuration.inference.model
        || choice?.finish_reason !== 'stop'
        || typeof choice?.message?.content !== 'string'
      ) {
        throw new BailianProblem(
          'Bailian inference did not return the configured model result',
          { status: response.status, requestId: providerRequestID },
        );
      }
      if (
        choice.message.reasoning_content !== undefined
        && choice.message.reasoning_content !== null
        && choice.message.reasoning_content !== ''
      ) {
        throw new BailianProblem(
          'Bailian inference returned reasoning while thinking was disabled',
          { status: response.status, requestId: providerRequestID },
        );
      }

      return {
        otp: parseOTP(choice.message.content, sms),
        requestId: providerRequestID,
        finishReason: choice.finish_reason,
      };
    } finally {
      clearTimeout(timer);
    }
  }
}

function defaultSleep(milliseconds) {
  return new Promise(resolve => setTimeout(resolve, milliseconds));
}

export async function waitForProviderExpiry(
  expiresAt,
  { now = Date.now, sleep = defaultSleep } = {},
) {
  const expiry = Date.parse(expiresAt);
  if (!Number.isFinite(expiry)) throw new Error('Provider expiry is not a timestamp');
  const target = expiry + 2_000;
  const startedAt = now();
  const remaining = Math.max(0, target - startedAt);
  if (remaining > (maximumLifetimeSeconds + 2) * 1000) {
    throw new Error('Provider expiry is too far away for one smoke run');
  }
  if (remaining > 0) await sleep(remaining);
  return {
    expiresAt: new Date(expiry).toISOString(),
    observedAt: new Date(now()).toISOString(),
    waitedMs: remaining,
  };
}
