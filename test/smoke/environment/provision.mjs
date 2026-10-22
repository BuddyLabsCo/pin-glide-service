import path from 'node:path';

import { quote } from '../support/shell.mjs';

const smokeEnvironmentKeys = [
  'COMPOSE_PROJECT_NAME',
  'PIN_GLIDE_API_BIND',
  'PIN_GLIDE_SNAPSHOT_PATH',
];

export function applySmokeEnvironment(source, target) {
  const desired = {
    COMPOSE_PROJECT_NAME: target.project,
    PIN_GLIDE_API_BIND: target.hostIP,
    PIN_GLIDE_SNAPSHOT_PATH: target.snapshotDir,
  };
  const lines = String(source).replace(/\r\n?/g, '\n').split('\n');
  const seen = new Set();
  const managed = new Set(smokeEnvironmentKeys);
  const output = [];
  for (const line of lines) {
    const match = line.match(/^[ \t]*(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*=/);
    if (!match) {
      output.push(line);
      continue;
    }
    if (managed.has(match[1]) && seen.has(match[1])) {
      throw new Error(`Smoke deployment .env contains duplicate ${match[1]}`);
    }
    if (managed.has(match[1])) seen.add(match[1]);
    output.push(Object.hasOwn(desired, match[1]) ? `${match[1]}=${desired[match[1]]}` : line);
  }
  for (const name of smokeEnvironmentKeys) {
    if (Object.hasOwn(desired, name) && !seen.has(name)) output.push(`${name}=${desired[name]}`);
  }
  while (output.length > 0 && output.at(-1) === '') output.pop();
  return `${output.join('\n')}\n`;
}

export function installationAction(currentRelease, targetRelease, origins = []) {
  if (!currentRelease) return 'initial';
  if (!/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.test(currentRelease)) {
    throw new Error(`Smoke database Release ${currentRelease} is not X.Y.Z`);
  }
  if (currentRelease === targetRelease) return 'redeploy';
  if (!origins.includes(currentRelease)) {
    throw new Error(
      `Smoke database Release ${currentRelease} is not a declared origin for ${targetRelease}`,
    );
  }
  return 'upgrade';
}

async function writeEnvironment(remote, content) {
  const directory = remote.target.deploymentDir;
  const filename = `${directory}/.env`;
  await remote.exec(`
set -eu
temporary=$(sudo -n mktemp ${quote(`${directory}/.env.`)}XXXXXXXX)
trap 'sudo -n rm -f "$temporary"' EXIT
sudo -n chmod 600 "$temporary"
sudo -n tee "$temporary" >/dev/null
sudo -n mv -f "$temporary" ${quote(filename)}
trap - EXIT
`, { input: content, timeoutMs: 30_000 });
}

async function readEnvironment(remote) {
  return (await remote.exec(`sudo -n cat ${quote(`${remote.target.deploymentDir}/.env`)}`, {
    timeoutMs: 30_000,
  })).stdout;
}

async function installFiles(remote, root) {
  const directory = remote.target.deploymentDir;
  await remote.exec(`
set -eu
uid=$(id -u)
gid=$(id -g)
sudo -n install -d -o "$uid" -g "$gid" -m 0775 ${quote(remote.target.snapshotDir)}
sudo -n install -d -m 700 ${quote(directory)}
sudo -n install -m 600 ${quote(path.join(root, 'deploy/compose/docker-compose.yml'))} ${quote(`${directory}/docker-compose.yml`)}
sudo -n install -m 700 ${quote(path.join(root, 'install.sh'))} ${quote(`${directory}/install.sh`)}
if ! sudo -n test -f ${quote(`${directory}/.env`)}; then
  sudo -n install -m 600 ${quote(path.join(root, 'deploy/compose/.env.example'))} ${quote(`${directory}/.env`)}
fi
`, { timeoutMs: 120_000 });
}

async function databaseRelease(remote) {
  const query = statement => remote.exec(
    `${remote.compose} exec -T postgres psql -X -v ON_ERROR_STOP=1 -A -t -U pin_glide -d pin_glide ` +
      `-c ${quote(statement)}`,
    { timeoutMs: 30_000 },
  );
  const initialized = (await query(
    "SELECT to_regclass('release.release_deploy_history') IS NOT NULL",
  )).stdout.trim();
  if (initialized === 'f') return '';
  if (initialized !== 't') throw new Error('Could not determine whether the smoke database is initialized');
  const release = (await query(
    "SELECT coalesce((SELECT release_version FROM release.release_deploy_history ORDER BY id DESC LIMIT 1), '')",
  )).stdout.trim();
  if (!release) throw new Error('The smoke database has no recorded Database Release');
  return release;
}

export async function ensureSmokeDeployment(remote, { root, release, origins = [] }) {
  await installFiles(remote, root);
  const environment = await readEnvironment(remote);
  const smokeEnvironment = applySmokeEnvironment(environment, remote.target);
  if (smokeEnvironment !== environment) {
    await writeEnvironment(remote, smokeEnvironment);
  }

  const volume = `${remote.target.project}_pin-glide-postgres-data`;
  const present = (await remote.exec(
    `if sudo -n docker volume inspect ${quote(volume)} >/dev/null 2>&1; then echo present; else echo absent; fi`,
    { timeoutMs: 30_000 },
  )).stdout.trim() === 'present';

  let currentRelease = '';
  if (present) {
    await remote.exec(`${remote.compose} up -d --no-deps --wait postgres`, {
      timeoutMs: 180_000,
    });
    currentRelease = await databaseRelease(remote);
  }
  const action = installationAction(currentRelease, release, origins);

  if (action === 'initial') {
    await remote.install('--prep --initial-install', release);
    await remote.install('--initial-deploy', release);
  } else {
    await remote.install('--prep', release);
    await remote.install('--full-release', release);
  }
  return { action, previousRelease: currentRelease || null };
}
