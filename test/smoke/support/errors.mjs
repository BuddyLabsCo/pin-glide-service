export class SmokeValidationError extends Error {
  constructor(cause) {
    super(cause.message);
    this.name = 'SmokeValidationError';
    this.cause = cause;
  }
}
