import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';

const openAPI = 'generated/protocol/openapi.yaml';
const asyncAPI = 'generated/protocol/asyncapi.yaml';
const openapiText = readFileSync(new URL('../../../generated/protocol/openapi.yaml', import.meta.url), 'utf8');
const asyncapiText = readFileSync(new URL('../../../generated/protocol/asyncapi.yaml', import.meta.url), 'utf8');
const digests = {
  openapi: createHash('sha256').update(openapiText).digest('hex'),
  asyncapi: createHash('sha256').update(asyncapiText).digest('hex'),
};

export function operationIdentity(name) {
  if (!openapiText.includes(`operationId: ${name}`)) {
    throw new Error(`Scenario names an unpublished HTTP operation ${name}`);
  }
  return { name, source: openAPI };
}

export function messageIdentity(name, wireName) {
  if (!wireName || !asyncapiText.includes(`name: ${wireName}`)) {
    throw new Error(`Scenario names an unpublished WSS message ${name}`);
  }
  return { name: wireName, source: asyncAPI };
}

export function contractEvidence(session, exercised, wireNames) {
  return {
    openapi: { source: openAPI, digest: digests.openapi },
    asyncapi: { source: asyncAPI, digest: digests.asyncapi },
    http: exercised.http.map(operationIdentity),
    wss: exercised.wss.map(name => messageIdentity(name, wireNames[name])),
    ...(session.target.sourceCommit ? { sourceCommit: session.target.sourceCommit } : {}),
  };
}
