CREATE TABLE IF NOT EXISTS release.release_deploy_history (
    id bigserial,
    release_version text NOT NULL,
    deployed_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_release_deploy_history PRIMARY KEY (id)
);

COMMENT ON TABLE release.release_deploy_history
IS 'Successful application Schema Sync milestones, not startup success.';
COMMENT ON COLUMN release.release_deploy_history.id
IS 'Append-only Database Release identifier.';
COMMENT ON COLUMN release.release_deploy_history.release_version
IS 'Release whose application schema applied and converged.';
COMMENT ON COLUMN release.release_deploy_history.deployed_at
IS 'When application Schema Sync completed, before data scripts.';
