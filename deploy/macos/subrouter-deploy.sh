#!/usr/bin/env bash
# subrouter-deploy.sh installs a worker binary onto a supervised host without
# ever closing the public listener.
#
# Why this exists: on 2026-09-04 a hand-built worker was copied over
# /usr/local/bin/subrouter and the LaunchDaemon was booted out and bootstrapped
# to pick it up. That binary needed longer than the supervisor's 30s
# --ready-timeout to answer /_subrouter/ready, so `supervise` aborted before it
# bound 0.0.0.0:31415, launchd restarted it, and the whole team lost the proxy
# for seven minutes. A supervisor restart makes worker readiness fatal.
#
# The supervisor already has a failure-atomic upgrade path: replace the worker
# binary, then POST /_subrouter/upgrade on the control socket. The supervisor
# starts the new generation behind the still-bound listener, waits for its
# readiness, and keeps serving from the old generation if the new one never
# becomes ready. subrouter-autoupdate.sh has always used it. This script gives
# operators and agents the same path for a binary that did not come from a
# release, so the only way to lose the listener is to bypass this script.
set -euo pipefail

BIN="${SUBROUTER_BIN:-/usr/local/bin/subrouter}"
LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter-team}"
PLIST="${SUBROUTER_PLIST:-/Library/LaunchDaemons/${LABEL}.plist}"
STATE="${SUBROUTER_DEPLOY_STATE:-/var/lib/subrouter-verify}"
LAST_GOOD="${SUBROUTER_LAST_GOOD:-${STATE}/subrouter.last-good}"
VERSION_FILE="${SUBROUTER_VERSION_FILE:-/etc/subrouter-version}"
HEALTH_URL="${SUBROUTER_HEALTH_URL:-http://127.0.0.1:31415/_subrouter/health}"
UPGRADE_INHIBIT_FILE="${SUBROUTER_UPGRADE_INHIBIT_FILE:-${PLIST}.supervisor-transaction/upgrade-inhibited}"
HEALTH_TIMEOUT_SECS="${SUBROUTER_DEPLOY_HEALTH_TIMEOUT_SECS:-45}"
LOCK_DIR="${SUBROUTER_DEPLOY_LOCK_DIR:-${STATE}/deploy.lock}"

log() { printf 'subrouter-deploy: %s\n' "$*" >&2; }
die() { log "$*"; exit 1; }

usage() {
  cat <<'EOF'
Usage:
  subrouter-deploy.sh install <candidate-binary> [--label <version-text>]
  subrouter-deploy.sh rollback
  subrouter-deploy.sh status

install   Hot-swap the worker behind the live listener and roll back by itself
          if the candidate never becomes ready or public health drops.
rollback  Put the recorded last-good worker back the same way.
status    Print the live binary, the recorded last-good, and health.

Never run `launchctl bootout` on the subrouter LaunchDaemon to pick up a new
worker. A restart turns a slow or broken worker into a total outage, because
the supervisor binds the public port only after the first worker is ready.
EOF
}

control_socket() {
  if [ -n "${SUBROUTER_CONTROL_SOCKET:-}" ]; then
    printf '%s\n' "$SUBROUTER_CONTROL_SOCKET"
    return
  fi
  [ -f "$PLIST" ] || die "cannot find $PLIST; set SUBROUTER_CONTROL_SOCKET"
  PLIST="$PLIST" python3 - <<'PY'
import os, plistlib
with open(os.environ["PLIST"], "rb") as stream:
    arguments = plistlib.load(stream).get("ProgramArguments") or []
for index, argument in enumerate(arguments):
    if argument == "--control-socket" and index + 1 < len(arguments):
        print(arguments[index + 1])
        break
    if argument.startswith("--control-socket="):
        print(argument.split("=", 1)[1])
        break
PY
}

sha_of() { [ -f "$1" ] && shasum -a 256 "$1" | awk '{print $1}' || echo "missing"; }

health_ok() { curl -fsS --max-time 5 "$HEALTH_URL" >/dev/null 2>&1; }

wait_health() {
  local deadline=$((SECONDS + HEALTH_TIMEOUT_SECS))
  while [ "$SECONDS" -lt "$deadline" ]; do
    health_ok && return 0
    sleep 1
  done
  return 1
}

request_upgrade() {
  local socket="$1"
  curl -fsS --max-time 120 --unix-socket "$socket" -X POST "http://localhost/_subrouter/upgrade"
}

restore_binary() {
  local source="$1"
  install -m 0755 "$source" "${BIN}.rollback"
  mv -f "${BIN}.rollback" "$BIN"
}

take_lock() {
  mkdir -p "$STATE"
  if ! mkdir "$LOCK_DIR" 2>/dev/null; then
    # A crashed deploy must not block the next one forever, but a live deploy
    # must not be joined by a second writer either.
    if [ -n "$(find "$LOCK_DIR" -maxdepth 0 -mmin -30 2>/dev/null)" ]; then
      die "another deploy holds $LOCK_DIR (started less than 30 minutes ago)"
    fi
    log "clearing stale lock $LOCK_DIR"
    rmdir "$LOCK_DIR" 2>/dev/null || true
    mkdir "$LOCK_DIR" 2>/dev/null || die "cannot take $LOCK_DIR"
  fi
  trap release_lock EXIT
}

# An operator or subrouter-guard.sh can pin worker autoupdate by writing the
# inhibit sentinel. A deploy must borrow that file, not consume it: clearing
# someone else's pin on exit would let the updater reinstall the release the
# pin exists to keep out.
HAD_INHIBIT=0
PREEXISTING_INHIBIT=""

release_lock() {
  if [ "$HAD_INHIBIT" -eq 1 ]; then
    printf '%s\n' "$PREEXISTING_INHIBIT" >"$UPGRADE_INHIBIT_FILE" 2>/dev/null || true
    chmod 0600 "$UPGRADE_INHIBIT_FILE" 2>/dev/null || true
  else
    rm -f "$UPGRADE_INHIBIT_FILE" 2>/dev/null || true
  fi
  rmdir "$LOCK_DIR" 2>/dev/null || true
}

inhibit_autoupdate() {
  mkdir -p "$(dirname "$UPGRADE_INHIBIT_FILE")"
  if [ -e "$UPGRADE_INHIBIT_FILE" ]; then
    HAD_INHIBIT=1
    PREEXISTING_INHIBIT="$(cat "$UPGRADE_INHIBIT_FILE" 2>/dev/null || true)"
    log "an autoupdate pin is in place; it will be restored when this deploy finishes"
  fi
  printf 'subrouter-deploy.sh running (pid %s)\n' "$$" >"$UPGRADE_INHIBIT_FILE"
  chmod 0600 "$UPGRADE_INHIBIT_FILE"
}

swap_and_verify() {
  # swap_and_verify <new-binary> <fallback-binary> <description>
  local candidate="$1" fallback="$2" description="$3"
  local socket
  socket="$(control_socket)"
  [ -S "$socket" ] || die "control socket $socket is not a socket; is ${LABEL} running?"

  install -m 0755 "$candidate" "${BIN}.new"
  mv -f "${BIN}.new" "$BIN"

  if ! request_upgrade "$socket" >/dev/null; then
    log "${description} never became ready; the old generation is still serving"
    restore_binary "$fallback"
    request_upgrade "$socket" >/dev/null 2>&1 || true
    return 1
  fi

  if ! wait_health; then
    log "${description} took the new generation but public health failed; restoring"
    restore_binary "$fallback"
    request_upgrade "$socket" >/dev/null 2>&1 || true
    wait_health || log "health is still down after restoring; check subrouter-guard.log"
    return 1
  fi
  return 0
}

cmd_install() {
  local candidate="${1:-}"
  shift || true
  local version_label=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --label) version_label="${2:-}"; shift 2 ;;
      *) die "unknown option $1" ;;
    esac
  done

  [ -n "$candidate" ] || { usage; exit 2; }
  [ -f "$candidate" ] || die "$candidate does not exist"
  [ -x "$candidate" ] || die "$candidate is not executable"
  "$candidate" --help >/dev/null 2>&1 || die "$candidate does not answer --help; wrong arch or a corrupt download"

  local candidate_sha current_sha
  candidate_sha="$(sha_of "$candidate")"
  current_sha="$(sha_of "$BIN")"
  [ "$candidate_sha" != "$current_sha" ] || { log "candidate is already installed ($candidate_sha)"; exit 0; }

  health_ok || die "public health is down right now; fix the outage before installing (subrouter-deploy.sh rollback, or check /var/log/subrouter-guard.log)"

  take_lock
  inhibit_autoupdate

  # The binary that is serving traffic right now is by definition good.
  mkdir -p "$(dirname "$LAST_GOOD")"
  cp -p "$BIN" "$LAST_GOOD"
  # The rollback source is this private copy, never $LAST_GOOD. That file is
  # shared with subrouter-guard.sh, which promotes whatever binary is answering
  # health; between the swap below and the generation switch the old worker is
  # still serving, so a guard tick there would record the untested candidate as
  # last-good and this rollback would "restore" the candidate over itself.
  local backup="${BIN}.backup-$(date +%Y%m%d-%H%M%S)"
  cp -p "$BIN" "$backup"
  log "current worker ${current_sha:0:12} saved to $LAST_GOOD and $backup"

  if ! swap_and_verify "$candidate" "$backup" "candidate ${candidate_sha:0:12}"; then
    die "install failed and the previous worker was restored; the listener never dropped"
  fi

  printf '%s\n' "${version_label:-local:${candidate_sha:0:12}}" >"${VERSION_FILE}.new"
  mv -f "${VERSION_FILE}.new" "$VERSION_FILE"
  log "installed ${candidate_sha:0:12}; old connections are draining"
  if [ -z "$version_label" ]; then
    log "note: /etc/subrouter-version now reads local:${candidate_sha:0:12}, so subrouter-autoupdate.sh will replace this build with the next release"
  fi
}

cmd_rollback() {
  [ -f "$LAST_GOOD" ] || die "no recorded last-good worker at $LAST_GOOD"
  local good_sha current_sha
  good_sha="$(sha_of "$LAST_GOOD")"
  current_sha="$(sha_of "$BIN")"
  [ "$good_sha" != "$current_sha" ] || { log "last-good is already installed ($good_sha)"; exit 0; }
  take_lock
  inhibit_autoupdate
  local rollback_from="${BIN}.rejected-$(date +%Y%m%d-%H%M%S)"
  cp -p "$BIN" "$rollback_from"
  if ! swap_and_verify "$LAST_GOOD" "$rollback_from" "last-good ${good_sha:0:12}"; then
    die "rollback could not take effect through the control socket; the service may be restart-looping, see subrouter-guard.sh"
  fi
  printf '%s\n' "rollback:${good_sha:0:12}" >"${VERSION_FILE}.new"
  mv -f "${VERSION_FILE}.new" "$VERSION_FILE"
  log "rolled back to ${good_sha:0:12}"
}

cmd_status() {
  printf 'live      %s %s\n' "$BIN" "$(sha_of "$BIN")"
  printf 'last-good %s %s\n' "$LAST_GOOD" "$(sha_of "$LAST_GOOD")"
  printf 'version   %s\n' "$(cat "$VERSION_FILE" 2>/dev/null || echo unknown)"
  if health_ok; then printf 'health    ok\n'; else printf 'health    DOWN\n'; fi
}

case "${1:-}" in
  install) shift; cmd_install "$@" ;;
  rollback) shift; cmd_rollback "$@" ;;
  status) shift; cmd_status "$@" ;;
  -h|--help|help|"") usage ;;
  *) usage; exit 2 ;;
esac
