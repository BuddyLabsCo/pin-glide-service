CREATE TABLE IF NOT EXISTS release.release_migration_scripts (
    target_schema text NOT NULL,
    filename text NOT NULL,
    checksum text NOT NULL,
    sql_body text,
    applied_at timestamptz NOT NULL DEFAULT now(),
    duration_ms bigint NOT NULL DEFAULT 0,
    executed_by text NOT NULL DEFAULT 'system',
    CONSTRAINT pk_release_migration_scripts
    PRIMARY KEY (target_schema, filename)
);

COMMENT ON TABLE release.release_migration_scripts
IS 'Migration scripts accepted as processed, including explicit skips.';
COMMENT ON COLUMN release.release_migration_scripts.target_schema
IS 'Schema prepared by this migration: release or pin_glide.';
COMMENT ON COLUMN release.release_migration_scripts.filename
IS 'Immutable filename within the target schema.';
COMMENT ON COLUMN release.release_migration_scripts.checksum
IS 'SHA-256 of the immutable script file.';
COMMENT ON COLUMN release.release_migration_scripts.sql_body
IS 'Complete script snapshot; NULL when historical SQL is unknown.';
COMMENT ON COLUMN release.release_migration_scripts.applied_at
IS 'When the script was accepted as processed.';
COMMENT ON COLUMN release.release_migration_scripts.duration_ms
IS 'Execution milliseconds; zero for skips or unknown durations.';
COMMENT ON COLUMN release.release_migration_scripts.executed_by
IS 'Execution attribution; skipped or an operator name is also allowed.';
