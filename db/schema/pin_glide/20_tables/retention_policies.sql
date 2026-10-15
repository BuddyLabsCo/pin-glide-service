CREATE TABLE IF NOT EXISTS pin_glide.retention_policies (
    schema_name text NOT NULL,
    table_name text NOT NULL,
    retention_days integer NOT NULL,
    date_column text NOT NULL,
    CONSTRAINT pk_retention_policies PRIMARY KEY (schema_name, table_name),
    CONSTRAINT chk_retention_policies_retention_days_is_positive CHECK (
        retention_days > 0
    )
);

COMMENT ON TABLE pin_glide.retention_policies
IS 'Configuration Data: one whole-table cleanup policy per target table.';
COMMENT ON COLUMN pin_glide.retention_policies.schema_name
IS 'Schema of the cleanup target, quoted separately when cleanup builds SQL.';
COMMENT ON COLUMN pin_glide.retention_policies.table_name
IS 'Table of the cleanup target; it shares the policy of no other table.';
COMMENT ON COLUMN pin_glide.retention_policies.retention_days
IS 'Whole days scheduled cleanup retains before deleting a row.';
COMMENT ON COLUMN pin_glide.retention_policies.date_column
IS 'Time field scheduled cleanup filters on; the caller validates its meaning.';
