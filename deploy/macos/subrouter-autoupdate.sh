#!/usr/bin/env bash
# Pull-based macOS worker updater for a supervised Subrouter service.
# The stable supervisor keeps the public listener and existing connections;
# this script replaces only the worker binary and asks the supervisor to start
# a new generation for future connections.
set -euo pipefail

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
# shellcheck disable=SC1091
. "$SCRIPT_DIR/mutation-lease-lib.sh"

REPO="${SUBROUTER_REPO:-manaflow-ai/subrouter}"
BIN="${SUBROUTER_BIN:-/usr/local/bin/subrouter}"
VERSION_FILE="${SUBROUTER_VERSION_FILE:-/etc/subrouter-version}"
LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter-team}"
PLIST="${SUBROUTER_PLIST:-/Library/LaunchDaemons/${LABEL}.plist}"
CONTROL_SOCKET="${SUBROUTER_CONTROL_SOCKET:-}"
UPGRADE_INHIBIT_FILE="${SUBROUTER_UPGRADE_INHIBIT_FILE:-${PLIST}.supervisor-transaction/upgrade-inhibited}"
MUTATION_LOCK_FILE="${SUBROUTER_MUTATION_LOCK_FILE:-${PLIST}.supervisor-mutation.lock}"
HEALTH_URL="${SUBROUTER_HEALTH_URL:-http://127.0.0.1:31415/_subrouter/health}"
STATE="${SUBROUTER_DEPLOY_STATE:-/var/lib/subrouter-verify}"
# The one lock every writer of the worker binary takes (subrouter-deploy.sh,
# this updater while it swaps, subrouter-guard.sh while it promotes or rolls
# back). The flock mutation lease below only serializes against the migration
# scripts; without this lock a guard tick between the swap and the generation
# switch recorded the untested candidate as last-good.
DEPLOY_LOCK_DIR="${SUBROUTER_DEPLOY_LOCK_DIR:-${STATE}/deploy.lock}"
RELEASE_DOWNLOAD_URL="${SUBROUTER_RELEASE_DOWNLOAD_URL:-https://github.com/${REPO}/releases/download}"
HELD_DEPLOY_LOCK=0
tmp=""
# Same directory and naming as subrouter-deploy.sh (<epoch-ns>_<version>), so
# `subrouter-deploy.sh list` shows what an autoupdate replaced and
# `subrouter-deploy.sh rollback --to <version>` can put it back.
BACKUP_DIR="${SUBROUTER_BACKUP_DIR:-${STATE}/backups}"
KEEP_BACKUPS="${SUBROUTER_KEEP_BACKUPS:-3}"

log() { echo "subrouter-autoupdate: $*"; }

if ! acquire_subrouter_mutation_lease "$MUTATION_LOCK_FILE"; then
  log "another deployment or worker update holds the mutation lease; update deferred"
  exit 0
fi
cleanup() {
  [ -z "$tmp" ] || rm -rf "$tmp"
  if [ "$HELD_DEPLOY_LOCK" -eq 1 ]; then
    rm -f "$DEPLOY_LOCK_DIR/owner" 2>/dev/null || true
    rmdir "$DEPLOY_LOCK_DIR" 2>/dev/null || true
  fi
  release_subrouter_mutation_lease
}
trap cleanup EXIT

if [ -e "$UPGRADE_INHIBIT_FILE" ]; then
  # The sentinel is also how an operator or subrouter-guard.sh pins the worker
  # after a rollback, so print why rather than assuming a live transaction.
  log "worker update deferred: $(sed -n '1p' "$UPGRADE_INHIBIT_FILE" 2>/dev/null || echo "$UPGRADE_INHIBIT_FILE exists")"
  exit 0
fi

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

RELEASE_API_URL="${SUBROUTER_RELEASE_API_URL:-https://api.github.com/repos/${REPO}/releases/latest}"
RELEASE_LATEST_URL="${SUBROUTER_RELEASE_LATEST_URL:-https://github.com/${REPO}/releases/latest}"

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
[ "$latest_tag" != "$installed" ] || exit 0

version="${latest_tag#v}"
asset="subrouter_${version}_darwin_${arch}"
base="${RELEASE_DOWNLOAD_URL}/${latest_tag}"
tmp="$(mktemp -d)"
backup_label="$(printf '%s' "${installed%% *}" | sed 's/:/-/g' | tr -c 'A-Za-z0-9._+-' '_')"
mkdir -p "$BACKUP_DIR"
backup="${BACKUP_DIR}/$(python3 -c 'import time; print(time.time_ns())')_${backup_label:-unknown}"

log "updating worker ${installed:-none} -> ${latest_tag} (${asset})"
curl -fsSL -o "${tmp}/${asset}" "${base}/${asset}"
curl -fsSL -o "${tmp}/SHA256SUMS" "${base}/SHA256SUMS"
(cd "$tmp" && grep " ${asset}\$" SHA256SUMS | shasum -a 256 -c -)
chmod 0755 "${tmp}/${asset}"
"${tmp}/${asset}" --help >/dev/null

mkdir -p "$(dirname "$DEPLOY_LOCK_DIR")"
if ! mkdir "$DEPLOY_LOCK_DIR" 2>/dev/null; then
  if [ -n "$(find "$DEPLOY_LOCK_DIR" -maxdepth 0 -mmin -30 2>/dev/null)" ]; then
    log "$(sed -n '1p' "$DEPLOY_LOCK_DIR/owner" 2>/dev/null | grep . || echo "a deploy") holds $DEPLOY_LOCK_DIR; worker update deferred"
    exit 0
  fi
  log "clearing stale deploy lock $DEPLOY_LOCK_DIR"
  rm -f "$DEPLOY_LOCK_DIR/owner"
  rmdir "$DEPLOY_LOCK_DIR" 2>/dev/null || true
  mkdir "$DEPLOY_LOCK_DIR" 2>/dev/null || { log "cannot take $DEPLOY_LOCK_DIR; worker update deferred"; exit 0; }
fi
HELD_DEPLOY_LOCK=1
printf 'subrouter-autoupdate.sh pid %s\n' "$$" >"$DEPLOY_LOCK_DIR/owner"

if [ -e "$UPGRADE_INHIBIT_FILE" ]; then
  log "deployment transaction began while preparing the update; worker update deferred"
  exit 0
fi

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
find "$BACKUP_DIR" -maxdepth 1 -type f -name '[0-9]*_*' 2>/dev/null | LC_ALL=C sort -r \
  | tail -n +"$((KEEP_BACKUPS + 1))" | while IFS= read -r old; do rm -f "$old"; done || true
log "updated to ${latest_tag}; active generation=${active}; old connections are draining"
