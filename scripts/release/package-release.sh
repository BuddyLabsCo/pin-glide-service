#!/usr/bin/env bash
set -euo pipefail

# Export only the release commit as an isolated Docker build context. The caller
# owns cleanup of the printed directory; no source bundle is published.
# usage: scripts/release/package-release.sh <commit> <validated-version>

script_dir="$(cd "$(dirname "$0")" && pwd)"
cd "$(git rev-parse --show-toplevel)"
commit="${1:-}"
version="${2:-}"
if [[ ! "$commit" =~ ^[a-f0-9]{40}$ || ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo 'usage: scripts/release/package-release.sh <commit> <validated-version>' >&2
  exit 2
fi

directory="$(mktemp -d "${TMPDIR:-/tmp}/pin-glide-db-tools-${version}.XXXXXX")"
trap 'rm -rf "$directory"' EXIT
git archive --format=tar "$commit" | tar -x -C "$directory"

# The runtime filters this shared repository-relative manifest by script stream.
mkdir -p "$directory/db/migrations"
"$script_dir/migration-order.sh" "$commit" "$directory" > "$directory/db/migrations/order.txt"
printf '%s\n' "$directory"
trap - EXIT
