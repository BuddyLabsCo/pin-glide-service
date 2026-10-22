import { runProcess } from '../support/commands.mjs';
import { SmokeBlocked } from './http.mjs';

// parseRanTests requires a PASS line for every referenced test, so a pattern
// that silently matched nothing cannot be reported as evidence.
export function parseRanTests(output, references) {
  const ran = new Set([...output.matchAll(/^\s*--- PASS: (\w+)/gm)].map(match => match[1]));
  const missing = references.filter(item => !ran.has(item.test)).map(item => item.test);
  if (missing.length > 0) {
    throw new Error(`deterministic smoke tests did not run: ${missing.join(', ')}`);
  }
  return references.map(item => item.test);
}

// failingTests reports the referenced tests a failed run named, so the
// evidence can identify the race that failed instead of only the exit code.
export function failingTests(output, references) {
  const failed = new Set([...String(output).matchAll(/^\s*--- FAIL: (\w+)/gm)].map(match => match[1]));
  return references.map(item => item.test).filter(test => failed.has(test));
}

// coverageTests names the executed tests that prove one required behavior. No
// executed test is a hole in the evidence, not an empty list to report as a
// pass.
export function coverageTests(references, passedTests, coverage) {
  const tests = references
    .filter(item => item.covers.includes(coverage) && passedTests.has(item.test))
    .map(item => item.test);
  if (tests.length === 0) {
    throw new Error(`no executed deterministic test covers ${coverage}`);
  }
  return tests;
}

// runDeterministicTests executes the referenced Go tests and reports only what
// actually passed, so the smoke evidence never claims an unexecuted check.
export async function runDeterministicTests(root, references, run = runProcess) {
  const packages = [...new Set(references.map(item => item.package))];
  try {
    if (references.some(item => item.level === 'integration')) {
      // The integration harness starts a disposable PostgreSQL container, so a
      // missing Docker is an unmet prerequisite rather than a failed test.
      await run('docker', ['version', '--format', '{{.Server.Version}}'], {
        cwd: root,
        timeoutMs: 30_000,
      });
    }
  } catch (error) {
    throw new SmokeBlocked(`deterministic smoke tests need a working Docker: ${error.message}`);
  }
  const tests = [];
  for (const packagePath of packages) {
    // One package per invocation keeps each PASS line bound to the package
    // that actually ran it.
    const inPackage = references.filter(item => item.package === packagePath);
    const pattern = `^(${inPackage.map(item => item.test).join('|')})$`;
    let result;
    try {
      result = await run('go', [
        'test', '-tags=integration', '-count=1', '-v', '-run', pattern, packagePath,
      ], { cwd: root, timeoutMs: 1_800_000 });
    } catch (error) {
      // A missing toolchain is an unmet prerequisite, not a failed assertion.
      if (error.code === 'ENOENT') {
        throw new SmokeBlocked(`deterministic smoke tests need a Go toolchain: ${error.message}`);
      }
      const output = `${error.result?.stdout || ''}\n${error.result?.stderr || ''}`;
      const failed = failingTests(output, inPackage);
      const detail = failed.length > 0
        ? `: ${failed.join(', ')} failed`
        : `: ${error.message}`;
      throw new Error(`deterministic smoke tests failed in ${packagePath}${detail}`, { cause: error });
    }
    tests.push(...parseRanTests(result.stdout, inPackage));
  }
  return { tests, packages };
}
