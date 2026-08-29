#!/usr/bin/env bash
# Remove the unfinished ai.manaflow.subrouter-forward LaunchDaemon left on
# cmux-mac-mini during a migration that never completed (#127).
#
# That plist points socat at a dead Tailscale peer, is not currently loaded,
# and would collide with the real 0.0.0.0:31415 listener if bootstrapped.
#
# Usage (on the affected Mac, as root):
#   sudo ./deploy/macos/remove-dormant-subrouter-forward.sh
# Optional: also prune inert team plist backups:
#   sudo ./deploy/macos/remove-dormant-subrouter-forward.sh --prune-team-backups
set -euo pipefail

label="${1:-ai.manaflow.subrouter-forward}"
prune_backups=0
if [[ "${1:-}" == "--prune-team-backups" ]]; then
  label="ai.manaflow.subrouter-forward"
  prune_backups=1
elif [[ "${2:-}" == "--prune-team-backups" ]]; then
  prune_backups=1
fi

plist="/Library/LaunchDaemons/${label}.plist"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "remove-dormant-subrouter-forward.sh is macOS-only" >&2
  exit 1
fi

if [[ "$(id -u)" -ne 0 ]]; then
  echo "run as root" >&2
  exit 1
fi

if [[ ! -f "${plist}" ]]; then
  echo "already absent: ${plist}"
else
  # Never load a dormant forwarder; only unload if something already registered it.
  if launchctl print "system/${label}" >/dev/null 2>&1; then
    echo "bootout system/${label}"
    launchctl bootout "system/${label}" 2>/dev/null || launchctl unload "${plist}" 2>/dev/null || true
  fi

  backup="${plist}.removed-$(date +%Y%m%d%H%M%S)"
  mv "${plist}" "${backup}"
  echo "moved ${plist} -> ${backup}"
fi

if [[ "${prune_backups}" -eq 1 ]]; then
  # launchd ignores files without a .plist suffix; these backups are inert but
  # obscure the real team configuration during incidents (#127).
  shopt -s nullglob
  for f in /Library/LaunchDaemons/ai.manaflow.subrouter-team.plist.*; do
    case "$f" in
      *.plist) continue ;; # real plists only — never touch active config
    esac
    dest="${f}.pruned-$(date +%Y%m%d%H%M%S)"
    mv "$f" "$dest"
    echo "pruned backup $f -> $dest"
  done
  shopt -u nullglob
fi

echo "dormant forwarder removed; team supervisor plist is unchanged"
echo "note: a loopback socat shim is not a migration step — cut over via servers.json after the destination daemon is live"
