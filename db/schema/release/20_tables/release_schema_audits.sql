CREATE TABLE IF NOT EXISTS release.release_schema_audits (
    id bigserial,
    target_schema text NOT NULL,
    operation text NOT NULL,
    stdout text NOT NULL,
    stderr text NOT NULL,
    exit_code integer,
    executed_at timestamptz NOT NULL DEFAULT now(),
    duration_ms bigint NOT NULL DEFAULT 0,
    executed_by text NOT NULL DEFAULT 'system',
    CONSTRAINT pk_release_schema_audits PRIMARY KEY (id)
);

COMMENT ON TABLE release.release_schema_audits
IS 'Every psqldef apply and check result, including failures.';
COMMENT ON COLUMN release.release_schema_audits.id
IS 'Append-only invocation identifier.';
COMMENT ON COLUMN release.release_schema_audits.target_schema
IS 'Schema compared or updated by psqldef.';
COMMENT ON COLUMN release.release_schema_audits.operation
IS 'Independent psqldef apply or check invocation.';
COMMENT ON COLUMN release.release_schema_audits.stdout
IS 'Complete captured standard output without truncation.';
COMMENT ON COLUMN release.release_schema_audits.stderr
IS 'Complete captured standard error without truncation.';
COMMENT ON COLUMN release.release_schema_audits.exit_code
IS 'Original child exit code; NULL if no normal exit code is available.';
COMMENT ON COLUMN release.release_schema_audits.executed_at
IS 'When this invocation was recorded.';
COMMENT ON COLUMN release.release_schema_audits.duration_ms
IS 'Invocation milliseconds; zero when unknown.';
COMMENT ON COLUMN release.release_schema_audits.executed_by
IS 'Invocation attribution, including operator-supplied names.';
