#!/usr/bin/env bash
set -euo pipefail

required_version='2.14.0'
lint_bin="${GOLANGCI_LINT_BIN:-golangci-lint}"

if ! command -v "$lint_bin" >/dev/null 2>&1; then
  cat >&2 <<MSG
golangci-lint ${required_version} is required but was not found on PATH.
Install it from https://golangci-lint.run/docs/welcome/install/ and retry.
MSG
  exit 1
fi

version="$("$lint_bin" version | sed -n 's/^golangci-lint has version \([^ ]*\).*/\1/p')"
if [[ "$version" != "$required_version" ]]; then
  printf 'golangci-lint %s is required; found %s.\n' "$required_version" "${version:-unknown}" >&2
  exit 1
fi

"$lint_bin" run ./...
