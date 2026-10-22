import { escapeSQLString } from '../support/sql.mjs';

export function renderExpirePairingSessionSQL(sessionID) {
  if (typeof sessionID !== 'string' || sessionID === '') {
    throw new Error('Smoke Pairing Session expiry needs a session ID');
  }
  const id = escapeSQLString(sessionID);
  return `BEGIN;
SET LOCAL lock_timeout = '10s';
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION 'refusing smoke Pairing Session expiry outside pin_glide';
    END IF;
END
$$;
UPDATE pin_glide.pairing_sessions
SET expires_at = now() - interval '1 second'
WHERE id = ${id};
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pin_glide.pairing_sessions
        WHERE id = ${id}
          AND expires_at <= now()
    ) THEN
        RAISE EXCEPTION 'smoke Pairing Session is missing';
    END IF;
END
$$;
COMMIT;
`;
}
