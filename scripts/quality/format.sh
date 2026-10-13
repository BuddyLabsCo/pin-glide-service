#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"

case "${1:-}" in
  --check)
    gofmt_check=true
    ;;
  '')
    gofmt_check=false
    ;;
  *)
    echo "usage: $0 [--check]" >&2
    exit 2
    ;;
esac

mapfile -d '' go_files < <(
  git ls-files --cached --others --exclude-standard -z -- '*.go' \
    ':!internal/db/jetgen/**' \
    ':!generated/**' | while IFS= read -r -d '' path; do
    [[ -f "$path" ]] && printf '%s\0' "$path"
  done
)
if ((${#go_files[@]})); then
  if [[ "$gofmt_check" == true ]]; then
    gofmt_files="$(gofmt -l "${go_files[@]}")"
    if [[ -n "$gofmt_files" ]]; then
      printf '%s\n' "$gofmt_files" >&2
      exit 1
    fi
  else
    gofmt -w "${go_files[@]}"
  fi
fi
