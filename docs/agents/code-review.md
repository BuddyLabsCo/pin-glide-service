# Code Review Contract

Use this contract for code, tests, tools, and build configuration.

## Prepare

1. Read the governing issue or specification's relevant requirements, non-goals,
   and explicit exceptions. Resolve missing or ambiguous details at the source.
2. Read guidelines in `code-architecture.md`
3. Inspect affected functionalities and tests.

Preparation is complete when every changed behavior maps to the requested
contract or is identified as unintended scope.

## Review

- Verify success, boundary, malformed-input, cancellation, and failure paths
  relevant to the contract.
- Check compatibility, public behavior, state transitions, resource ownership,
  concurrency, exceptions, and performance where affected.
- Make sure code changes follow guidelines under `code-architecture.md`
- Confirm tests independently prove the changed behavior at stable seams.
- Unit tests should follow guidelines under `docs/agents/test-value-gate.md`
- Prefer the simplest design that fully satisfies the requirement.
- Keep one authoritative source for each fact; derive copies, fixtures, and
  expectations from it.
- Avoid duplication, speculative abstractions, hidden side effects, and
  unnecessary dependencies.
- Make invalid states, boundaries, and failure paths explicit.
- Use names that reveal intent. Comments explain why, not what.
- Preserve existing behavior unless the specification explicitly changes it.
- Review the final diff for design, correctness, simplicity, tests, naming,
  comments, style, and documentation.
- After non-mechanical review fixes, review the final HEAD again.

Review is complete when every changed execution path and applicable guideline
category has been considered.

## Findings

Report only actionable problems caused or exposed by the change. Each finding
must identify the smallest useful file and line range, a concrete failure
scenario, and its engineering impact.

Exclude subjective preferences, configured-tool diagnostics, unrelated
pre-existing problems, and out-of-scope recommendations. Respect explicit
exceptions and non-goals; silence in the issue is not a waiver of repository
standards.

Present findings first, ordered by file location. If there are none, say so
explicitly.
