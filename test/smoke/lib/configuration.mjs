import fs from 'node:fs';
import path from 'node:path';

const modelScript = 'db/scripts/issue_33_1_model_configuration.sql';

export async function modelConfiguration(root) {
  const sql = await fs.promises.readFile(path.join(root, modelScript), 'utf8');
  const match = sql.match(
    /VALUES\s*\(\s*'default'\s*,\s*'([^']+)'\s*,\s*'([^']+)'\s*,\s*(true|false)\s*\)/i,
  );
  if (!match) throw new Error(`${modelScript} does not declare the approved default model`);
  return {
    id: 'default',
    workspaceId: match[1],
    model: match[2],
    enabled: match[3].toLowerCase() === 'true',
  };
}
