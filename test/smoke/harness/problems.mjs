import { SmokeProblem } from './http.mjs';

export function expectedProblem(status, code) {
  return error => {
    if (!(error instanceof SmokeProblem)) return false;
    if (error.status !== status || error.code !== code) return false;
    return error.type === `/problems/${code}`
      && Boolean(error.title)
      && Boolean(error.detail)
      && error.bodyStatus === status
      && Boolean(error.bodyRequestId)
      && Boolean(error.requestId);
  };
}

export function refusal(problem) {
  return {
    status: problem.status,
    code: problem.code,
    type: problem.type,
    title: problem.title,
    detail: problem.detail,
    requestId: problem.requestId,
  };
}

export async function exactProblem(session, options, status, code) {
  try {
    await session.request(options);
  } catch (error) {
    if (!expectedProblem(status, code)(error)) throw error;
    return refusal(error);
  }
  throw new Error(`Expected HTTP ${status} ${code}, but the request succeeded`);
}
