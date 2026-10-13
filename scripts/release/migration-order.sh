#!/usr/bin/env bash
set -euo pipefail

# Without arguments, include local uncommitted SQL after committed history.
# A release supplies its commit and exported build context for strict validation.
# usage: scripts/release/migration-order.sh [commit [directory]]
commit="${1:-HEAD}"
directory="${2:-.}"
export LC_ALL=C
if [[ "$(git rev-parse --is-shallow-repository)" == true ]]; then
  echo 'Migration order requires complete Git history; fetch unshallow first' >&2
  exit 1
fi
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

git -c diff.orderFile=/dev/null log --reverse --first-parent --diff-merges=first-parent \
  --no-renames --diff-filter=A --format= --name-only "$commit" -- db/migrations db/scripts \
  | awk '/\.sql$/ {
      if ($0 !~ /^db\/(migrations\/(pin_glide|release)|scripts)\/[^/]+\.sql$/) {
        print "Migration order manifest is inconsistent: invalid path " $0 > "/dev/stderr"; exit 1
      }
      if (!seen[$0]++) print
    }' > "$tmp/history"
(
  cd "$directory"
  for path in db/migrations db/scripts; do
    if [[ -d "$path" ]]; then find "$path" -type f -name '*.sql' -print; fi
  done
) | sort > "$tmp/files"
if grep -Ev '^db/(migrations/(pin_glide|release)|scripts)/[^/]+\.sql$' "$tmp/files" >&2; then
  echo 'Migration order manifest is inconsistent: SQL must belong to a supported stream' >&2
  exit 1
fi
if (($# == 0)); then
  awk 'FILENAME == ARGV[1] { seen[$0] = 1; next } !seen[$0]' "$tmp/history" "$tmp/files" > "$tmp/local"
  cat "$tmp/local" >> "$tmp/history"
fi
if ! diff -u <(sort "$tmp/history") "$tmp/files" >&2; then
  echo 'Migration order manifest is inconsistent with the release SQL files' >&2
  exit 1
fi
cat "$tmp/history"
