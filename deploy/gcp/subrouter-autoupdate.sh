#!/usr/bin/env bash
# Pull-based auto-updater for the Subrouter VM.
#
# Polls the latest published GitHub release and, when it differs from the
# installed version, downloads the matching binary, verifies its checksum,
# swaps it in, and hands over to the new binary.
#
# When the supervisor control socket exists, the swap is connection-preserving:
# the supervisor owns the listener, health-checks the new worker before routing
# new connections to it, and lets the old worker finish the connections it
# already accepted.
#
# Without the supervisor this falls back to `systemctl restart`, which is *not*
# connection-preserving. Socket activation keeps the listener up, so new
# connections are never refused, but connections already established belong to
# the worker being replaced and are closed. A client mid-stream sees its
# response cut. Migrate with deploy/gcp/migrate-systemd-to-supervisor.sh.
#
# A release only exists after CI ran `go test`, so this never deploys untested
# code.
set -euo pipefail

REPO="${SUBROUTER_REPO:-manaflow-ai/subrouter}"
BIN="${SUBROUTER_BIN:-/usr/local/bin/subrouter}"
VERSION_FILE="${SUBROUTER_VERSION_FILE:-/etc/subrouter-version}"
SERVICE="${SUBROUTER_SERVICE:-subrouter.service}"
CONTROL_SOCKET="${SUBROUTER_CONTROL_SOCKET:-/var/lib/subrouter/supervisor.sock}"

log() { echo "subrouter-autoupdate: $*"; }

case "$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) log "unsupported arch $(uname -m)"; exit 1 ;;
esac

latest_tag="$(curl -fsSL -H 'Accept: application/vnd.github+json' \
  "https://api.github.com/repos/${REPO}/releases/latest" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])')"
if [ -z "${latest_tag}" ]; then
  log "could not resolve latest release tag"; exit 1
fi

installed=""
[ -f "${VERSION_FILE}" ] && installed="$(cat "${VERSION_FILE}" 2>/dev/null || true)"

if [ "${latest_tag}" = "${installed}" ]; then
  exit 0
fi

version="${latest_tag#v}"
asset="subrouter_${version}_linux_${arch}"
base="https://github.com/${REPO}/releases/download/${latest_tag}"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

log "updating ${installed:-none} -> ${latest_tag} (${asset})"
curl -fsSL -o "${tmp}/${asset}" "${base}/${asset}"
curl -fsSL -o "${tmp}/SHA256SUMS" "${base}/SHA256SUMS"
( cd "${tmp}" && grep " ${asset}\$" SHA256SUMS | sha256sum -c - )

cp -p "${BIN}" "${BIN}.bak-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
install -m 0755 -o root -g root "${tmp}/${asset}" "${BIN}"
if [ -S "${CONTROL_SOCKET}" ]; then
  log "asking the supervisor for a new worker generation"
  if ! curl -fsS --unix-socket "${CONTROL_SOCKET}" -X POST \
      http://localhost/_subrouter/upgrade >/dev/null; then
    log "supervisor upgrade failed; restoring the previous worker"
    mv -f "${BIN}.previous" "${BIN}" 2>/dev/null || true
    curl -fsS --unix-socket "${CONTROL_SOCKET}" -X POST \
      http://localhost/_subrouter/upgrade >/dev/null 2>&1 || true
    exit 1
  fi
else
  log "no supervisor control socket; falling back to a restart that drops established connections"
  systemctl restart "${SERVICE}"
fi

i=0
until curl -fsS http://127.0.0.1:31415/_subrouter/health >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "${i}" -ge 30 ]; then
    log "health check failed after restart; leaving new binary in place"
    exit 1
  fi
  sleep 1
done

echo "${latest_tag}" > "${VERSION_FILE}"
log "updated to ${latest_tag} and healthy"
