import { runScenario } from './session_credential_support.mjs';
import { runDeterministicTests } from './session_receiver_support.mjs';

// The unattended journey cannot obtain a fresh provider authorization code.
// These integration tests use provider fakes to keep the excluded business
// paths observable without labeling them as deployed smoke coverage.
export const excludedProviderTestReferences = Object.freeze([
  {
    test: 'TestWeChatLoginRegistersAndRestoresSenderInstallations',
    package: './test/integration/identity',
    level: 'integration',
    covers: ['wechat-login', 'login-family-replacement', 'provider-identity-mapping'],
    observes: 'fresh WeChat login registers, restores and replaces one Sender family without merging another identity',
  },
  {
    test: 'TestQQLoginCreatesRestoresAndSeparatesAccounts',
    package: './test/integration/identity',
    level: 'integration',
    covers: ['qq-login', 'login-family-replacement', 'provider-identity-mapping'],
    observes: 'fresh QQ login registers, restores and replaces one Sender family while WeChat remains a separate namespace',
  },
  {
    test: 'TestOwnerRevocationRequiresAnOwnedRegistrationAndFreshSameAccountProof',
    package: './test/integration/identity',
    level: 'integration',
    covers: ['device-security-revocation'],
    observes: 'Sender security revocation requires an owned registration and fresh same-Account provider proof',
  },
  {
    test: 'TestOwnerRevocationClosesOnlyThatSenderRegistration',
    package: './test/integration/identity',
    level: 'integration',
    covers: ['device-security-revocation'],
    observes: 'an approved Sender security revocation closes one registration without disturbing shared Receiver access',
  },
  {
    test: 'TestRecoveryRequiresTheOwningSendersFreshProof',
    package: './test/integration/pairing',
    level: 'integration',
    covers: ['receiver-recovery'],
    observes: 'Receiver recovery requires the owning Sender and fresh proof of its Account',
  },
  {
    test: 'TestRecoveryReplacesTheCredentialAndPreservesRetainedAssociations',
    package: './test/integration/pairing',
    level: 'integration',
    covers: ['receiver-recovery'],
    observes: 'approved Receiver recovery replaces the credential and preserves Pairings whose keys remain available',
  },
  {
    test: 'TestQQProofSupportsRecoveryAndPermanentDeviceRevocation',
    package: './test/integration/pairing',
    level: 'integration',
    covers: ['qq-proof', 'device-security-revocation', 'receiver-recovery'],
    observes: 'QQ owner proof drives recovery and permanent Sender and Receiver revocation',
  },
]);

export async function runExcludedProviderEvidence({ session, root, run } = {}) {
  return runScenario(
    session,
    'excluded real-provider paths retain separate deterministic coverage',
    async () => ({
      smokeStatus: 'excluded',
      ...(await runDeterministicTests(root, excludedProviderTestReferences, run)),
      references: excludedProviderTestReferences.map(({
        test,
        package: packagePath,
        level,
        covers,
        observes,
      }) => ({
        test,
        package: packagePath,
        level,
        covers,
        observes,
      })),
    }),
    { evidenceType: 'deterministic' },
  );
}
