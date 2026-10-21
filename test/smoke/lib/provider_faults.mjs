import { withFailureJoiningCleanup } from './cleanup.mjs';
import { quote } from './shell.mjs';

const watchdogAttempts = 24;
const watchdogIntervalSeconds = 5;

function compose(remote, envFileArgument) {
  return [
    'sudo -n docker compose',
    `--project-directory ${quote(remote.target.deploymentDir)}`,
    `--env-file ${envFileArgument}`,
    `-f ${quote(`${remote.target.deploymentDir}/docker-compose.yml`)}`,
  ].join(' ');
}

export function watchdogBody(
  remote,
  {
    attempts = watchdogAttempts,
    intervalSeconds = watchdogIntervalSeconds,
  } = {},
) {
  const envFile = `${remote.target.deploymentDir}/.env`;
  return `#!/bin/sh
guard=$1
attempt=0
while [ -e "$guard" ] && [ "$attempt" -lt ${attempts} ]; do
    sleep ${intervalSeconds}
    attempt=$((attempt + 1))
done
if [ -e "$guard" ]; then
    rm -f "$guard"
    attempt=0
    while [ "$attempt" -lt 3 ]; do
        ${compose(remote, quote(envFile))} up -d --no-deps --force-recreate --wait api && break
        attempt=$((attempt + 1))
        sleep ${intervalSeconds}
    done
fi
rm -f "$0"
`;
}

export function removeHostsLine(marker, hostsFile = '/etc/hosts') {
  const suffix = ` ${marker}`;
  return [
    'set -e',
    'tmp=$(mktemp)',
    `awk -v suffix=${quote(suffix)} 'substr($0, length($0) - length(suffix) + 1) != suffix' ${quote(hostsFile)} > "$tmp"`,
    `cat "$tmp" > ${quote(hostsFile)}`,
    'rm -f "$tmp"',
  ].join('\n');
}

async function startInvalidBailianKeyFault(remote) {
  const envFile = `${remote.target.deploymentDir}/.env`;
  const command = `
set -euo pipefail
umask 077
temporary=
guard=
watchdog=
fault_active=false
cleanup() {
    status=$?
    trap - EXIT
    if [ "$status" -ne 0 ]; then
        if [ "$fault_active" = true ]; then
            ${compose(remote, quote(envFile))} up -d --no-deps --force-recreate --wait api || true
        fi
    fi
    [ -z "$watchdog" ] || rm -f -- "$watchdog"
    [ -z "$guard" ] || rm -f -- "$guard"
    [ -z "$temporary" ] || rm -f -- "$temporary"
    exit "$status"
}
trap cleanup EXIT
temporary=$(mktemp /tmp/pin-glide-dev-smoke-bailian.XXXXXX.env)
guard=$(mktemp /tmp/pin-glide-dev-smoke-bailian-watchdog.XXXXXX)
watchdog="$guard.sh"
printf '%s\\n' ${quote(watchdogBody(remote))} > "$watchdog"
chmod 700 "$watchdog"
nohup "$watchdog" "$guard" >/dev/null 2>&1 &
cp -- ${quote(envFile)} "$temporary"
if grep -q '^BAILIAN_API_KEY=' "$temporary"; then
    sed -i 's|^BAILIAN_API_KEY=.*$|BAILIAN_API_KEY=sk-smoke-invalid-parent-key|' "$temporary"
else
    printf '%s\\n' 'BAILIAN_API_KEY=sk-smoke-invalid-parent-key' >> "$temporary"
fi
fault_active=true
${compose(remote, '"$temporary"')} up -d --no-deps --force-recreate --wait api
rm -f -- "$temporary"
trap - EXIT
printf '%s|%s|%s\\n' "$temporary" "$guard" "$watchdog"
`;
  const result = await remote.ssh(`sudo -n bash -ec ${quote(command)}`, { timeoutMs: 180_000 });
  const line = result.stdout
    .split('\n')
    .map(value => value.trim())
    .find(value => value.split('|').length === 3);
  const [temporaryEnv, guard, watchdog] = (line || '').split('|');
  if (
    !/^\/tmp\/pin-glide-dev-smoke-bailian\.[A-Za-z0-9]+\.env$/.test(temporaryEnv)
    || !/^\/tmp\/pin-glide-dev-smoke-bailian-watchdog\.[A-Za-z0-9]+$/.test(guard)
    || watchdog !== `${guard}.sh`
  ) {
    throw new Error('Invalid parent-key fault did not report its restoration state');
  }
  try {
    const runningEnvironment = await remote.ssh(
      `sudo -n docker inspect "$(${compose(remote, quote(`${remote.target.deploymentDir}/.env`))} ps -q api)" --format '{{json .Config.Env}}'`,
      { timeoutMs: 30_000 },
    );
    if (!runningEnvironment.stdout.includes('BAILIAN_API_KEY=sk-smoke-invalid-parent-key')) {
      throw new Error('Invalid parent-key fault did not reach the running API');
    }
  } catch (error) {
    try {
      await stopInvalidBailianKeyFault(remote, { temporaryEnv, guard, watchdog });
    } catch (cleanupError) {
      throw new AggregateError(
        [error, cleanupError],
        'Invalid parent-key fault verification and restoration both failed',
      );
    }
    throw error;
  }
  return { temporaryEnv, guard, watchdog };
}

async function stopInvalidBailianKeyFault(remote, paths) {
  const { temporaryEnv, guard, watchdog } = paths;
  if (
    !/^\/tmp\/pin-glide-dev-smoke-bailian\.[A-Za-z0-9]+\.env$/.test(temporaryEnv)
    || !/^\/tmp\/pin-glide-dev-smoke-bailian-watchdog\.[A-Za-z0-9]+$/.test(guard)
    || watchdog !== `${guard}.sh`
  ) {
    throw new Error('Refusing to remove unexpected smoke fault files');
  }
  const envFile = `${remote.target.deploymentDir}/.env`;
  const command = `
set +e
${compose(remote, quote(envFile))} up -d --no-deps --force-recreate --wait api
restore_status=$?
if [ "$restore_status" -eq 0 ]; then
    rm -f -- ${quote(guard)} ${quote(watchdog)} ${quote(temporaryEnv)}
fi
exit "$restore_status"
`;
  await remote.ssh(`sudo -n bash -ec ${quote(command)}`, { timeoutMs: 180_000 });
}

export async function withInvalidBailianKey(remote, callback) {
  await remote.preserveApiLogs();
  const paths = await startInvalidBailianKeyFault(remote);
  return withFailureJoiningCleanup(
    async () => {
      try {
        return await callback();
      } finally {
        await remote.preserveApiLogs();
      }
    },
    () => stopInvalidBailianKeyFault(remote, paths),
    'Invalid parent-key fault and environment restoration both failed',
  );
}

export async function withBailianConnectivityFault(
  remote,
  workspaceID,
  callback,
  { idleWaitMs = 95_000 } = {},
) {
  if (!/^[a-z0-9][a-z0-9-]*$/.test(workspaceID)) {
    throw new Error('Refusing a provider fault for an invalid workspace ID');
  }
  await remote.preserveApiLogs();
  const marker = 'pin-glide-smoke-fault';
  const host = `${workspaceID}.cn-beijing.maas.aliyuncs.com`;
  const container = `id=$(${remote.compose} ps -q api); test -n "$id"`;
  const remove = removeHostsLine(marker);
  const add = [
    `printf '%s\\n' ${quote(`127.0.0.1 ${host} # ${marker}`)} >> /etc/hosts`,
    `(sleep 600; ${remove}) >/dev/null 2>&1 &`,
  ].join('\n');
  await remote.ssh(`${container} && sudo -n docker exec -u 0 "$id" sh -ec ${quote(add)}`, {
    timeoutMs: 30_000,
  });
  // Go may reuse a provider connection from before the fault for 90 seconds.
  if (idleWaitMs > 0) {
    await new Promise(resolve => setTimeout(resolve, idleWaitMs));
  }

  return withFailureJoiningCleanup(
    async () => {
      try {
        return await callback();
      } finally {
        await remote.preserveApiLogs();
      }
    },
    () => {
      const restore = [
        remove,
        `! grep -F ${quote(marker)} /etc/hosts`,
      ].join('\n');
      return remote.ssh(
        `${container} && sudo -n docker exec -u 0 "$id" sh -ec ${quote(restore)} && ` +
          `${compose(remote, quote(`${remote.target.deploymentDir}/.env`))} ` +
          'up -d --no-deps --force-recreate --wait api',
        { timeoutMs: 180_000 },
      );
    },
    'Provider-connectivity fault and network restoration both failed',
  );
}
