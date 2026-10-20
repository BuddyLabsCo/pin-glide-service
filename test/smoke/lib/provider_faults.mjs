import { quote } from './shell.mjs';

function compose(remote, envFileArgument) {
  return [
    'docker compose',
    `--project-directory ${quote(remote.target.deploymentDir)}`,
    `--env-file ${envFileArgument}`,
    `-f ${quote(`${remote.target.deploymentDir}/docker-compose.yml`)}`,
  ].join(' ');
}

function removeHostsLine(marker) {
  return [
    'tmp=$(mktemp)',
    `grep -v ${quote(`# ${marker}$`)} /etc/hosts > "$tmp" || true`,
    'cat "$tmp" > /etc/hosts',
    'rm -f "$tmp"',
  ].join('\n');
}

async function startInvalidBailianKeyFault(remote) {
  const envFile = `${remote.target.deploymentDir}/.env`;
  const command = `
set -euo pipefail
umask 077
temporary=$(mktemp /tmp/pin-glide-dev-smoke-bailian.XXXXXX.env)
cleanup() {
    status=$?
    trap - EXIT
    rm -f -- "$temporary"
    if [ "$status" -ne 0 ]; then
        ${compose(remote, quote(envFile))} up -d --no-deps --force-recreate --wait api || true
    fi
    exit "$status"
}
trap cleanup EXIT
cp -- ${quote(envFile)} "$temporary"
if grep -q '^BAILIAN_API_KEY=' "$temporary"; then
    sed -i 's|^BAILIAN_API_KEY=.*$|BAILIAN_API_KEY=sk-smoke-invalid-parent-key|' "$temporary"
else
    printf '%s\\n' 'BAILIAN_API_KEY=sk-smoke-invalid-parent-key' >> "$temporary"
fi
${compose(remote, '"$temporary"')} up -d --no-deps --force-recreate --wait api
rm -f -- "$temporary"
trap - EXIT
printf '%s\\n' "$temporary"
`;
  const result = await remote.ssh(`sudo -n bash -ec ${quote(command)}`, { timeoutMs: 180_000 });
  const temporaryEnv = result.stdout
    .split('\n')
    .map(line => line.trim())
    .find(line => /^\/tmp\/pin-glide-dev-smoke-bailian\.[A-Za-z0-9]+\.env$/.test(line));
  if (!temporaryEnv) throw new Error('Invalid parent-key fault did not report its temporary env file');
  return temporaryEnv;
}

async function stopInvalidBailianKeyFault(remote, temporaryEnv) {
  if (!/^\/tmp\/pin-glide-dev-smoke-bailian\.[A-Za-z0-9]+\.env$/.test(temporaryEnv)) {
    throw new Error('Refusing to remove an unexpected smoke env file');
  }
  const envFile = `${remote.target.deploymentDir}/.env`;
  const command = `
set +e
trap 'rm -f -- ${quote(temporaryEnv)}' EXIT
${compose(remote, quote(envFile))} up -d --no-deps --force-recreate --wait api
restore_status=$?
rm -f -- ${quote(temporaryEnv)}
exit "$restore_status"
`;
  await remote.ssh(`sudo -n bash -ec ${quote(command)}`, { timeoutMs: 180_000 });
}

export async function withInvalidBailianKey(remote, callback) {
  const temporaryEnv = await startInvalidBailianKeyFault(remote);
  let failure;
  try {
    return await callback();
  } catch (error) {
    failure = error;
    throw error;
  } finally {
    try {
      await stopInvalidBailianKeyFault(remote, temporaryEnv);
    } catch (cleanupError) {
      if (failure) {
        throw new AggregateError(
          [failure, cleanupError],
          'Invalid parent-key fault and environment restoration both failed',
        );
      }
      throw cleanupError;
    }
  }
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

  let failure;
  try {
    return await callback();
  } catch (error) {
    failure = error;
    throw error;
  } finally {
    const restore = [
      remove,
      `! grep -F ${quote(marker)} /etc/hosts`,
    ].join('\n');
    try {
      await remote.ssh(
        `${container} && sudo -n docker exec -u 0 "$id" sh -ec ${quote(restore)}`,
        { timeoutMs: 30_000 },
      );
    } catch (cleanupError) {
      if (failure) {
        throw new AggregateError(
          [failure, cleanupError],
          'Provider-connectivity fault and network restoration both failed',
        );
      }
      throw cleanupError;
    }
  }
}
