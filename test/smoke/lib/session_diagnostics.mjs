import { sqlTextArray } from './fixtures.mjs';
import { SmokeBlocked } from './http.mjs';

function containsSecret(text, secret) {
  if (secret.length >= 8) return text.includes(secret);
  const escaped = secret.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  return new RegExp(`(^|[^0-9A-Za-z])${escaped}([^0-9A-Za-z]|$)`).test(text);
}

function rejectSecrets(session, label, value) {
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  for (const secret of session.secretValues()) {
    if (containsSecret(text, secret)) {
      throw new Error(`${label} retained a raw credential or payload value`);
    }
  }
}

export async function verifyDiagnosticRedaction(
  session,
  {
    settleMs = 2_000,
    pollMs = 100,
    includeLogs = false,
    artifacts = {},
    databaseRows = null,
  } = {},
) {
  const requestIds = [...new Set(session.requestIds)];
  if (requestIds.length === 0) throw new Error('No run-owned requests were available to inspect');
  let records = [];
  let seen = new Set();
  const attempts = Math.max(1, Math.ceil(settleMs / pollMs));
  for (let attempt = 0; attempt < attempts; attempt++) {
    records = await session.remote.sqlJSON(`
SELECT coalesce(json_agg(
  to_jsonb(diagnostic) || jsonb_build_object('requestId', request_id)
), '[]'::json)
FROM pin_glide.service_diagnostics AS diagnostic
WHERE diagnostic.request_id = ANY(${sqlTextArray(requestIds)})
   OR diagnostic.subject = ANY(${sqlTextArray([...session.subjects])});
`);
    seen = new Set(records.map(record => record.requestId));
    if (requestIds.every(requestId => seen.has(requestId))) break;
    if (attempt + 1 < attempts) {
      await new Promise(resolve => setTimeout(resolve, pollMs));
    }
  }
  const missing = requestIds.filter(requestId => !seen.has(requestId));
  if (records.length === 0) {
    throw new SmokeBlocked('No run-owned service diagnostics were available to inspect');
  }
  rejectSecrets(session, 'service diagnostics', records);
  rejectSecrets(session, 'HTTP errors', session.errorBodies || []);
  if (databaseRows) rejectSecrets(session, 'run-owned database rows', databaseRows);

  if (Object.keys(artifacts).length > 0) {
    rejectSecrets(session, 'public examples or SQL artifacts', artifacts);
  }
  if (includeLogs) {
    if (typeof session.remote.apiLogsSince !== 'function') {
      throw new SmokeBlocked('Service logs are unavailable for the privacy sweep');
    }
    const logs = await session.remote.apiLogsSince(session.startedAt);
    rejectSecrets(session, 'service logs', `${logs.stdout}\n${logs.stderr}`);
  }
  return {
    records: records.length,
    scannedRequestIds: requestIds.filter(requestId => seen.has(requestId)).length,
    missingRequestIds: missing.length,
    logsScanned: includeLogs,
    artifactsScanned: Object.keys(artifacts).length,
    errorBodiesScanned: (session.errorBodies || []).length,
    databaseRowsScanned: databaseRows
      ? Object.values(databaseRows).reduce((count, rows) => count + (rows?.length || 0), 0)
      : 0,
  };
}
