export async function withFailureJoiningCleanup(action, cleanup, message) {
  let failure;
  try {
    return await action();
  } catch (error) {
    failure = error;
    throw error;
  } finally {
    try {
      await cleanup();
    } catch (cleanupError) {
      if (failure) throw new AggregateError([failure, cleanupError], message);
      throw cleanupError;
    }
  }
}
