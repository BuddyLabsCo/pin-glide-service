CREATE OR REPLACE FUNCTION pin_glide.pin_glide_fn_cleanup_expired_rows(
    reference_time timestamptz
)
RETURNS integer
LANGUAGE plpgsql
AS $$
DECLARE
    policy record;
    deleted integer;
    removed integer := 0;
BEGIN
    FOR policy IN
        SELECT schema_name, table_name, retention_days, date_column
        FROM pin_glide.retention_policies
        ORDER BY schema_name, table_name
    LOOP
        -- Identifiers are quoted separately and never bound; the cutoff is
        -- bound, so a policy cannot carry SQL that would run with it.
        EXECUTE format(
            'DELETE FROM %I.%I WHERE %I < $1',
            policy.schema_name,
            policy.table_name,
            policy.date_column
        ) USING reference_time - make_interval(days => policy.retention_days);
        GET DIAGNOSTICS deleted = ROW_COUNT;
        removed := removed + deleted;
    END LOOP;
    RAISE NOTICE 'pin_glide cleanup deleted % rows', removed;
    RETURN removed;
END
$$;

COMMENT ON FUNCTION pin_glide.pin_glide_fn_cleanup_expired_rows(timestamptz) IS
'Deletes every retained row each configured policy puts before its cutoff.
parameters:
reference_time: The instant the retention windows of every policy are measured back from.';
