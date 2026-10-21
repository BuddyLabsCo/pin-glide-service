import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

import { runModelScenarios } from './lib/model.mjs';
import { ModelFaults } from './lib/model_faults.mjs';
import {
  BailianPeer,
  BailianProblem,
  validateModelCredential,
  waitForProviderExpiry,
} from './lib/model_provider.mjs';

const model = Object.freeze({
  id: 'default',
  workspaceId: 'ws-kea2u5v9va6wsdvh',
  model: 'qwen3.7-flash',
  enabled: true,
});

const responseFormat = {
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
};

function credentialBody({ expiresAt = '2026-10-21T00:10:00Z' } = {}) {
  return {
    credential: {
      apiKey: 'st-smoke-temporary-key',
      expiresAt,
    },
    configuration: {
      provider: 'bailian',
      region: 'cn-beijing',
      baseUrl: `https://${model.workspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1`,
      inference: {
        model: model.model,
        enable_thinking: false,
        response_format: structuredClone(responseFormat),
      },
    },
  };
}

test('a model credential is one enabled snapshot with the provider actual lifetime', () => {
  const issuedAt = new Date('2026-10-21T00:00:00Z');
  const issued = validateModelCredential(credentialBody(), model, issuedAt);
  assert.equal(issued.apiKey, 'st-smoke-temporary-key');
  assert.equal(issued.expiresAt, '2026-10-21T00:10:00.000Z');
  assert.equal(issued.lifetimeSeconds, 600);
  assert.deepEqual(issued.configuration, credentialBody().configuration);

  const mutations = [
    body => { body.configuration.provider = 'another-provider'; },
    body => { body.configuration.region = 'cn-hangzhou'; },
    body => { body.configuration.baseUrl = 'https://example.invalid/compatible-mode/v1'; },
    body => { body.configuration.inference.model = 'another-model'; },
    body => { body.configuration.inference.enable_thinking = true; },
    body => { body.configuration.inference.response_format.type = 'json_object'; },
    body => { body.configuration.inference.response_format.json_schema.strict = false; },
    body => {
      body.configuration.inference.response_format.json_schema.schema.properties.extra = {
        type: 'string',
      };
    },
    body => { body.configuration.inference.response_format.json_schema.schema.required = []; },
    body => { body.configuration.inference.response_format.json_schema.schema.additionalProperties = true; },
    body => { body.credential.expiresAt = '2026-10-21T00:20:00Z'; },
    body => { body.credential.expiresAt = '2026-10-20T23:59:59Z'; },
    body => { body.credential.expiresAt = '2026-10-21T08:00:00+08:00'; },
    body => { body.credential.apiKey = ''; },
    body => { body.parentKey = 'must-not-be-returned'; },
  ];
  for (const mutate of mutations) {
    const body = credentialBody();
    mutate(body);
    assert.throws(() => validateModelCredential(body, model, issuedAt));
  }
});

test('direct Bailian inference sends text only with strict nullable OTP output', async () => {
  const issued = validateModelCredential(
    credentialBody(),
    model,
    new Date('2026-10-21T00:00:00Z'),
  );
  const calls = [];
  const outputs = ['{"otp":"481927"}', '{"otp":null}'];
  const peer = new BailianPeer({
    fetchImpl: async (url, options) => {
      calls.push({ url, options, body: JSON.parse(options.body) });
      return new Response(JSON.stringify({
        id: `provider-request-${calls.length}`,
        model: 'qwen3.7-flash',
        choices: [{
          finish_reason: 'stop',
          message: { content: outputs.shift(), reasoning_content: '' },
        }],
      }), {
        status: 200,
        headers: {
          'content-type': 'application/json',
          ...(calls.length === 1 ? { 'x-request-id': 'provider-1' } : {}),
        },
      });
    },
  });

  const otpSMS = 'Pin Glide smoke: your verification code is 481927. Do not share it.';
  assert.deepEqual(
    await peer.infer({ apiKey: issued.apiKey, configuration: issued.configuration, sms: otpSMS }),
    { otp: '481927', requestId: 'provider-1', finishReason: 'stop' },
  );
  assert.deepEqual(
    await peer.infer({
      apiKey: issued.apiKey,
      configuration: issued.configuration,
      sms: 'Pin Glide smoke: this reminder contains no verification code.',
    }),
    { otp: null, requestId: 'provider-request-2', finishReason: 'stop' },
  );

  assert.equal(calls.length, 2);
  for (const call of calls) {
    assert.equal(
      call.url,
      'https://ws-kea2u5v9va6wsdvh.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/chat/completions',
    );
    assert.equal(call.options.headers.authorization, 'Bearer st-smoke-temporary-key');
    assert.equal(typeof call.body.messages[0].content, 'string');
    assert.equal(call.body.model, 'qwen3.7-flash');
    assert.equal(call.body.enable_thinking, false);
    assert.deepEqual(call.body.response_format, responseFormat);
    assert.deepEqual(Object.keys(call.body).sort(), [
      'enable_thinking',
      'messages',
      'model',
      'response_format',
    ]);
  }
  assert(calls[0].body.messages[0].content.includes(otpSMS));
});

test('Bailian inference rejects a malformed or reasoning response without exposing its body', async () => {
  const issued = validateModelCredential(
    credentialBody(),
    model,
    new Date('2026-10-21T00:00:00Z'),
  );
  for (const message of [
    { content: '{"otp":"481927","extra":true}' },
    { content: '{"otp":"481927"}', reasoning_content: 'hidden reasoning' },
  ]) {
    const peer = new BailianPeer({
      fetchImpl: async () => new Response(JSON.stringify({
        id: 'provider-bad',
        model: 'qwen3.7-flash',
        choices: [{ finish_reason: 'stop', message }],
      }), { status: 200 }),
    });
    await assert.rejects(
      () => peer.infer({
        apiKey: issued.apiKey,
        configuration: issued.configuration,
        sms: 'Your verification code is 481927.',
      }),
      error => error instanceof BailianProblem
        && !error.message.includes('481927')
        && !error.message.includes('hidden reasoning'),
    );
  }
});

test('actual expiry waits for the provider answer without changing clocks', async () => {
  let slept;
  let current = Date.parse('2026-10-21T00:09:59.000Z');
  const result = await waitForProviderExpiry('2026-10-21T00:10:00.000Z', {
    now: () => current,
    sleep: async milliseconds => {
      slept = milliseconds;
      current += milliseconds;
    },
  });
  assert.equal(slept, 3_000);
  assert.deepEqual(result, {
    expiresAt: '2026-10-21T00:10:00.000Z',
    observedAt: '2026-10-21T00:10:02.000Z',
    waitedMs: 3_000,
  });
});

test('controlled model configuration faults always restore the release row', async () => {
  const statements = [];
  const remote = {
    async sql(statement) {
      statements.push(statement);
    },
    async withInvalidBailianKey(callback) {
      return callback();
    },
    async withBailianConnectivityFault(_workspaceID, callback) {
      return callback();
    },
  };
  const faults = new ModelFaults({ remote, model });

  await assert.rejects(
    () => faults.withDisabled(async () => {
      assert.match(statements.at(-1), /enabled\s*=\s*false/);
      throw new Error('disabled configuration observed');
    }),
    /disabled configuration observed/,
  );
  assert.match(statements.at(-1), /INSERT INTO pin_glide\.model_configurations/);
  assert.match(statements.at(-1), /'ws-kea2u5v9va6wsdvh'/);
  assert.match(statements.at(-1), /'qwen3\.7-flash'/);
  assert.match(statements.at(-1), /true/);

  statements.length = 0;
  await faults.withMissing(async () => {
    assert.match(statements.at(-1), /DELETE FROM pin_glide\.model_configurations/);
  });
  assert.match(statements.at(-1), /ON CONFLICT \(id\) DO UPDATE/);

  let applied = 0;
  await faults.withInvalidParentKey(async () => {
    applied++;
  });
  await faults.withConnectivity(async () => {
    applied++;
  });
  assert.equal(applied, 2);
});

test('model configuration restoration uses the release Data Script shape', async () => {
  const script = `
import { pathToFileURL } from 'node:url';
const faults = await import(pathToFileURL(process.env.FAULTS).href);
const statements = [];
const remote = {
  async sql(statement) { statements.push(statement); },
};
const model = {
  id: 'default',
  workspaceId: 'ws-kea2u5v9va6wsdvh',
  model: 'qwen3.7-flash',
  enabled: true,
};
const fault = new faults.ModelFaults({ remote, model });
await fault.withDisabled(async () => {});
await fault.withMissing(async () => {});
process.stdout.write(JSON.stringify(statements));
`;
  const result = spawnSync('node', ['--input-type=module', '--eval', script], {
    env: {
      ...process.env,
      FAULTS: new URL('./lib/model_faults.mjs', import.meta.url).pathname,
    },
    encoding: 'utf8',
  });
  assert.equal(result.status, 0, result.stderr);
  const statements = JSON.parse(result.stdout);
  assert.equal(statements.length, 4);
  for (const statement of statements.filter(value => value.includes('INSERT INTO'))) {
    assert.match(statement, /ON CONFLICT \(id\) DO UPDATE/);
    assert.match(statement, /'ws-kea2u5v9va6wsdvh'/);
    assert.match(statement, /'qwen3\.7-flash'/);
    assert.match(statement, /true/);
  }
});

test('the model smoke journey uses normal upload authority and restores every fault', async () => {
  const modelSender = { id: 'sender-model', label: 'model', accountID: 'account-a' };
  const plan = {
    runId: 'model-run',
    senders: [
      modelSender,
      { id: 'sender-expired', label: 'model-expired', accountID: 'account-a' },
      { id: 'sender-revoked', label: 'model-revoked', accountID: 'account-a' },
    ],
  };
  const requestIds = [];
  const evidenceEntries = [];
  const secrets = [];
  const credentialRequests = [];
  let fault = '';
  let faultCount = 0;
  let recoveryIssued = false;
  let expiredInference = false;
  let recoveryInferences = 0;
  let waitedFor = '';
  const peer = {
    async infer({ apiKey, configuration, sms }) {
      assert.equal(apiKey, 'st-smoke-temporary-key');
      assert.equal(configuration.inference.enable_thinking, false);
      if (expiredInference) {
        throw new BailianProblem('expired', { status: 401, requestId: 'provider-expired' });
      }
      if (recoveryIssued) recoveryInferences++;
      if (sms.includes('481927')) return { otp: '481927', requestId: 'provider-otp', finishReason: 'stop' };
      return { otp: null, requestId: 'provider-null', finishReason: 'stop' };
    },
  };
  const session = {
    plan,
    requestIds,
    evidence: {
      record(entry) {
        evidenceEntries.push(entry);
      },
    },
    sender(label) {
      return plan.senders.find(sender => sender.id.endsWith(label));
    },
    token(label, scope) {
      return `${label}-${scope}`;
    },
    addSecret(secret) {
      secrets.push(secret);
    },
    async request(request) {
      assert.equal(request.path, '/v1/model/credentials');
      credentialRequests.push(request);
      requestIds.push(recoveryIssued ? 'request-model-recovery' : 'request-model-initial');
      recoveryIssued = true;
      return {
        status: 200,
        requestId: requestIds.at(-1),
        cacheControl: 'no-store',
        body: credentialBody(),
      };
    },
    async expectProblem(request, status, code) {
      assert.equal(request.path, '/v1/model/credentials');
      if (request.token === 'model-user_session') {
        assert.equal(status, 401);
        assert.equal(code, 'credential_invalid');
      } else if (request.token === 'model-refresh') {
        assert.equal(status, 401);
        assert.equal(code, 'credential_invalid');
      } else if (request.token === 'model-expired-uploader') {
        assert.equal(status, 401);
        assert.equal(code, 'credential_expired');
      } else if (request.token === 'model-revoked-uploader') {
        assert.equal(status, 401);
        assert.equal(code, 'credential_revoked');
      } else if (fault === 'disabled' || fault === 'missing') {
        assert.equal(status, 503);
        assert.equal(code, 'model_configuration_unavailable');
      } else if (fault === 'invalid-parent' || fault === 'connectivity') {
        assert.equal(status, 502);
        assert.equal(code, 'model_provider_error');
      } else {
        assert.fail(`unexpected refusal for ${request.token} in ${fault}`);
      }
      requestIds.push(`request-${fault || code}`);
      return { status, code, requestId: requestIds.at(-1) };
    },
    remote: {
      async sqlJSON() {
        return { ...model };
      },
      async sql() {},
    },
  };
  const faults = {
    async withDisabled(callback) {
      fault = 'disabled';
      try {
        return await callback();
      } finally {
        fault = '';
        faultCount++;
      }
    },
    async withMissing(callback) {
      fault = 'missing';
      try {
        return await callback();
      } finally {
        fault = '';
        faultCount++;
      }
    },
    async withInvalidParentKey(callback) {
      fault = 'invalid-parent';
      try {
        return await callback();
      } finally {
        fault = '';
        faultCount++;
      }
    },
    async withConnectivity(callback) {
      fault = 'connectivity';
      try {
        return await callback();
      } finally {
        fault = '';
        faultCount++;
      }
    },
  };

  const result = await runModelScenarios({
    session,
    model,
    peer,
    faults,
    otp: '481927',
    now: () => new Date('2026-10-21T00:00:00Z'),
    wait: async expiresAt => {
      waitedFor = expiresAt;
      expiredInference = true;
      return {
        expiresAt,
        observedAt: '2026-10-21T00:10:01.000Z',
        waitedMs: 601_000,
      };
    },
  });

  assert.equal(credentialRequests.length, 2);
  assert.equal(credentialRequests[0].token, 'model-uploader');
  assert.equal(credentialRequests[1].token, 'model-uploader');
  assert.deepEqual(credentialRequests.map(request => request.path), [
    '/v1/model/credentials',
    '/v1/model/credentials',
  ]);
  assert.equal(faultCount, 4);
  assert.equal(waitedFor, '2026-10-21T00:10:00.000Z');
  assert.deepEqual(secrets, [
    'sk-smoke-invalid-parent-key',
    '481927',
    'Pin Glide smoke: your verification code is 481927. Do not share it.',
    'Pin Glide smoke: this reminder contains no verification code.',
    'st-smoke-temporary-key',
    'st-smoke-temporary-key',
  ]);
  assert.equal(result.messagesUploaded, 0);
  assert(recoveryInferences >= 1);
  assert.equal(result.expiry.status, 401);
  assert.equal(result.expiry.requestId, 'provider-expired');
  assert.deepEqual(
    evidenceEntries.map(entry => entry.scenario),
    [
      'model credential snapshot and configured provider profile',
      'real Bailian text-only strict OTP inference',
      'model credential authority refusals',
      'model configuration and provider fault restoration',
      'actual temporary-key expiry',
    ],
  );
  assert(!JSON.stringify(evidenceEntries).includes('st-smoke-temporary-key'));
  assert(!JSON.stringify(evidenceEntries).includes('481927'));
  assert(!JSON.stringify(evidenceEntries).includes('Pin Glide smoke:'));
});
