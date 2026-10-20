import { sqlTextArray } from './fixtures.mjs';

export async function verifyDiagnosticRedaction(
  session,
  { settleMs = 2_000, pollMs = 100 } = {},
) {
  const requestIds = [...new Set(session.requestIds)];
  if (requestIds.length === 0) throw new Error('No run-owned requests were available to inspect');
  let records = [];
  let seen = new Set();
  const attempts = Math.max(1, Math.ceil(settleMs / pollMs));
  for (let attempt = 0; attempt < attempts; attempt++) {
    records = await session.remote.sqlJSON(`
SELECT coalesce(json_agg(json_build_object(
  'requestId', request_id,
  'subject', subject,
  'method', method
)), '[]'::json)
FROM pin_glide.service_diagnostics
WHERE request_id = ANY(${sqlTextArray(requestIds)})
   OR subject = ANY(${sqlTextArray([...session.subjects])});
`);
    seen = new Set(records.map(record => record.requestId));
    if (requestIds.every(requestId => seen.has(requestId))) break;
    if (attempt + 1 < attempts) {
      await new Promise(resolve => setTimeout(resolve, pollMs));
    }
  }
  const missing = requestIds.filter(requestId => !seen.has(requestId));
  if (records.length === 0) throw new Error('No run-owned service diagnostics were available to inspect');
  const serialized = JSON.stringify(records);
  for (const secret of session.secretValues()) {
    if (serialized.includes(secret)) throw new Error('A raw credential appeared in service diagnostics');
  }
  return {
    records: records.length,
    scannedRequestIds: requestIds.filter(requestId => seen.has(requestId)).length,
    missingRequestIds: missing.length,
  };
}
