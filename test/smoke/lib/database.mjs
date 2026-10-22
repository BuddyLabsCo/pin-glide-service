export const retention = new Map([
  ['pin_glide/delivery_deliveries', { days: 7, column: 'created_at' }],
  ['pin_glide/identity_credentials', { days: 7, column: 'expires_at' }],
  ['pin_glide/identity_refresh_operations', { days: 7, column: 'expires_at' }],
  ['pin_glide/pairing_sessions', { days: 7, column: 'expires_at' }],
  ['pin_glide/service_diagnostics', { days: 30, column: 'occurred_at' }],
  ['release/release_schema_audits', { days: 30, column: 'executed_at' }],
  ['cron/job_run_details', { days: 30, column: 'end_time' }],
]);

export const cronCommand = 'SELECT pin_glide.pin_glide_fn_cleanup_expired_rows(now())';
export const retentionJobName = 'pin-glide-retention-cleanup';
export const cronTimezones = new Set(['GMT', 'UTC', 'Etc/UTC']);
export const cronJobs = new Map([
  [retentionJobName, {
    username: 'pin_glide',
    database: 'pin_glide',
    schedule: '0 19 * * *',
    command: cronCommand,
  }],
]);

function uniqueMap(rows, key, label) {
  const result = new Map();
  for (const row of rows) {
    const identity = key(row);
    if (result.has(identity)) throw new Error(`${label} ${identity} is duplicated`);
    result.set(identity, row);
  }
  return result;
}

function verifyScripts(actual, expected, label) {
  const current = uniqueMap(actual, row => row.filename, label);
  for (const [filename, checksum] of Object.entries(expected)) {
    if (!current.has(filename)) throw new Error(`${label} ${filename} is missing from the database ledger`);
    if (current.get(filename).checksum !== checksum) {
      throw new Error(`${label} ${filename} does not match the immutable packaged file`);
    }
  }
  for (const filename of current.keys()) {
    if (!(filename in expected)) throw new Error(`${label} ${filename} is not present in the selected release`);
  }
  return current.size;
}

export function validateConfiguration(facts, model, { cronActive = true } = {}) {
  if (
    !model
    || facts.model?.id !== model.id
    || facts.model.workspaceId !== model.workspaceId
    || facts.model.model !== model.model
    || facts.model.enabled !== model.enabled
  ) {
    throw new Error('Enabled model configuration does not match the release Data Script');
  }

  const policies = uniqueMap(facts.retention || [], row => `${row.schema}/${row.table}`, 'retention policy');
  if (policies.size !== retention.size) throw new Error('Retention policies are incomplete or unapproved');
  for (const [identity, expected] of retention) {
    const policy = policies.get(identity);
    if (!policy || policy.days !== expected.days || policy.column !== expected.column) {
      throw new Error(`Retention policy ${identity} does not match the adopted tier`);
    }
  }

  if (!Array.isArray(facts.cron)) throw new Error('Scheduled job inventory is not an array');
  if (!cronTimezones.has(facts.cronTimezone)) {
    throw new Error(`pg_cron timezone ${facts.cronTimezone || '(missing)'} is not UTC`);
  }
  const actualJobs = facts.cron;
  const currentJobs = uniqueMap(actualJobs, row => row.jobname, 'scheduled job');
  if (currentJobs.size !== cronJobs.size) throw new Error('Scheduled job inventory is incomplete or unapproved');
  for (const [jobname, expected] of cronJobs) {
    const job = currentJobs.get(jobname);
    if (!job || job.active !== cronActive) {
      throw new Error(`Scheduled job ${jobname} has the wrong active state`);
    }
    for (const [key, value] of Object.entries(expected)) {
      if (job[key] !== value) throw new Error(`Scheduled job ${jobname} ${key} does not match the adopted value`);
    }
  }
  return {
    retentionPolicies: policies.size,
    cronTimezone: facts.cronTimezone,
    cron: actualJobs.map(job => ({ ...job })),
  };
}

export function validateDatabaseFacts(facts, { release, scripts, origins = null, upgradeOrigin = null, model }) {
  if (facts?.database !== 'pin_glide') throw new Error('Smoke database must be pin_glide');
  const history = validateReleaseHistory(facts, {
    release,
    origins,
    upgradeOrigin,
  });
  const configuration = validateConfiguration(facts, model);

  for (const [identity, exitCode] of Object.entries({
    'release:apply': 0,
    'release:check': 0,
    'pin_glide:apply': 0,
    'pin_glide:check': 0,
  })) {
    if (facts.audits?.[identity] !== exitCode) {
      throw new Error(`${identity} schema check did not converge`);
    }
  }

  const dataScripts = verifyScripts(facts.dataScripts || [], scripts.data, 'data script');
  const migrations = (facts.migrationScripts || []).map(row => ({
    filename: `${row.schema}/${row.filename}`,
    checksum: row.checksum,
  }));
  const migrationScripts = verifyScripts(migrations, scripts.migrations, 'migration script');
  return {
    database: facts.database,
    release: facts.release,
    history: history.versions,
    installation: history.installation,
    upgradeOrigin: history.upgradeOrigin,
    model: { ...facts.model },
    retentionPolicies: configuration.retentionPolicies,
    dataScripts,
    migrationScripts,
    cron: configuration.cron,
  };
}

export function validateReleaseHistory(facts, { release, origins = null, upgradeOrigin = null }) {
  if (facts?.release !== release) throw new Error(`Database Release must be ${release}`);
  if (
    !Array.isArray(facts.history)
    || facts.history.length === 0
    || facts.history.some(version => typeof version !== 'string' || !/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.test(version))
    || facts.history.at(-1) !== release
  ) {
    throw new Error('Release deployment history does not end at the selected release');
  }
  const previous = [...facts.history].reverse().find(version => version !== release);
  if (previous !== undefined && previous !== upgradeOrigin) {
    throw new Error(
      `Release deployment history origin ${previous || 'none'} is not supported; expected ${upgradeOrigin || 'fresh installation'}`,
    );
  }
  if (origins) {
    for (let index = 1; index < facts.history.length; index++) {
      const from = facts.history[index - 1];
      const to = facts.history[index];
      if (from === to || !Object.hasOwn(origins, to)) continue;
      if (!origins[to].includes(from)) {
        throw new Error(`Release deployment history transition ${from} to ${to} is not declared`);
      }
    }
  }
  return {
    versions: [...facts.history],
    installation: previous === undefined ? 'fresh' : 'upgrade',
    upgradeOrigin: previous || null,
  };
}
