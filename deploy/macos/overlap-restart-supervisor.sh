#!/usr/bin/env bash
# Overlap-restart a LaunchDaemon supervisor without a multi-minute outage (#125).
#
# The supervisor binds with SO_REUSEPORT, so this script can start the
# replacement before SIGTERM reaches the draining process. launchd KeepAlive
# alone cannot do that: it waits for exit, and today SIGTERM closes the TCP
# listener immediately, so the whole drain window is an outage.
set -euo pipefail

label="${1:-ai.manaflow.subrouter-team}"
plist="/Library/LaunchDaemons/${label}.plist"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "overlap-restart-supervisor.sh is macOS-only" >&2
  exit 1
fi

if [[ ! -f "${plist}" ]]; then
  echo "missing plist: ${plist}" >&2
  exit 1
fi

bin="$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "${plist}" 2>/dev/null || true)"
if [[ -z "${bin}" || ! -x "${bin}" ]]; then
  echo "could not resolve ProgramArguments[0] from ${plist}" >&2
  exit 1
fi

# Collect remaining ProgramArguments for the successor.
args=()
i=1
while value="$(/usr/libexec/PlistBuddy -c "Print :ProgramArguments:${i}" "${plist}" 2>/dev/null)"; do
  args+=("${value}")
  i=$((i + 1))
done

echo "starting overlapping successor: ${bin} ${args[*]}"
sudo "${bin}" "${args[@]}" &
successor_pid=$!

# Give the successor a moment to bind with SO_REUSEPORT before draining the old process.
sleep 1
if ! kill -0 "${successor_pid}" 2>/dev/null; then
  echo "successor exited immediately" >&2
  exit 1
fi

echo "signaling launchd to stop ${label} (successor pid ${successor_pid} already listening)"
sudo launchctl bootout "system/${label}" 2>/dev/null || sudo launchctl unload "${plist}" 2>/dev/null || true

# Prefer the managed plist again once the old process is gone.
sudo launchctl bootstrap system "${plist}" 2>/dev/null || sudo launchctl load -w "${plist}" 2>/dev/null || true
echo "overlap restart requested for ${label}"
