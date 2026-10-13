#!/usr/bin/env bash
set -euo pipefail

# The one derived server distribution every database consumer runs. Printing the
# tag on stdout lets a caller compose it into docker run; the build itself is
# cached, so repeated callers only pay for it once.
root="$(cd "$(dirname "$0")/../.." && pwd)"
tag="pin-glide-postgres:local"
docker build --quiet --tag "$tag" --file "$root/deploy/compose/postgres.Dockerfile" "$root" >&2
printf '%s\n' "$tag"
