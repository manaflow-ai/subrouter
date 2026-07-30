#!/usr/bin/env bash
# Pull-based macOS worker updater for a supervised Subrouter service.
# The stable supervisor keeps the public listener and existing connections;
# this script replaces only the worker binary and asks the supervisor to start
# a new generation for future connections.
set -euo pipefail

REPO="${SUBROUTER_REPO:-manaflow-ai/subrouter}"
BIN="${SUBROUTER_BIN:-/usr/local/bin/subrouter}"
VERSION_FILE="${SUBROUTER_VERSION_FILE:-/etc/subrouter-version}"
LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter-team}"
PLIST="${SUBROUTER_PLIST:-/Library/LaunchDaemons/${LABEL}.plist}"
CONTROL_SOCKET="${SUBROUTER_CONTROL_SOCKET:-}"
HEALTH_URL="${SUBROUTER_HEALTH_URL:-http://127.0.0.1:31415/_subrouter/health}"

log() { echo "subrouter-autoupdate: $*"; }

if [ -z "$CONTROL_SOCKET" ] && [ -f "$PLIST" ]; then
  # The migration script places the control socket in the service's state
  # directory; read the authoritative path from the LaunchDaemon.
  CONTROL_SOCKET="$(PLIST="$PLIST" python3 - <<'PY'
import os, plistlib
with open(os.environ["PLIST"], "rb") as stream:
    arguments = plistlib.load(stream).get("ProgramArguments") or []
for i, argument in enumerate(arguments):
    if argument == "--control-socket" and i + 1 < len(arguments):
        print(arguments[i + 1])
        break
    if argument.startswith("--control-socket="):
        print(argument.split("=", 1)[1])
        break
PY
)"
fi
CONTROL_SOCKET="${CONTROL_SOCKET:-/var/run/subrouter-supervisor.sock}"

case "$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) log "unsupported arch $(uname -m)"; exit 1 ;;
esac

latest_tag="$(curl -fsSL -H 'Accept: application/vnd.github+json' \
  "https://api.github.com/repos/${REPO}/releases/latest" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])')"
[ -n "$latest_tag" ] || { log "could not resolve latest release tag"; exit 1; }

installed=""
[ -f "$VERSION_FILE" ] && installed="$(sed -n '1p' "$VERSION_FILE" 2>/dev/null || true)"
[ "$latest_tag" != "$installed" ] || exit 0

version="${latest_tag#v}"
asset="subrouter_${version}_darwin_${arch}"
base="https://github.com/${REPO}/releases/download/${latest_tag}"
tmp="$(mktemp -d)"
backup="${BIN}.backup-$(date +%Y%m%d-%H%M%S)"
trap 'rm -rf "$tmp"' EXIT

log "updating worker ${installed:-none} -> ${latest_tag} (${asset})"
curl -fsSL -o "${tmp}/${asset}" "${base}/${asset}"
curl -fsSL -o "${tmp}/SHA256SUMS" "${base}/SHA256SUMS"
(cd "$tmp" && grep " ${asset}\$" SHA256SUMS | shasum -a 256 -c -)
chmod 0755 "${tmp}/${asset}"
"${tmp}/${asset}" --help >/dev/null

cp -p "$BIN" "$backup"
install -m 0755 "${tmp}/${asset}" "${BIN}.new"
mv -f "${BIN}.new" "$BIN"

if ! response="$(curl -fsS --unix-socket "$CONTROL_SOCKET" -X POST "http://localhost/_subrouter/upgrade")"; then
  log "new worker failed readiness; restoring previous worker binary"
  install -m 0755 "$backup" "${BIN}.rollback"
  mv -f "${BIN}.rollback" "$BIN"
  exit 1
fi

if ! curl -fsS "$HEALTH_URL" >/dev/null; then
  log "new generation switched but public health failed; rolling new connections back"
  install -m 0755 "$backup" "${BIN}.rollback"
  mv -f "${BIN}.rollback" "$BIN"
  curl -fsS --unix-socket "$CONTROL_SOCKET" -X POST "http://localhost/_subrouter/upgrade" >/dev/null || true
  exit 1
fi

active="$(printf '%s' "$response" | python3 -c 'import json,sys; print(json.load(sys.stdin)["active"]["id"])')"
printf '%s\n' "$latest_tag" >"${VERSION_FILE}.new"
mv -f "${VERSION_FILE}.new" "$VERSION_FILE"
log "updated to ${latest_tag}; active generation=${active}; old connections are draining"
