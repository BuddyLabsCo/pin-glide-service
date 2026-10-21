import { spawn } from 'node:child_process';

export async function acquireEnvironmentLock({ target, spawnImpl = spawn, timeoutMs = 10_000 }) {
  const marker = 'PIN_GLIDE_SMOKE_LOCK_ACQUIRED';
  const child = spawnImpl('ssh', [
    '-o', 'BatchMode=yes',
    '-o', 'ConnectTimeout=10',
    target.sshHost,
    `flock -n /tmp/pin-glide-dev-smoke.lock -c 'printf "${marker}\\n"; cat >/dev/null'`,
  ], { stdio: ['pipe', 'pipe', 'pipe'] });
  let output = '';
  let errorOutput = '';
  let acquired = false;
  let exited = false;
  let settled = false;
  const finish = (resolve, reject, error) => {
    if (settled) return;
    settled = true;
    if (error) reject(error);
    else resolve();
  };
  const terminate = () => {
    if (!exited) child.kill('SIGKILL');
    child.stdin.destroy();
  };
  // A failed pipe write must not surface as an unhandled error after cancellation.
  child.stdin.on('error', () => {});
  child.stderr.on('data', chunk => {
    errorOutput += chunk.toString();
  });
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      terminate();
      finish(resolve, reject, new Error('timed out acquiring the shared smoke environment lock'));
    }, timeoutMs);
    child.stdout.on('data', chunk => {
      output += chunk.toString();
      if (!settled && !acquired && output.includes(marker)) {
        acquired = true;
        clearTimeout(timer);
        finish(resolve, reject);
      }
    });
    child.once('error', error => {
      clearTimeout(timer);
      terminate();
      finish(resolve, reject, error);
    });
    child.once('exit', code => {
      exited = true;
      if (!settled && !acquired) {
        clearTimeout(timer);
        finish(resolve, reject, new Error(`another smoke run holds the environment lock${code ? ` (exit ${code})` : ''}${errorOutput ? `: ${errorOutput.trim()}` : ''}`));
      }
    });
  });
  return {
    async release() {
      if (!exited) child.stdin.end();
      if (exited) return;
      const term = setTimeout(() => child.kill('SIGKILL'), 2_000);
      term.unref();
      await new Promise(resolve => child.once('exit', resolve));
      clearTimeout(term);
    },
  };
}
