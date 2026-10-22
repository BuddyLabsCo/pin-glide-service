import { sqlTextArray } from '../support/sql.mjs';

export function transaction(message, body) {
  return `BEGIN;
SET LOCAL lock_timeout = '10s';
DO $$
BEGIN
    IF NOT (current_database() = 'pin_glide') THEN
        RAISE EXCEPTION 'refusing smoke fixture outside pin_glide';
    END IF;
END
$$;
${body.trim()}
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pin_glide.identity_sender_registrations
        WHERE id = ANY(${sqlTextArray(message.senderIDs)})
    ) THEN
        RAISE EXCEPTION 'smoke fixture Sender is missing';
    END IF;
END
$$;
COMMIT;
`;
}
