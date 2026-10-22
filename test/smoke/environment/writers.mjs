import { withFailureJoiningCleanup } from '../support/cleanup.mjs';
import { escapeSQLString } from '../support/sql.mjs';
import { quote } from '../support/shell.mjs';

function sleep(milliseconds) {
  return new Promise(resolve => setTimeout(resolve, milliseconds));
}

export function terminateApplicationBackendsSQL(application) {
  if (typeof application !== 'string' || application === '' || application.length > 63) {
    throw new Error('Unknown-writer application name must be 1 to 63 characters');
  }
  return `
SELECT pg_terminate_backend(pid)
FROM pg_stat_activity
WHERE application_name = ${escapeSQLString(application)};
`;
}

export async function holdExternalWriter(remote, application, sleepImpl = sleep) {
  const terminate = terminateApplicationBackendsSQL(application);
  const holder = remote.exec(
    `${remote.compose} exec -T -e ${quote(`PGAPPNAME=${application}`)} postgres ` +
      `psql -X -v ON_ERROR_STOP=1 -U pin_glide -d pin_glide -c 'SELECT pg_sleep(20);'`,
    { timeoutMs: 30_000 },
  );
  holder.catch(() => {});
  try {
    const deadline = Date.now() + 10_000;
    for (;;) {
      const writers = await remote.sqlJSON(`
SELECT json_build_object('connections', count(*))
FROM pg_stat_activity
WHERE application_name = ${escapeSQLString(application)};
`);
      if (Number(writers.connections) > 0) break;
      if (Date.now() >= deadline) throw new Error('Controlled external writer did not connect');
      await sleepImpl(100);
    }
  } catch (error) {
    await remote.sql(terminate).catch(() => {});
    await holder.catch(() => {});
    throw error;
  }
  return {
    application,
    async cleanup({ verify = true } = {}) {
      await remote.sql(terminate);
      await holder.catch(() => {});
      if (verify) await remote.assertNoExternalWriters();
    },
  };
}

export async function unknownWriterBlocksSnapshot(remote, application, sleepImpl = sleep) {
  const writer = await holdExternalWriter(remote, application, sleepImpl);
  return withFailureJoiningCleanup(
    async () => {
      let blocked = false;
      try {
        await remote.assertNoExternalWriters();
      } catch (error) {
        if (!/Another database writer/.test(error.message)) throw error;
        blocked = true;
      }
      if (!blocked) throw new Error('The snapshot writer guard accepted a connected external writer');
      return { blocked: true, application };
    },
    () => writer.cleanup(),
    'Unknown-writer probe and writer restoration both failed',
  );
}
