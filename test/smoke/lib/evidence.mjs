import fs from 'node:fs';
import path from 'node:path';

const sensitive = /^(?:authorization|token|refreshtoken|receivertoken|pairingtoken|api_?key|parent_?key|password|secret|wechatcode|qqcode)$/i;
const statuses = new Set(['passed', 'failed', 'blocked']);

function redact(value, secrets, key = '') {
  if (sensitive.test(key)) return '[REDACTED]';
  if (typeof value === 'string') {
    let result = value;
    for (const secret of secrets) {
      if (secret) result = result.replaceAll(secret, '[REDACTED]');
    }
    return result;
  }
  if (Array.isArray(value)) return value.map(item => redact(item, secrets));
  if (value && typeof value === 'object') {
    return Object.fromEntries(Object.entries(value).map(([name, item]) => [name, redact(item, secrets, name)]));
  }
  return value;
}

export class Evidence {
  constructor({ runId, secrets = [], exclusions = [] } = {}) {
    this.runId = runId;
    this.secrets = [...new Set(secrets.filter(Boolean))].sort((left, right) => right.length - left.length);
    this.exclusions = [...exclusions];
    this.scenarios = [];
  }

  addSecret(value) {
    if (value && !this.secrets.includes(value)) {
      this.secrets.push(value);
      this.secrets.sort((left, right) => right.length - left.length);
    }
  }

  secretValues() {
    return [...this.secrets];
  }

  record({ scenario, status, requestId = '', httpStatus, code = '', details = {} }) {
    if (!scenario) throw new Error('Smoke evidence needs a scenario name');
    if (!statuses.has(status)) throw new Error(`Smoke evidence status must be passed, failed or blocked, found ${status}`);
    this.scenarios.push({
      scenario,
      status,
      requestId,
      ...(httpStatus === undefined ? {} : { httpStatus }),
      ...(code ? { code } : {}),
      ...(Object.keys(details).length > 0 ? { details } : {}),
    });
  }

  report() {
    return redact({
      version: 1,
      runId: this.runId,
      generatedAt: new Date().toISOString(),
      exclusions: this.exclusions,
      scenarios: this.scenarios,
    }, this.secrets);
  }

  write(filename) {
    const report = this.report();
    fs.mkdirSync(path.dirname(filename), { recursive: true, mode: 0o700 });
    const temporary = `${filename}.${process.pid}.tmp`;
    fs.writeFileSync(temporary, `${JSON.stringify(report, null, 2)}\n`, { mode: 0o600 });
    fs.renameSync(temporary, filename);
    return report;
  }
}

export function redacted(value, secrets = []) {
  return redact(value, secrets);
}
