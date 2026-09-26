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
LOCK_FILE="${SUBROUTER_DEPLOY_LOCK_FILE:-/run/lock/subrouter-deploy.lock}"
FRONT_CONTROL_SOCKET="${SUBROUTER_FRONT_CONTROL_SOCKET:-/var/lib/subrouter/front.sock}"

log() { echo "subrouter-autoupdate: $*"; }

for required_command in curl flock mktemp python3 sha256sum systemctl; do
  command -v "${required_command}" >/dev/null 2>&1 \
    || { log "${required_command} is required"; exit 1; }
done
exec 9>"${LOCK_FILE}"
if ! flock -n 9; then
  log "another deployment owns ${LOCK_FILE}; skipping this poll"
  exit 0
fi

# Front/slot hosts are release-pinned and changed only by the live deployment
# workflow. Updating the legacy path behind the front would bypass its switch
# and rollback protocol.
if [ -S "${FRONT_CONTROL_SOCKET}" ] || systemctl is-active --quiet subrouter-front.service 2>/dev/null; then
  log "front/slot deployment detected; automatic latest-release updates are disabled"
  exit 0
fi

case "$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) log "unsupported arch $(uname -m)"; exit 1 ;;
esac

RELEASE_API_URL="${SUBROUTER_RELEASE_API_URL:-https://api.github.com/repos/${REPO}/releases/latest}"
RELEASE_LATEST_URL="${SUBROUTER_RELEASE_LATEST_URL:-https://github.com/${REPO}/releases/latest}"

# resolve_latest_tag prints the newest release tag.
#
# The GitHub API is rate limited per source IP and answers 403 from a busy
# address. That is not a transient failure: an updater on a timer keeps hitting
# the same limit, so the host sits on an old release with nothing but a
# traceback in a log nobody reads. The releases/latest redirect carries the same
# answer in a Location header, is not rate limited, and needs no credential, so
# it is tried first. The API remains the fallback, and an explicitly configured
# API URL wins outright because that is how tests point the updater at a
# fixture.
resolve_latest_tag() {
  if [ -n "${SUBROUTER_RELEASE_API_URL:-}" ]; then
    resolve_latest_tag_from_api
    return
  fi
  local effective=""
  effective="$(curl -fsSL -o /dev/null -w '%{url_effective}' "${RELEASE_LATEST_URL}" 2>/dev/null || true)"
  case "${effective}" in
    */releases/tag/*)
      printf '%s\n' "${effective##*/}"
      return 0
      ;;
  esac
  resolve_latest_tag_from_api
}

resolve_latest_tag_from_api() {
  curl -fsSL -H 'Accept: application/vnd.github+json' "${RELEASE_API_URL}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])'
}

latest_tag="$(resolve_latest_tag)"
if [ -z "${latest_tag}" ]; then
  log "could not resolve latest release tag"; exit 1
fi
[[ "${latest_tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || {
  log "latest release tag is not versioned: ${latest_tag}"
  exit 1
}

installed=""
[ -f "${VERSION_FILE}" ] && installed="$(cat "${VERSION_FILE}" 2>/dev/null || true)"

if [ "${latest_tag}" = "${installed}" ]; then
  exit 0
fi

version="${latest_tag#v}"
asset="subrouter_${version}_linux_${arch}"
base="https://github.com/${REPO}/releases/download/${latest_tag}"
tmp="$(mktemp -d)"
version_incoming=""
binary_incoming=""
restore_incoming=""
cleanup() {
  rm -rf -- "${tmp}"
  [ -z "${version_incoming}" ] || rm -f -- "${version_incoming}"
  [ -z "${binary_incoming}" ] || rm -f -- "${binary_incoming}"
  [ -z "${restore_incoming}" ] || rm -f -- "${restore_incoming}"
}
trap cleanup EXIT INT TERM

log "updating ${installed:-none} -> ${latest_tag} (${asset})"
curl -fsSL --retry 3 --retry-all-errors -o "${tmp}/${asset}" "${base}/${asset}"
curl -fsSL --retry 3 --retry-all-errors -o "${tmp}/SHA256SUMS" "${base}/SHA256SUMS"
awk -v asset="${asset}" '$2 == asset || $2 == "*" asset {print $1 "  " asset}' \
  "${tmp}/SHA256SUMS" >"${tmp}/candidate.SHA256SUM"
[[ "$(wc -l <"${tmp}/candidate.SHA256SUM" | tr -d '[:space:]')" == "1" ]] || {
  log "SHA256SUMS must contain exactly one checksum for ${asset}"
  exit 1
}
expected_sum="$(awk '{print $1}' "${tmp}/candidate.SHA256SUM")"
[[ "${expected_sum}" =~ ^[0-9a-f]{64}$ ]] || { log "release checksum is invalid"; exit 1; }
(cd "${tmp}" && sha256sum -c candidate.SHA256SUM >/dev/null)

[[ -f "${BIN}" && ! -L "${BIN}" ]] || { log "installed binary is not a regular file: ${BIN}"; exit 1; }
if { [[ -e "${VERSION_FILE}" ]] || [[ -L "${VERSION_FILE}" ]]; } && [[ ! -f "${VERSION_FILE}" ]]; then
  log "version marker target is not a regular file: ${VERSION_FILE}"
  exit 1
fi
previous="$(mktemp "${BIN}.bak-$(date +%Y%m%d-%H%M%S).XXXXXX")"
if ! cp -p -- "${BIN}" "${previous}"; then
  rm -f -- "${previous}"
  log "could not preserve the current binary"
  exit 1
fi
version_incoming="$(mktemp "$(dirname "${VERSION_FILE}")/.subrouter-version.XXXXXX")"
printf '%s\n' "${latest_tag}" >"${tmp}/subrouter-version"
install -m 0644 -o root -g root "${tmp}/subrouter-version" "${version_incoming}"
# Write beside the target and rename, never onto it. Opening a running
# executable for writing fails with ETXTBSY ("Text file busy"); rename replaces
# the directory entry and leaves the running inode alone. Reproduced on the
# staging VM, where writing directly to /usr/local/bin/subrouter fails while the
# service is up.
binary_incoming="$(mktemp "$(dirname "${BIN}")/.subrouter.incoming.XXXXXX")"
install -m 0755 -o root -g root "${tmp}/${asset}" "${binary_incoming}"
mv -f -- "${binary_incoming}" "${BIN}"
binary_incoming=""

# Assert the bytes on disk are the bytes we verified. Overwriting a running
# binary can fail (ETXTBSY) or partially succeed, and a silent no-op here means
# the old code keeps serving while the version file claims otherwise.
installed_sum="$(sha256sum "${BIN}" | awk '{print $1}')"

activate_current_binary() {
  if [ -S "${CONTROL_SOCKET}" ]; then
    curl -fsS --max-time 30 --unix-socket "${CONTROL_SOCKET}" -X POST \
      http://localhost/_subrouter/upgrade >/dev/null
  else
    systemctl restart "${SERVICE}"
  fi
}

restore_previous_bytes() {
  log "restoring ${previous}"
  restore_incoming="$(mktemp "$(dirname "${BIN}")/.subrouter.rollback.XXXXXX")" || return 1
  install -m 0755 -o root -g root "${previous}" "${restore_incoming}" || return 1
  mv -f -- "${restore_incoming}" "${BIN}" || return 1
  restore_incoming=""
}

restore_previous() {
  restore_previous_bytes || return 1
  activate_current_binary
}

if [ "${expected_sum}" != "${installed_sum}" ]; then
  log "installed binary does not match the verified checksum"
  restore_previous_bytes || log "automatic binary restore also failed"
  exit 1
fi

if [ -S "${CONTROL_SOCKET}" ]; then
  log "asking the supervisor for a new worker generation"
else
  log "no supervisor control socket; falling back to a restart that drops established connections"
fi
if ! activate_current_binary; then
  log "release activation failed"
  restore_previous || log "automatic rollback activation also failed"
  exit 1
fi

i=0
until curl -fsS --max-time 2 http://127.0.0.1:31415/_subrouter/health >/dev/null 2>&1 &&
    curl -fsS --max-time 2 http://127.0.0.1:31415/_subrouter/ready >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "${i}" -ge 30 ]; then
    # Leaving a binary that cannot pass a health check in place is how a bad
    # release becomes a permanent outage. Put the previous one back.
    log "health or readiness failed after upgrade"
    restore_previous || log "automatic rollback activation also failed"
    exit 1
  fi
  sleep 1
done

if ! mv -f -- "${version_incoming}" "${VERSION_FILE}"; then
  log "could not commit the version marker"
  restore_previous || log "automatic rollback activation also failed"
  exit 1
fi
version_incoming=""
log "updated to ${latest_tag} and healthy"
