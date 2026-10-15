#!/usr/bin/env bash
# Pull-based updater for a user's Subrouter CLI. The server autoupdater keeps a
# shared host current; this keeps the `sr` on a laptop current, so a client and
# the servers it talks to do not drift apart.
#
# Idempotent and cheap: it compares the release tag against a per-user version
# marker and exits without downloading anything when they match. The install
# itself is delegated to install.sh, which verifies the release checksum.
set -euo pipefail

REPO="${SUBROUTER_REPO:-manaflow-ai/subrouter}"
INSTALL_DIR="${SUBROUTER_INSTALL_DIR:-$HOME/bin}"
VERSION_FILE="${SUBROUTER_VERSION_FILE:-$HOME/.subrouter/cli-version}"
INSTALL_URL="${SUBROUTER_INSTALL_URL:-https://raw.githubusercontent.com/$REPO/main/install.sh}"
RELEASE_API_URL="${SUBROUTER_RELEASE_API_URL:-https://api.github.com/repos/${REPO}/releases/latest}"
RELEASE_LATEST_URL="${SUBROUTER_RELEASE_LATEST_URL:-https://github.com/${REPO}/releases/latest}"

log() { echo "subrouter-cli-autoupdate: $*"; }

# resolve_latest_tag prints the newest release tag.
#
# The GitHub API is rate limited per source IP and answers 403 from a busy
# address. That is not a transient failure: this updater ran every two minutes
# against the same limit and stayed 403 for days, so the host it updates sat on
# an old release with nothing but a traceback in a log nobody reads. The
# releases/latest redirect carries the same answer in a Location header, is not
# rate limited, and needs no credential, so it is tried first. The API remains
# the fallback, and an explicitly configured API URL wins outright because that
# is how the tests point the updater at a fixture.
resolve_latest_tag() {
  if [ -n "${SUBROUTER_RELEASE_API_URL:-}" ]; then
    resolve_latest_tag_from_api
    return
  fi
  local effective=""
  effective="$(curl -fsSL -o /dev/null -w '%{url_effective}' "$RELEASE_LATEST_URL" 2>/dev/null || true)"
  case "$effective" in
    */releases/tag/*)
      printf '%s\n' "${effective##*/}"
      return 0
      ;;
  esac
  resolve_latest_tag_from_api
}

resolve_latest_tag_from_api() {
  curl -fsSL -H 'Accept: application/vnd.github+json' "$RELEASE_API_URL" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])'
}

# is_semver accepts vMAJOR.MINOR.PATCH[-prerelease][+build].
is_semver() {
  [[ "$1" =~ ^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z.-]*)?$ ]]
}

# compare_numeric prints -1, 0 or 1 for two decimal strings of any length.
compare_numeric() {
  if [ "${#1}" -ne "${#2}" ]; then
    [ "${#1}" -lt "${#2}" ] && echo -1 || echo 1
  elif [[ "$1" < "$2" ]]; then
    echo -1
  elif [[ "$1" > "$2" ]]; then
    echo 1
  else
    echo 0
  fi
}

# semver_newer succeeds when $1 has higher semver precedence than $2: a
# prerelease sorts before its release, numeric identifiers compare
# numerically and before alphanumeric ones, build metadata is ignored.
semver_newer() {
  local left="${1#v}" right="${2#v}"
  left="${left%%+*}"
  right="${right%%+*}"
  local left_pre="" right_pre=""
  case "$left" in *-*) left_pre="${left#*-}"; left="${left%%-*}" ;; esac
  case "$right" in *-*) right_pre="${right#*-}"; right="${right%%-*}" ;; esac
  local -a lc rc lp rp
  IFS=. read -r -a lc <<<"$left"
  IFS=. read -r -a rc <<<"$right"
  local i c
  for i in 0 1 2; do
    c="$(compare_numeric "${lc[$i]}" "${rc[$i]}")"
    [ "$c" = 0 ] || { [ "$c" = 1 ]; return; }
  done
  [ -n "$left_pre" ] || { [ -n "$right_pre" ]; return; }
  [ -n "$right_pre" ] || return 1
  IFS=. read -r -a lp <<<"$left_pre"
  IFS=. read -r -a rp <<<"$right_pre"
  for ((i = 0; i < ${#lp[@]} && i < ${#rp[@]}; i++)); do
    local a="${lp[$i]}" b="${rp[$i]}"
    [ "$a" = "$b" ] && continue
    if [[ "$a" =~ ^[0-9]+$ && "$b" =~ ^[0-9]+$ ]]; then
      [ "$(compare_numeric "$a" "$b")" = 1 ]
      return
    fi
    [[ "$a" =~ ^[0-9]+$ ]] && return 1
    [[ "$b" =~ ^[0-9]+$ ]] && return 0
    [[ "$a" > "$b" ]]
    return
  done
  [ "${#lp[@]}" -gt "${#rp[@]}" ]
}

# supports_update_auto succeeds when the installed CLI has `update --auto`
# (#623). The probe only reads help text, and SUBROUTER_NO_AUTO_UPDATE keeps the
# probe itself from starting a background update.
supports_update_auto() {
  [ -x "$INSTALL_DIR/subrouter" ] || return 1
  SUBROUTER_NO_AUTO_UPDATE=1 "$INSTALL_DIR/subrouter" update --help </dev/null 2>/dev/null \
    | grep -q -- 'update --auto'
}

# update_auto_targets_cli succeeds when `update --auto` would replace
# $INSTALL_DIR/subrouter itself. It replaces the binary the daemon LaunchAgent
# runs (its --worker-bin under a supervisor), and only without that agent the
# CLI that was invoked. When the daemon runs a different binary, handing over
# would leave this CLI on its old release, so it stays on install.sh.
update_auto_targets_cli() {
  local plist="$HOME/Library/LaunchAgents/${SUBROUTER_DAEMON_LABEL:-ai.manaflow.subrouter}.plist"
  [ -f "$plist" ] || return 0
  python3 -I - "$plist" "$INSTALL_DIR/subrouter" <<'PY' 2>/dev/null
import os, plistlib, sys
with open(sys.argv[1], "rb") as f:
    args = plistlib.load(f).get("ProgramArguments") or []
target = args[0] if args else ""
if len(args) > 1 and args[1] == "supervise":
    target = ""
    for i, arg in enumerate(args):
        if arg == "--worker-bin" and i + 1 < len(args):
            target = args[i + 1]
            break
        if arg.startswith("--worker-bin="):
            target = arg.split("=", 1)[1]
            break
real = lambda p: os.path.realpath(os.path.abspath(p))
sys.exit(0 if target and real(target) == real(sys.argv[2]) else 1)
PY
}

# A CLI that can update itself does it better than this script: `update
# --auto` verifies the staged binary, keeps a backup, waits for the daemon to
# be idle, health-checks it and restores on failure, and quietly skips a team
# host, a package-manager install or a newer build. Hand over to it, and keep
# the install.sh path below only to bring an older CLI onto a release that has
# it.
if supports_update_auto && update_auto_targets_cli; then
  log "running $INSTALL_DIR/subrouter update --auto (details in ~/.subrouter/logs/auto-update.log)"
  "$INSTALL_DIR/subrouter" update --auto </dev/null
  exit
fi

latest_tag="$(resolve_latest_tag)"
[ -n "$latest_tag" ] || { log "could not resolve latest release tag"; exit 1; }

installed=""
[ -f "$VERSION_FILE" ] && installed="$(sed -n '1p' "$VERSION_FILE" 2>/dev/null || true)"

# The marker can outlive the binary it describes, so treat a missing binary as
# out of date no matter what the marker says.
if [ "$latest_tag" = "$installed" ] && [ -x "$INSTALL_DIR/subrouter" ]; then
  exit 0
fi

# Never move backwards. A CLI built from main reports a Go pseudo-version such
# as v0.1.134-0.20260927095747-b5bd50354af3, which is newer than v0.1.133, and
# a plain `go build` reports "devel", which cannot be ordered at all. Either
# one was put there on purpose, so leave it until a newer release exists.
# Builds too old to answer `version` (anything not starting with the program
# name) report nothing and are always replaced.
binary_version=""
if [ -x "$INSTALL_DIR/subrouter" ]; then
  binary_version="$("$INSTALL_DIR/subrouter" version 2>/dev/null | awk 'NR == 1 && ($1 == "subrouter" || $1 == "sr" || $1 == "cx") { print $2 }' || true)"
fi
if [ -n "$binary_version" ] && [ "${binary_version#v}" != "${latest_tag#v}" ]; then
  if ! is_semver "$binary_version"; then
    log "installed CLI $binary_version is a development build; not replacing it with ${latest_tag}"
    exit 0
  fi
  if is_semver "$latest_tag" && semver_newer "$binary_version" "$latest_tag"; then
    log "installed CLI $binary_version is newer than ${latest_tag}; not downgrading"
    exit 0
  fi
fi

log "updating CLI ${installed:-none} -> ${latest_tag}"
curl -fsSL "$INSTALL_URL" | env \
  SUBROUTER_VERSION="$latest_tag" \
  SUBROUTER_INSTALL_DIR="$INSTALL_DIR" \
  SUBROUTER_VERSION_FILE="$VERSION_FILE" \
  ${SUBROUTER_DOWNLOAD_BASE:+SUBROUTER_DOWNLOAD_BASE="$SUBROUTER_DOWNLOAD_BASE"} \
  sh

# Prove the new binary runs before anyone depends on it. install.sh already
# rolled back on a failed install; this catches a binary that installs but
# cannot execute, such as a quarantined or wrong-architecture download.
"$INSTALL_DIR/subrouter" --help >/dev/null
log "CLI is now ${latest_tag}"
