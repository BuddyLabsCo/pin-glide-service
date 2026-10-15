CREATE TABLE IF NOT EXISTS pin_glide.model_configurations (
    id text NOT NULL,
    workspace_id text NOT NULL,
    model text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_model_configurations PRIMARY KEY (id),
    CONSTRAINT chk_model_configurations_is_the_single_row CHECK (
        id = 'default'
    ),
    CONSTRAINT chk_model_configurations_values_are_present CHECK (
        workspace_id <> '' AND model <> ''
    )
);

COMMENT ON TABLE pin_glide.model_configurations
IS 'Configuration Data: the one enabled non-secret model configuration row.';
COMMENT ON COLUMN pin_glide.model_configurations.id
IS 'Stable identity of the configuration row; the table holds exactly one.';
COMMENT ON COLUMN pin_glide.model_configurations.workspace_id
IS 'Bailian workspace that serves the published Beijing endpoint.';
COMMENT ON COLUMN pin_glide.model_configurations.model
IS 'Model ID the Sender requests at Bailian; inference stays on the Sender.';
COMMENT ON COLUMN pin_glide.model_configurations.enabled
IS 'Whether the service may issue temporary credentials from this row.';
COMMENT ON COLUMN pin_glide.model_configurations.updated_at
IS 'When the configuration row was last changed.';
