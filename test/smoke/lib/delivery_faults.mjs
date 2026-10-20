import { withFailureJoiningCleanup } from './cleanup.mjs';
import { quote } from './shell.mjs';

const guardPattern = /^\/tmp\/pin-glide-dev-smoke-delivery\.[A-Za-z0-9]+$/;
const environmentLock = '/tmp/pin-glide-dev-smoke.lock';

export function deliveryRestoreSQL() {
  return `BEGIN;
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
`;
}

export function deliveryRestoreWatchdog(remote, { intervalSeconds = 5, lockFile = environmentLock } = {}) {
  return `#!/bin/sh
guard=$1
[ -e ${lockFile} ] || (umask 0; : > ${lockFile})
exec 9>${lockFile}
flock 9
attempt=0
while [ -e "$guard" ] && [ "$attempt" -lt 12 ]; do
    if ${remote.compose} exec -T postgres psql -X -v ON_ERROR_STOP=1 -U pin_glide -d pin_glide -f - < "$guard.sql"; then
        rm -f "$guard" "$guard.sql"
        break
    fi
    attempt=$((attempt + 1))
    sleep ${intervalSeconds}
done
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
${remote.compose} exec -T postgres psql -X -v ON_ERROR_STOP=1 -U pin_glide -d pin_glide -f - < ${quote(`${guard}.sql`)}
status=$?
if [ "$status" -eq 0 ]; then
    rm -f -- ${quote(guard)} ${quote(`${guard}.sql`)} ${quote(watchdog)}
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
printf '%s\\n' ${quote(deliveryRestoreSQL())} > "$guard.sql"
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

// A previous run can die between renaming the table and releasing the shared
// lock. The next authorized run repairs that leftover before doing any work.
export async function recoverDeliveryStorageFault(remote) {
  const state = await remote.sqlJSON(`
SELECT json_build_object(
  'hidden', to_regclass('pin_glide.smoke_delivery_hidden') IS NOT NULL
);
`);
  if (state.hidden) await remote.sql(deliveryRestoreSQL());
}

const lockQuery = `
SELECT count(*)::int AS held
FROM pg_locks AS locks
JOIN pg_class AS relations ON relations.oid = locks.relation
WHERE relations.relname = 'delivery_deliveries'
  AND locks.mode = 'AccessExclusiveLock'
  AND locks.granted;
`;

// Hold the Delivery relation so the next upload's claim waits, which is what
// lets the scenario observe the whole-batch deadline rather than a write or
// Receipt bound.
export async function withDeliveryBatchLock(remote, callback, { seconds = 20 } = {}) {
  const command = [
    `psql -X -v ON_ERROR_STOP=1 -U pin_glide -d pin_glide -c`,
    quote(`BEGIN; LOCK TABLE pin_glide.delivery_deliveries IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(${seconds}); COMMIT;`),
  ].join(' ');
  const holder = remote.ssh(
    `${remote.compose} exec -T postgres ${command}`,
    { timeoutMs: (seconds + 30) * 1_000 },
  );
  // The holder ends by itself either when its sleep completes or when the
  // service's deadline ends the waiting claim.
  holder.catch(() => {});
  const deadline = Date.now() + 10_000;
  for (;;) {
    const state = await remote.sqlJSON(lockQuery);
    if (Number(state.held) > 0) break;
    if (Date.now() > deadline) throw new Error('Delivery batch lock was not acquired');
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  return withFailureJoiningCleanup(
    async () => callback(holder),
    async () => {
      await holder.catch(() => {});
      for (;;) {
        const state = await remote.sqlJSON(lockQuery);
        if (Number(state.held) === 0) return;
        if (Date.now() > deadline + 30_000) throw new Error('Delivery batch lock was not released');
        await new Promise(resolve => setTimeout(resolve, 100));
      }
    },
    'Delivery batch lock and its release both failed',
  );
}

export async function withDeliveryStorageFault(remote, callback) {
  const fault = await startDeliveryStorageFault(remote);
  return withFailureJoiningCleanup(
    callback,
    () => restore(remote, fault.guard, fault.watchdog),
    'Delivery storage fault and restoration both failed',
  );
}
