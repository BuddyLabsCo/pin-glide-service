const defaults = Object.freeze({
  httpOrigin: 'http://127.0.0.1:33489',
  wsOrigin: 'ws://127.0.0.1:33489',
  deploymentDir: '/srv/pin-glide-smoke',
  project: 'pin-glide-smoke',
  database: 'pin_glide',
  snapshotDir: '/tmp/snapshots/pin-glide',
  hostPort: 33489,
  containerPort: 8080,
  hostIP: '127.0.0.1',
});

export const TARGET = Object.freeze({ ...defaults });

function origin(value, protocol) {
  let parsed;
  try {
    parsed = new URL(value);
  } catch {
    throw new Error(`Smoke target must be an absolute ${protocol} origin`);
  }
  if (parsed.protocol !== `${protocol}:` || parsed.pathname !== '/' || parsed.search || parsed.hash || parsed.username || parsed.password) {
    throw new Error(`Smoke target must be the adopted bare ${protocol} origin`);
  }
  return parsed.origin;
}

export function releaseVersion(value) {
  if (typeof value !== 'string' || !/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.test(value)) {
    throw new Error('PIN_GLIDE_SMOKE_RELEASE must select an explicit X.Y.Z release');
  }
  return value;
}

// Every value is compared with the adopted development target. An override is
// allowed only when it is exactly that value, so a typo cannot widen the guard.
export function validateTarget(values = {}) {
  const release = releaseVersion(values.release);
  const resolved = {
    ...defaults,
    release,
  };
  for (const key of Object.keys(defaults)) {
    if (values[key] === undefined) continue;
    if (values[key] !== defaults[key]) {
      throw new Error(`Refusing non-adopted smoke target for ${key}`);
    }
  }
  if (values.httpOrigin !== undefined) origin(values.httpOrigin, 'http');
  if (values.wsOrigin !== undefined) origin(values.wsOrigin, 'ws');
  return resolved;
}

export function targetFromOptions(options) {
  return validateTarget({ release: options.release });
}

export function assertHttpOrigin(value, target = TARGET) {
  if (origin(value, 'http') !== target.httpOrigin) {
    throw new Error('HTTP peer must use the adopted local origin');
  }
  return value;
}

export function assertWebSocketOrigin(value, target = TARGET) {
  if (origin(value, 'ws') !== target.wsOrigin) {
    throw new Error('WebSocket peer must use the adopted local origin');
  }
  return value;
}
