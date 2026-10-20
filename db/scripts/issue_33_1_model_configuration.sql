-- The approved Bailian Beijing Workspace and model are non-secret deployment
-- configuration. Re-running the release converges this row without moving the
-- provider key into application data or the release ledger.
INSERT INTO pin_glide.model_configurations
(id, workspace_id, model, enabled)
VALUES ('default', 'ws-kea2u5v9va6wsdvh', 'qwen3.7-flash', true)
ON CONFLICT (id) DO UPDATE
    SET
        workspace_id = EXCLUDED.workspace_id,
        model = EXCLUDED.model,
        enabled = EXCLUDED.enabled,
        updated_at = now();
