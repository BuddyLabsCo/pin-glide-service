#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd -P)"
cd "$root"
state_dir="${1:?Development state directory required}"
build_dir="$state_dir/bin"
mkdir -p "$build_dir"

pids=()
cleanup() {
  trap '' INT TERM
  for pid in "${pids[@]}"; do
    kill -TERM -- "-$pid" 2>/dev/null || true
  done
  for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  rm -f "$state_dir/apps.pid"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# The supervisor owns cleanup; lifecycle commands may replace it while building.
printf '%s %s\n' "$$" "$(awk '{print $22}' "/proc/$$/stat")" > "$state_dir/apps.pid"
flock -u 9
exec 9>&-

# Local applications must not reach providers or inherit deployment identity.
unset PIN_GLIDE_WECHAT_APP_ID PIN_GLIDE_WECHAT_APP_SECRET PIN_GLIDE_BAILIAN_API_KEY

dlv_bin="${DLV_BIN:-$(command -v dlv || printf '%s' "$HOME/go/bin/dlv")}"
[[ -x "$dlv_bin" ]] || dlv_bin=''

# Keep debug symbols for remote delve attachment.
setsid go build -gcflags='all=-N -l' -o "$build_dir/pin-glide-api" ./cmd/pin-glide-api &
pids+=("$!")
wait "${pids[-1]}"
unset 'pids[-1]'

if [[ -n "$dlv_bin" ]]; then
  setsid "$dlv_bin" exec "$build_dir/pin-glide-api" \
    --headless --continue --accept-multiclient \
    --listen="127.0.0.1:2345" --api-version=2 &
else
  setsid "$build_dir/pin-glide-api" &
fi
pids+=("$!")

wait -n
