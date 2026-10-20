import { sqlTextArray } from './fixtures.mjs';

export async function verifyDiagnosticRedaction(session) {
  const requestIds = [...new Set(session.requestIds)];
  if (requestIds.length === 0) throw new Error('No run-owned requests were available to inspect');
  let records = [];
  for (let attempt = 0; attempt < 100; attempt++) {
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
    const seen = new Set(records.map(record => record.requestId));
    if (requestIds.every(requestId => seen.has(requestId))) break;
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  const seen = new Set(records.map(record => record.requestId));
  const missing = requestIds.filter(requestId => !seen.has(requestId));
  if (missing.length > 0) {
    throw new Error(`Service diagnostics are incomplete for ${missing.length} run-owned requests`);
  }
  const serialized = JSON.stringify(records);
  for (const secret of session.secretValues()) {
    if (serialized.includes(secret)) throw new Error('A raw credential appeared in service diagnostics');
  }
  return { records: records.length, scannedRequestIds: requestIds.length };
}
