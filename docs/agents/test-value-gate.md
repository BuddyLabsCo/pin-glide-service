## Test Value Gate

Automated tests are not an output quota.

Before writing each test, identify:

- Regression: the realistic production break it catches.
- Seam: the stable public interface it exercises.
- Observation: the caller-visible result or side effect it asserts.
- Existing protection: why an existing test does not already catch it.

If these cannot be stated concretely, do not add the test.

Do not test private methods, source text, constants, getters, trivial
delegation, framework mechanics, or mocks themselves.

Mock only slow, external, or nondeterministic system boundaries.
If mock setup dominates the test, prefer an integration or contract test.

Choose the test level where the risk lives. Never add tests only for
coverage targets or a per-function checklist.

For bug fixes, add the smallest regression test and observe it fail first.
Preserve tests protecting distinct business invariants, security,
compatibility, data integrity, and cross-component behavior.