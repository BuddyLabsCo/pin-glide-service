const defaults = Object.freeze({
  sshHost: 'local-server',
  httpOrigin: 'http://192.168.10.17:33489',
  wsOrigin: 'ws://192.168.10.17:33489',
  deploymentDir: '/srv/pin-glide-dev',
  project: 'pin-glide-dev',
  database: 'pin_glide',
  snapshotDir: '/mnt/share/workspace/snapshots/pin-glide',
  hostPort: 33489,
  containerPort: 8080,
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

export function targetFromOptions(options, env = process.env) {
  const names = {
    sshHost: 'PIN_GLIDE_SMOKE_SSH_HOST',
    httpOrigin: 'PIN_GLIDE_SMOKE_HTTP_ORIGIN',
    wsOrigin: 'PIN_GLIDE_SMOKE_WS_ORIGIN',
    deploymentDir: 'PIN_GLIDE_SMOKE_DEPLOYMENT_DIR',
    project: 'PIN_GLIDE_SMOKE_PROJECT',
    database: 'PIN_GLIDE_SMOKE_DATABASE',
    snapshotDir: 'PIN_GLIDE_SMOKE_SNAPSHOT_DIR',
  };
  const values = { release: options.release };
  for (const [key, name] of Object.entries(names)) {
    if (env[name] !== undefined) values[key] = env[name];
  }
  return validateTarget(values);
}

export function assertHttpOrigin(value, target = TARGET) {
  if (origin(value, 'http') !== target.httpOrigin) {
    throw new Error('HTTP peer must use the adopted LAN origin');
  }
  return value;
}

export function assertWebSocketOrigin(value, target = TARGET) {
  if (origin(value, 'ws') !== target.wsOrigin) {
    throw new Error('WebSocket peer must use the adopted LAN origin');
  }
  return value;
}
