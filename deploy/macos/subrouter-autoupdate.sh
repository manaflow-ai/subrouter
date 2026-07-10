#!/usr/bin/env bash
# Pull-based auto-updater for a macOS Subrouter host (parity with the GCP
# updater in ../gcp/subrouter-autoupdate.sh).
#
# Polls the latest published GitHub release and, when it differs from the
# installed version, downloads the matching darwin binary, verifies its
# checksum, swaps it in atomically, and restarts the LaunchDaemon. launchd has
# no systemd-socket handoff, so a restart briefly drops in-flight requests; the
# process handles SIGTERM and `launchctl kickstart -k` gives a clean stop/start.
# A release only exists after CI ran `go test`, so this never deploys untested
# code. Runs as root from a LaunchDaemon (writes /usr/local/bin and /etc).
set -euo pipefail

REPO="${SUBROUTER_REPO:-manaflow-ai/subrouter}"
BIN="${SUBROUTER_BIN:-/usr/local/bin/subrouter}"
VERSION_FILE="${SUBROUTER_VERSION_FILE:-/etc/subrouter-version}"
LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter-team}"
HEALTH_URL="${SUBROUTER_HEALTH_URL:-http://127.0.0.1:31415/_subrouter/health}"

log() { echo "subrouter-autoupdate: $*"; }

case "$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
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
asset="subrouter_${version}_darwin_${arch}"
base="https://github.com/${REPO}/releases/download/${latest_tag}"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

log "updating ${installed:-none} -> ${latest_tag} (${asset})"
curl -fsSL -o "${tmp}/${asset}" "${base}/${asset}"
curl -fsSL -o "${tmp}/SHA256SUMS" "${base}/SHA256SUMS"
( cd "${tmp}" && grep " ${asset}\$" SHA256SUMS | shasum -a 256 -c - )

chmod 0755 "${tmp}/${asset}"
cp -p "${BIN}" "${BIN}.bak-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
# Atomic replace: install to a temp path on the same filesystem, then rename.
install -m 0755 "${tmp}/${asset}" "${BIN}.new"
mv -f "${BIN}.new" "${BIN}"
launchctl kickstart -k "system/${LABEL}"

i=0
until curl -fsS "${HEALTH_URL}" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "${i}" -ge 30 ]; then
    log "health check failed after restart; leaving new binary in place"
    exit 1
  fi
  sleep 1
done

echo "${latest_tag}" > "${VERSION_FILE}"
log "updated to ${latest_tag} and healthy"
