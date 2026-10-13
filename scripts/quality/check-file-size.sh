#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
max_lines=500

is_in_scope() {
  local path="$1"
  case "$path" in
    internal/db/jetgen/*|generated/*) return 1 ;;
    *.go|*.ts|*.tsx|*.css|*.module.css|*.sh) return 0 ;;
    *) return 1 ;;
  esac
}

check_file() {
  local path="$1"
  local relative="$path"
  local lines
  if [[ "$relative" == "$root/"* ]]; then
    relative="${relative#"$root/"}"
  fi
  is_in_scope "$relative" || return 0
  lines="$(awk 'END { print NR }' "$path")"
  if (( lines > max_lines )); then
    printf '%s: %s physical lines (maximum %s)\n' "$path" "$lines" "$max_lines" >&2
    return 1
  fi
}

if (($#)); then
  for path in "$@"; do
    check_file "$path"
  done
else
  while IFS= read -r -d '' path; do
    [[ -f "$root/$path" ]] || continue
    check_file "$root/$path"
  done < <(
    cd "$root"
    git ls-files --cached --others --exclude-standard -z -- '*.go' '*.ts' '*.tsx' '*.css' '*.module.css' '*.sh'
  )
fi
