#!/usr/bin/env bash

# Fails when regeneration left the working tree different from the commit:
# tracked drift means the checked-in generated output is stale, and untracked
# output means generation produced an artifact nobody committed. Run it after
# `make generate`.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"

if ! git diff --quiet; then
  printf 'generated artifacts drifted from the committed output:\n' >&2
  git --no-pager diff --stat >&2
  exit 1
fi

untracked="$(git ls-files --others --exclude-standard)"
if [[ -n "$untracked" ]]; then
  printf 'generation produced untracked output:\n%s\n' "$untracked" >&2
  exit 1
fi
