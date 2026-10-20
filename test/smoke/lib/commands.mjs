import { spawn } from 'node:child_process';

const maxOutput = 32 * 1024 * 1024;

export function runProcess(command, args, {
  cwd,
  env,
  input,
  timeoutMs = 120_000,
  allowFailure = false,
} = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      cwd,
      env,
      stdio: ['pipe', 'pipe', 'pipe'],
    });
    const stdout = [];
    const stderr = [];
    let stdoutBytes = 0;
    let stderrBytes = 0;
    let settled = false;
    const fail = error => {
      if (settled) return;
      settled = true;
      reject(error);
    };
    const timer = setTimeout(() => {
      child.kill('SIGKILL');
      fail(new Error(`${command} timed out after ${timeoutMs}ms`));
    }, timeoutMs);
    timer.unref();
    child.stdout.on('data', chunk => {
      stdoutBytes += chunk.length;
      if (stdoutBytes > maxOutput) {
        child.kill('SIGKILL');
        fail(new Error(`${command} produced too much output`));
        return;
      }
      stdout.push(chunk);
    });
    child.stderr.on('data', chunk => {
      stderrBytes += chunk.length;
      if (stderrBytes > maxOutput) {
        child.kill('SIGKILL');
        fail(new Error(`${command} produced too much error output`));
        return;
      }
      stderr.push(chunk);
    });
    child.on('error', error => {
      clearTimeout(timer);
      fail(error);
    });
    child.on('close', (code, signal) => {
      clearTimeout(timer);
      if (settled) return;
      settled = true;
      const result = {
        command,
        args,
        code,
        signal,
        stdout: Buffer.concat(stdout).toString('utf8'),
        stderr: Buffer.concat(stderr).toString('utf8'),
      };
      if (code !== 0 && !allowFailure) {
        const error = new Error(`${command} failed with exit code ${code ?? signal}`);
        error.result = result;
        return reject(error);
      }
      resolve(result);
    });
    child.stdin.end(input);
  });
}
