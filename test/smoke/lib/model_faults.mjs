import { escapeSQLString } from './fixtures.mjs';

function restoreModelConfiguration(remote, model) {
  return remote.sql(`
INSERT INTO pin_glide.model_configurations
    (id, workspace_id, model, enabled)
VALUES (
    ${escapeSQLString(model.id)},
    ${escapeSQLString(model.workspaceId)},
    ${escapeSQLString(model.model)},
    true
)
ON CONFLICT (id) DO UPDATE
    SET
        workspace_id = EXCLUDED.workspace_id,
        model = EXCLUDED.model,
        enabled = EXCLUDED.enabled,
        updated_at = now();
`);
}

async function withRestoredModelConfiguration(remote, model, apply, callback) {
  let failure;
  try {
    await remote.sql(apply);
    return await callback();
  } catch (error) {
    failure = error;
    throw error;
  } finally {
    try {
      await restoreModelConfiguration(remote, model);
    } catch (cleanupError) {
      if (failure) {
        throw new AggregateError(
          [failure, cleanupError],
          'Model configuration fault and restoration both failed',
        );
      }
      throw cleanupError;
    }
  }
}

export class ModelFaults {
  constructor({ remote, model }) {
    this.remote = remote;
    this.model = model;
  }

  withDisabled(callback) {
    return withRestoredModelConfiguration(
      this.remote,
      this.model,
      `UPDATE pin_glide.model_configurations
SET enabled = false
WHERE id = ${escapeSQLString(this.model.id)};`,
      callback,
    );
  }

  withMissing(callback) {
    return withRestoredModelConfiguration(
      this.remote,
      this.model,
      `DELETE FROM pin_glide.model_configurations
WHERE id = ${escapeSQLString(this.model.id)};`,
      callback,
    );
  }

  withInvalidParentKey(callback) {
    return this.remote.withInvalidBailianKey(callback);
  }

  withConnectivity(callback) {
    return this.remote.withBailianConnectivityFault(this.model.workspaceId, callback);
  }
}
