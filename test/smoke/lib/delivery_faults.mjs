import { withFailureJoiningCleanup } from './cleanup.mjs';
import { quote } from './shell.mjs';

const guardPattern = /^\/tmp\/pin-glide-dev-smoke-delivery\.[A-Za-z0-9]+$/;
const attempts = 12;
const intervalSeconds = 5;

export function deliveryRestoreWatchdog(remote) {
  const restoreSQL = quote(`
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION 'refusing smoke Delivery restore outside pin_glide';
    END IF;
    IF to_regclass('pin_glide.smoke_delivery_hidden') IS NOT NULL THEN
        IF to_regclass('pin_glide.delivery_deliveries') IS NOT NULL THEN
            RAISE EXCEPTION 'smoke Delivery restore found both table names';
        END IF;
        EXECUTE 'ALTER TABLE pin_glide.smoke_delivery_hidden RENAME TO delivery_deliveries';
    END IF;
    IF to_regclass('pin_glide.delivery_deliveries') IS NULL THEN
        RAISE EXCEPTION 'smoke Delivery restore found neither table name';
    END IF;
END
$$;
`.trim());
  return `#!/bin/sh
guard=$1
attempt=0
while [ -e "$guard" ] && [ "$attempt" -lt ${attempts} ]; do
    sleep ${intervalSeconds}
    attempt=$((attempt + 1))
done
if [ -e "$guard" ]; then
    attempt=0
    while [ "$attempt" -lt 3 ]; do
        if ${remote.compose} exec -T postgres psql -X -v ON_ERROR_STOP=1 -U pin_glide -d pin_glide -c ${restoreSQL}; then
            rm -f "$guard"
            break
        fi
        attempt=$((attempt + 1))
        sleep ${intervalSeconds}
    done
fi
rm -f "$guard"
rm -f "$0"
`;
}

function renameSQL(from, to) {
  return `BEGIN;
SET LOCAL lock_timeout = '10s';
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION 'refusing smoke Delivery fault outside pin_glide';
    END IF;
    IF to_regclass('pin_glide.${from}') IS NULL
       OR to_regclass('pin_glide.${to}') IS NOT NULL THEN
        RAISE EXCEPTION 'smoke Delivery fault found unexpected table state';
    END IF;
END
$$;
ALTER TABLE pin_glide.${from} RENAME TO ${to};
COMMIT;
`;
}

async function restore(remote, guard, watchdog) {
  const script = `
set +e
${remote.compose} exec -T postgres psql -X -v ON_ERROR_STOP=1 -U pin_glide -d pin_glide <<'SQL'
BEGIN;
SET LOCAL lock_timeout = '10s';
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION 'refusing smoke Delivery restore outside pin_glide';
    END IF;
    IF to_regclass('pin_glide.smoke_delivery_hidden') IS NOT NULL THEN
        IF to_regclass('pin_glide.delivery_deliveries') IS NOT NULL THEN
            RAISE EXCEPTION 'smoke Delivery restore found both table names';
        END IF;
        EXECUTE 'ALTER TABLE pin_glide.smoke_delivery_hidden RENAME TO delivery_deliveries';
    END IF;
    IF to_regclass('pin_glide.delivery_deliveries') IS NULL THEN
        RAISE EXCEPTION 'smoke Delivery restore found neither table name';
    END IF;
END
$$;
COMMIT;
SQL
status=$?
if [ "$status" -eq 0 ]; then
    rm -f -- ${quote(guard)} ${quote(watchdog)}
fi
exit "$status"
`;
  await remote.ssh(`sudo -n bash -ec ${quote(script)}`, { timeoutMs: 60_000 });
}

export async function startDeliveryStorageFault(remote) {
  const script = `
set -euo pipefail
guard=$(mktemp /tmp/pin-glide-dev-smoke-delivery.XXXXXX)
watchdog="$guard.sh"
printf '%s\\n' ${quote(deliveryRestoreWatchdog(remote))} > "$watchdog"
chmod 700 "$watchdog"
nohup "$watchdog" "$guard" </dev/null >/dev/null 2>&1 &
printf '%s\\n' "$guard|$watchdog"
`;
  const result = await remote.ssh(`sudo -n bash -ec ${quote(script)}`, { timeoutMs: 30_000 });
  const line = result.stdout.split('\n').map(value => value.trim()).find(value => value.includes('|'));
  const [guard, watchdog] = (line || '').split('|');
  if (!guardPattern.test(guard || '') || watchdog !== `${guard}.sh`) {
    throw new Error('Delivery storage fault did not report its guard and watchdog');
  }
  try {
    await remote.sql(renameSQL('delivery_deliveries', 'smoke_delivery_hidden'));
  } catch (error) {
    await restore(remote, guard, watchdog).catch(() => {});
    throw error;
  }
  return { guard, watchdog };
}

export async function withDeliveryStorageFault(remote, callback) {
  const fault = await startDeliveryStorageFault(remote);
  return withFailureJoiningCleanup(
    callback,
    () => restore(remote, fault.guard, fault.watchdog),
    'Delivery storage fault and restoration both failed',
  );
}
