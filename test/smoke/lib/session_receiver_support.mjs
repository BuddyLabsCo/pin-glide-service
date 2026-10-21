import { randomBytes } from 'node:crypto';

import { withFailureJoiningCleanup } from './cleanup.mjs';
import { runProcess } from './commands.mjs';
import { SmokeBlocked, SmokeProblem } from './http.mjs';
import { settle } from './session_credential_support.mjs';
import { waitForPresence } from './session_pairing_support.mjs';

export const pairingHTTP = [
  'PostPairingSessions',
  'PostPairingSessionAuthorize',
  'PostPairingSessionComplete',
  'PostPairingSessionReceiverToken',
  'GetSelfReceivers',
];
export const receiverHTTP = ['GetSelfReceivers'];
// Presence settles asynchronously; the ordinary wait is ten seconds, and only
// restart and heartbeat recovery use a longer one.
export const presenceWait = { attempts: 50, intervalMs: 200 };
// References to the Go tests that prove orderings this live path cannot
// observe. The smoke run executes exactly these tests and records a pass only
// when each one reports one.
export const deterministicTestReferences = [
  {
    test: 'TestConcurrentFirstConnectionsAdmitOneReceiver',
    package: './test/integration/pairing',
    level: 'integration',
    covers: ['one-slot-race'],
    observes: 'concurrent upgrades admit exactly one 101 and one 409',
  },
  {
    test: 'TestNewerGenerationOwnsPresenceAcrossOldClose',
    package: './internal/api/wss',
    level: 'unit',
    covers: ['old-close', 'presence'],
    observes: 'an old close cannot remove a newer connection or its presence',
  },
  {
    test: 'TestGenerationChangeDuringUpgradePreventsTheConnection',
    package: './internal/api/wss',
    level: 'unit',
    covers: ['failed-upgrade-cleanup'],
    observes: 'a generation change cancelling a committing upgrade',
  },
  {
    test: 'TestShutdownEndsAnUpgradeStillCommitting',
    package: './internal/api/wss',
    level: 'unit',
    covers: ['shutdown'],
    observes: 'shutdown cancelling an upgrade while its final commit is in flight',
  },
  {
    test: 'TestRecoveryRedemptionAndOldUpgradeCommitOneGeneration',
    package: './test/integration/pairing',
    level: 'integration',
    covers: ['excluded-recovery'],
    observes: 'the excluded recovery race that ordinary smoke cannot observe',
  },
  {
    test: 'TestOwnerRevocationClosesOneReceiverForEverySender',
    package: './test/integration/pairing',
    level: 'integration',
    covers: ['policy-close', 'excluded-revocation'],
    observes: 'the excluded owner-revocation control and policy close across shared Pairings',
  },
  {
    test: 'TestReceiverRevocationSerializesWithRedemptionAndUpgrade',
    package: './test/integration/pairing',
    level: 'integration',
    // Its 1008 assertion runs only when the racing upgrade commits, so it
    // proves serialization rather than a guaranteed policy close.
    covers: ['excluded-revocation'],
    observes: 'revocation serialized with redemption and an upgrade still committing',
  },
  {
    test: 'TestDeviceRevokedReportsTheControlThenCloses',
    package: './internal/api/wss',
    level: 'unit',
    covers: ['policy-close', 'excluded-revocation'],
    observes: 'the excluded policy close that ends a revoked Receiver',
  },
];

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

// exactProblem requires the same published Problem fields for an ordinary HTTP
// operation that expectedProblem requires of a refused upgrade.
export async function exactProblem(session, options, status, code) {
  try {
    await session.request(options);
  } catch (error) {
    if (!expectedProblem(status, code)(error)) throw error;
    return refusal(error);
  }
  throw new Error(`HTTP request unexpectedly succeeded, expected ${status} ${code}`);
}

export function delay(milliseconds) {
  return new Promise(resolve => setTimeout(resolve, milliseconds));
}

export async function waitForHealth(session) {
  let last;
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    try {
      const health = await session.request({ path: '/health' });
      if (health.body?.status === 'ok') return health.requestId;
      last = new Error(`health reported ${health.body?.status || 'no status'}`);
    } catch (error) {
      last = error;
    }
    await delay(500);
  }
  throw new Error(`API did not become healthy after restart: ${last?.message || 'no answer'}`);
}

// deliverOnce uploads one synthetic ciphertext and waits for the live delivery
// frame and its Receipt, so a later reconnect can prove no payload was kept.
export async function deliverOnce(session, label, pairing, socket, tag) {
  const messageID = session.newOperationID(`receiver-delivery-${tag}`);
  const nonce = randomBytes(12).toString('base64');
  const ciphertext = randomBytes(16).toString('base64');
  session.addSecret([nonce, ciphertext]);
  const uploading = settle(session.request({
    method: 'POST',
    path: '/v1/messages',
    token: session.token(label, 'uploader'),
    body: {
      messageId: messageID,
      items: [{
        receiverId: pairing.receiverID,
        pairingId: pairing.pairingID,
        cryptoVersion: 1,
        nonce,
        ciphertext,
      }],
    },
  }));
  const first = await Promise.race([
    socket.next(10_000).then(frame => ({ frame }), error => ({ error })),
    uploading.then(outcome => ({ outcome })),
  ]);
  if (first.error) throw first.error;
  if (first.outcome) {
    if (first.outcome.error) throw first.outcome.error;
    const result = first.outcome.value.body?.results?.[0];
    throw new Error(
      `live Delivery ended as ${first.outcome.value.status} ${result?.status || '(no result)'} ` +
        `${result?.error?.code || '(no code)'} before a frame`,
    );
  }
  const { frame } = first;
  if (
    frame.json?.type !== 'delivery'
    || frame.json.senderId !== session.sender(label).id
    || frame.json.receiverId !== pairing.receiverID
    || frame.json.pairingId !== pairing.pairingID
    || frame.json.messageId !== messageID
    || frame.json.nonce !== nonce
    || frame.json.ciphertext !== ciphertext
  ) {
    throw new Error('live Delivery did not preserve its published correlation and bytes');
  }
  socket.sendJSON({
    version: 1,
    type: 'receipt',
    senderId: frame.json.senderId,
    messageId: frame.json.messageId,
  });
  const outcome = await uploading;
  if (outcome.error) throw outcome.error;
  const uploaded = outcome.value;
  const result = uploaded.body?.results?.[0];
  if (uploaded.status !== 207 || result?.status !== 'received') {
    throw new Error(`live Delivery reported ${uploaded.status} ${result?.status || '(no result)'}`);
  }
  return { messageId: messageID, status: result.status, requestId: uploaded.requestId };
}

export async function rejectedUpgrade(session, options, status, code) {
  try {
    const socket = await session.openSocket(options);
    await socket.close();
  } catch (error) {
    if (!expectedProblem(status, code)(error)) throw error;
    return refusal(error);
  }
  throw new Error(`Receiver upgrade unexpectedly succeeded, expected ${status} ${code}`);
}

export async function sharedPresence(session, pairings, wanted, options) {
  const observed = {};
  for (const [label, pairingID] of Object.entries(pairings)) {
    observed[label] = await waitForPresence(session, label, pairingID, wanted, options);
  }
  return observed;
}

export async function withApiRestart(remote, callback) {
  return withFailureJoiningCleanup(
    async () => {
      await remote.stopApi();
      return callback();
    },
    () => remote.startApi(),
    'API restart and restoration both failed',
  );
}
