-- A re-pairing reserves its new Pairing next to the working association, so
-- the pair holds one active association rather than one row in any state.
-- Schema Sync issues no destructive change, so the superseded index is dropped
-- here; the partial index in the desired schema then replaces it. Dropping an
-- already partial index is skipped, which keeps a rerun from rebuilding it.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_index AS index
        JOIN pg_class AS relation ON relation.oid = index.indexrelid
        JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
        WHERE namespace.nspname = 'pin_glide'
          AND relation.relname = 'uidx_pairing_pairings_sender_id_receiver_id'
          AND index.indpred IS NULL
    ) THEN
        DROP INDEX pin_glide.uidx_pairing_pairings_sender_id_receiver_id;
    END IF;
END
$$;
