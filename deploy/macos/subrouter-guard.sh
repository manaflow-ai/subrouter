#!/usr/bin/env bash
# subrouter-guard.sh is the fast outage watchdog for the supervised proxy.
#
# subrouter-verify.sh already self-heals a service that was booted out or
# wedged, but it runs every five minutes and its recovery is a kickstart. A
# kickstart cannot fix the failure that actually took the fleet down on
# 2026-09-04: a worker binary that never answers /_subrouter/ready inside the
# supervisor's --ready-timeout makes `supervise` exit before it binds the
# public port, so launchd restarts it every 30 seconds and the port stays
# closed no matter how often it is kicked. The only recovery is to put the
# previous worker binary back.
#
# So this job does two things a health check alone cannot:
#   - while health is good, it records the serving binary as last-good
#   - while health is down, it restores that binary and restarts the service
#
# It runs every 60 seconds and acts on the second consecutive failure, which
# bounds a bad-worker outage at about two minutes without reacting to a single
# transient probe failure.
set -uo pipefail

LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter-team}"
PLIST="${SUBROUTER_PLIST:-/Library/LaunchDaemons/${LABEL}.plist}"
BIN="${SUBROUTER_BIN:-/usr/local/bin/subrouter}"
SUPERVISOR_BIN="${SUBROUTER_SUPERVISOR_BIN:-/usr/local/libexec/subrouter-supervisor}"
STATE="${SUBROUTER_VERIFY_STATE:-/var/lib/subrouter-verify}"
LAST_GOOD="${SUBROUTER_LAST_GOOD:-${STATE}/subrouter.last-good}"
HEALTH="${SUBROUTER_HEALTH_URL:-http://127.0.0.1:31415/_subrouter/health}"
ALERTS="${SUBROUTER_ALERTS_FILE:-${STATE}/alerts.log}"
HEARTBEAT="${SUBROUTER_GUARD_HEARTBEAT:-${STATE}/guard.heartbeat}"
STRIKES_FILE="${STATE}/guard.strikes"
MAINTENANCE="${STATE}/maintenance"
STRIKE_THRESHOLD="${SUBROUTER_GUARD_STRIKE_THRESHOLD:-2}"
PROBE_TIMEOUT_SECS="${SUBROUTER_GUARD_PROBE_TIMEOUT_SECS:-4}"
RESTART_WAIT_SECS="${SUBROUTER_GUARD_RESTART_WAIT_SECS:-10}"
HEALTH_WAIT_SECS="${SUBROUTER_GUARD_HEALTH_WAIT_SECS:-45}"
# Injectable so the test can assert on launchd calls without touching launchd.
LAUNCHCTL="${SUBROUTER_LAUNCHCTL:-launchctl}"
UPGRADE_INHIBIT_FILE="${SUBROUTER_UPGRADE_INHIBIT_FILE:-${PLIST}.supervisor-transaction/upgrade-inhibited}"
DEPLOY_LOCK_DIR="${SUBROUTER_DEPLOY_LOCK_DIR:-${STATE}/deploy.lock}"
DEPLOY_LOCK_GRACE_MINS="${SUBROUTER_GUARD_DEPLOY_LOCK_GRACE_MINS:-5}"
GUARD_LOCK_DIR="${SUBROUTER_GUARD_LOCK_DIR:-${STATE}/guard.lock}"
GUARD_LOCK_STALE_MINS="${SUBROUTER_GUARD_LOCK_STALE_MINS:-10}"
# A maintenance sentinel silences recovery for 90 minutes, which is right while
# a human is working on the service. It is wrong when the service is not in the
# launchd domain at all: that is what an interrupted `bootout` leaves behind,
# and on 2026-09-04 it kept the router down with the watchdog muzzled. A hand
# restart passes through that state for seconds, so the sentinel still wins for
# a short grace, and after it the guard bootstraps a service nobody is running.
MISSING_SERVICE_GRACE_MINS="${SUBROUTER_GUARD_MISSING_SERVICE_GRACE_MINS:-3}"

mkdir -p "$STATE"
now_iso="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

emit() { # level msg...
  local level="$1"; shift
  local line="$now_iso [$level] $*"
  echo "SUBROUTER-GUARD $line"
  echo "guard $line" >>"$ALERTS" 2>/dev/null || true
}

probe_health() { curl -fsS --max-time "$PROBE_TIMEOUT_SECS" "$HEALTH" >/dev/null 2>&1; }

wait_health() {
  local deadline=$((SECONDS + HEALTH_WAIT_SECS))
  while [ "$SECONDS" -lt "$deadline" ]; do
    probe_health && return 0
    sleep 2
  done
  return 1
}

sha_of() { [ -f "$1" ] && shasum -a 256 "$1" | awk '{print $1}' || echo "missing"; }

read_strikes() {
  local value
  value="$(cat "$STRIKES_FILE" 2>/dev/null || echo 0)"
  case "$value" in ''|*[!0-9]*) value=0 ;; esac
  printf '%s' "$value"
}

# Exact-path pids only. A pattern like `pkill -f subrouter` on a shared box
# kills unrelated sessions, and this job runs as root.
supervisor_pids() { pgrep -f "^${SUPERVISOR_BIN} supervise" 2>/dev/null | tr '\n' ' '; }
worker_pids() { pgrep -f "^${BIN} serve " 2>/dev/null | tr '\n' ' '; }

restart_service() {
  # A restart-looping supervisor has to be removed from the launchd domain and
  # confirmed dead before bootstrap, or bootstrap fails with
  # "Input/output error" while the old job drains under its ExitTimeOut.
  "$LAUNCHCTL" bootout "system/${LABEL}" >/dev/null 2>&1 || true
  local deadline=$((SECONDS + RESTART_WAIT_SECS))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ -z "$(supervisor_pids)$(worker_pids)" ] && break
    sleep 1
  done
  local leftover
  leftover="$(supervisor_pids)$(worker_pids)"
  if [ -n "${leftover// /}" ]; then
    emit ALERT "force killing draining pids: ${leftover}"
    # shellcheck disable=SC2086
    kill -KILL ${leftover} 2>/dev/null || true
    sleep 2
  fi
  "$LAUNCHCTL" bootstrap system "$PLIST" >/dev/null 2>&1 || true
}

: >"$HEARTBEAT"

# One actor at a time. launchd will not overlap this job with itself, but an
# operator running it by hand during a scheduled run would double-restart the
# service, and this job kills processes.
if ! mkdir "$GUARD_LOCK_DIR" 2>/dev/null; then
  if [ -n "$(find "$GUARD_LOCK_DIR" -maxdepth 0 -mmin "-${GUARD_LOCK_STALE_MINS}" 2>/dev/null)" ]; then
    emit INFO "another subrouter-guard run holds $GUARD_LOCK_DIR"
    exit 0
  fi
  emit ALERT "clearing stale guard lock $GUARD_LOCK_DIR"
  rmdir "$GUARD_LOCK_DIR" 2>/dev/null || true
  mkdir "$GUARD_LOCK_DIR" 2>/dev/null || { emit ALERT "cannot take $GUARD_LOCK_DIR"; exit 0; }
fi
trap 'rmdir "$GUARD_LOCK_DIR" 2>/dev/null || true' EXIT

# subrouter-deploy.sh owns the outcome while it runs: it swaps the binary,
# waits for the new generation, and reverts on its own. A guard tick inside
# that window would either record the untested candidate as last-good or
# restart the service under the deploy, so stand down and say so.
if [ -d "$DEPLOY_LOCK_DIR" ] && [ -n "$(find "$DEPLOY_LOCK_DIR" -maxdepth 0 -mmin "-${DEPLOY_LOCK_GRACE_MINS}" 2>/dev/null)" ]; then
  emit INFO "subrouter-deploy.sh holds $DEPLOY_LOCK_DIR; standing down this cycle"
  exit 0
fi

if probe_health; then
  rm -f "$STRIKES_FILE"
  live_sha="$(sha_of "$BIN")"
  good_sha="$(sha_of "$LAST_GOOD")"
  if [ "$live_sha" != "missing" ] && [ "$live_sha" != "$good_sha" ]; then
    # The binary answering health right now is the one worth keeping.
    mkdir -p "$(dirname "$LAST_GOOD")"
    if cp -p "$BIN" "${LAST_GOOD}.new" && mv -f "${LAST_GOOD}.new" "$LAST_GOOD"; then
      emit INFO "recorded last-good worker ${live_sha:0:12}"
    else
      rm -f "${LAST_GOOD}.new"
      emit ALERT "could not record last-good worker ${live_sha:0:12}"
    fi
  fi
  exit 0
fi

strikes=$(( $(read_strikes) + 1 ))
printf '%s\n' "$strikes" >"$STRIKES_FILE"

if [ -e "$MAINTENANCE" ] && [ -n "$(find "$MAINTENANCE" -mmin -90 2>/dev/null)" ]; then
  if "$LAUNCHCTL" print "system/${LABEL}" >/dev/null 2>&1 ||
     [ -n "$(find "$MAINTENANCE" -mmin "-${MISSING_SERVICE_GRACE_MINS}" 2>/dev/null)" ]; then
    emit INFO "health down (strike ${strikes}); fresh maintenance sentinel, no recovery"
    exit 0
  fi
  emit ALERT "maintenance sentinel is set but ${LABEL} is not in the launchd domain after ${MISSING_SERVICE_GRACE_MINS}m; bootstrapping anyway"
  [ -f "$PLIST" ] && "$LAUNCHCTL" bootstrap system "$PLIST" >/dev/null 2>&1
  if wait_health; then
    emit INFO "recovery succeeded: health answering again"
    rm -f "$STRIKES_FILE"
  else
    emit ALERT "bootstrap under maintenance did not restore health; this needs a human"
  fi
  exit 0
fi

if [ "$strikes" -lt "$STRIKE_THRESHOLD" ]; then
  emit ALERT "health down (strike ${strikes}/${STRIKE_THRESHOLD}); acting next cycle if it persists"
  exit 0
fi

live_sha="$(sha_of "$BIN")"
good_sha="$(sha_of "$LAST_GOOD")"

if [ "$good_sha" != "missing" ] && [ "$live_sha" != "$good_sha" ]; then
  emit ALERT "health down ${strikes} cycles with worker ${live_sha:0:12}; rolling back to last-good ${good_sha:0:12}"
  cp -p "$BIN" "${BIN}.rejected-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
  # Stop subrouter-autoupdate.sh before it reinstalls the binary we are about
  # to remove. Without this, a bad release flaps: updater installs it, guard
  # rolls it back, updater installs it again two minutes later. Worker updates
  # stay paused until a human clears the sentinel, which is the safe direction
  # after an automatic rollback.
  mkdir -p "$(dirname "$UPGRADE_INHIBIT_FILE")" 2>/dev/null || true
  printf 'subrouter-guard.sh rolled back worker %s at %s; clear this file after review\n' \
    "${live_sha:0:12}" "$now_iso" >"$UPGRADE_INHIBIT_FILE" 2>/dev/null || true
  chmod 0600 "$UPGRADE_INHIBIT_FILE" 2>/dev/null || true
  emit ALERT "worker autoupdate paused by $UPGRADE_INHIBIT_FILE until a human clears it"
  if install -m 0755 "$LAST_GOOD" "${BIN}.rollback" && mv -f "${BIN}.rollback" "$BIN"; then
    restart_service
  else
    rm -f "${BIN}.rollback"
    emit ALERT "could not write ${BIN}; rollback failed"
  fi
elif ! "$LAUNCHCTL" print "system/${LABEL}" >/dev/null 2>&1; then
  emit ALERT "health down ${strikes} cycles and ${LABEL} is not in the launchd domain; bootstrapping"
  [ -f "$PLIST" ] && "$LAUNCHCTL" bootstrap system "$PLIST" >/dev/null 2>&1
else
  emit ALERT "health down ${strikes} cycles on the last-good worker; restarting ${LABEL}"
  restart_service
fi

if wait_health; then
  emit INFO "recovery succeeded: health answering again"
  rm -f "$STRIKES_FILE"
else
  emit ALERT "recovery did not restore health; this needs a human"
fi
