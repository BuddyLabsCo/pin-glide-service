CREATE TABLE IF NOT EXISTS release.release_data_scripts (
    filename text NOT NULL,
    checksum text NOT NULL,
    sql_body text,
    applied_at timestamptz NOT NULL DEFAULT now(),
    duration_ms bigint NOT NULL DEFAULT 0,
    executed_by text NOT NULL DEFAULT 'system',
    CONSTRAINT pk_release_data_scripts PRIMARY KEY (filename)
);

COMMENT ON TABLE release.release_data_scripts
IS 'Data scripts accepted as processed; ordinary failures are not recorded.';
COMMENT ON COLUMN release.release_data_scripts.filename
IS 'Immutable data script filename.';
COMMENT ON COLUMN release.release_data_scripts.checksum
IS 'SHA-256 of the immutable script file.';
COMMENT ON COLUMN release.release_data_scripts.sql_body
IS 'Complete script snapshot; NULL when historical SQL is unknown.';
COMMENT ON COLUMN release.release_data_scripts.applied_at
IS 'When the script was accepted as processed.';
COMMENT ON COLUMN release.release_data_scripts.duration_ms
IS 'Execution milliseconds; zero for skips or unknown durations.';
COMMENT ON COLUMN release.release_data_scripts.executed_by
IS 'Execution attribution; skipped or an operator name is also allowed.';
