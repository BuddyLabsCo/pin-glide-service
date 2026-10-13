#!/usr/bin/env bash

# Validates the published HTTP and WSS descriptions with the pinned validators
# and runs the contract coverage tests against the same documents. Both
# descriptions come from the API entrypoint's offline modes, so nothing here
# needs a listener, an application database or a provider.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"

tmp="$(mktemp -d)"
cleanup() {
  rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# Keep the pinned validators hermetic: no usage reporting, no update notice and
# no writes to the home directory of whoever runs the check.
export SUPPRESS_NO_CONFIG_WARNING=1 REDOCLY_SUPPRESS_UPDATE_NOTICE=true
export ASYNCAPI_METRICS_CONFIG_PATH="$tmp/asyncapi-analytics.json"
printf '{"analyticsEnabled":"false","infoMessageShown":"true","userID":"pin-glide-service"}\n' \
  > "$ASYNCAPI_METRICS_CONFIG_PATH"

violating_openapi="$root/test/fixtures/contract/violating_openapi.yaml"
violating_asyncapi="$root/test/fixtures/contract/violating_asyncapi.yaml"

redocly_lint() { npx --no-install redocly lint "$@"; }
asyncapi_validate() { npx --no-install asyncapi validate "$@"; }

# `npx --no-install` runs whatever node_modules holds, so the installed
# validators are checked against the versions package.json pins.
check_pinned_version() {
  local package="$1" pinned installed
  pinned="$(node -p "require('$root/package.json').devDependencies['$package']")"
  installed="$(node -p "require('$package/package.json').version" 2>/dev/null || true)"
  if [[ "$installed" != "$pinned" ]]; then
    printf '%s %s is required; found %s. Run npm ci.\n' "$package" "$pinned" "${installed:-nothing}" >&2
    exit 1
  fi
}

# A clean run cannot show that a validator is able to fail, so a fixture it
# must reject has to be rejected, with the expected diagnostic, before this
# command may pass.
expect_failure() {
  local description="$1" reports="$2"
  shift 2
  local missing=() output report
  if output="$("$@" 2>&1)"; then
    printf '%s\n%s accepted the violating fixture.\n' "$output" "$description" >&2
    exit 1
  fi
  local IFS='|'
  for report in $reports; do
    grep -qF -- "$report" <<<"$output" || missing+=("$report")
  done
  if ((${#missing[@]})); then
    printf '%s\n%s failed without reporting %s.\n' "$output" "$description" "${missing[*]}" >&2
    exit 1
  fi
}

check_pinned_version '@redocly/cli'
check_pinned_version '@asyncapi/cli'

go run ./cmd/pin-glide-api -openapi > "$tmp/openapi.yaml"
go run ./cmd/pin-glide-api -asyncapi > "$tmp/asyncapi.yaml"

redocly_lint "$tmp/openapi.yaml"
asyncapi_validate "$tmp/asyncapi.yaml" --fail-severity=error

# The contract tests cover what the validators cannot judge: the AsyncAPI
# schema dialect conversion of the shared schemas, and that the published
# descriptions carry the routes, messages and authentication the service
# actually implements.
go test ./internal/api/wss/... ./test/contract/...

expect_failure 'the OpenAPI validator' 'no-unresolved-refs' redocly_lint "$violating_openapi"
expect_failure 'the AsyncAPI validator' 'asyncapi-document-resolved' \
  asyncapi_validate "$violating_asyncapi" --fail-severity=error
