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

latest_tag="$(resolve_latest_tag)"
[ -n "$latest_tag" ] || { log "could not resolve latest release tag"; exit 1; }

installed=""
[ -f "$VERSION_FILE" ] && installed="$(sed -n '1p' "$VERSION_FILE" 2>/dev/null || true)"

# The marker can outlive the binary it describes, so treat a missing binary as
# out of date no matter what the marker says.
if [ "$latest_tag" = "$installed" ] && [ -x "$INSTALL_DIR/subrouter" ]; then
  exit 0
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
