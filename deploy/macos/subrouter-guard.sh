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
# Health alone cannot catch a release that starts fine and then breaks routing
# or streaming. While release-state.json says a new worker is "baking" (see
# release-bake-lib.sh), this job also compares the new generation's
# /_subrouter/traffic outcome ratios with the baseline taken from the outgoing
# generation, rolls a regression back and pins the previous release, and only
# records the new worker as last-good once the bake passes.
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
VERSION_FILE="${SUBROUTER_VERSION_FILE:-/etc/subrouter-version}"
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

# The bake gate lives next to this script. Without it the guard keeps its
# health-only behaviour rather than failing every tick.
SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
BAKE_LIB="${SUBROUTER_BAKE_LIB:-${SCRIPT_DIR}/release-bake-lib.sh}"
BAKE_GATE=0
if [ -f "$BAKE_LIB" ]; then
  # shellcheck disable=SC1090
  . "$BAKE_LIB" && BAKE_GATE=1
fi

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

# After a rollback the version marker must describe what is running, not the
# release that was just rejected. Left alone, subrouter-autoupdate.sh sees the
# rejected tag as "installed" and never retries it (even after a human clears
# the inhibit sentinel), and subrouter-verify.sh reports the wrong version.
# A "rollback:" label never equals a release tag, so the updater retries the
# release once the sentinel is cleared.
record_rollback_version() { # record_rollback_version <restored sha256>
  local previous
  previous="$(sed -n '1p' "$VERSION_FILE" 2>/dev/null || true)"
  previous="${previous:-unknown}"
  # Keep the original release across repeated rollbacks instead of nesting.
  case "$previous" in
    rollback:*' (was '*')') previous="${previous#* (was }"; previous="${previous%)}" ;;
  esac
  mkdir -p "$(dirname "$VERSION_FILE")" 2>/dev/null || true
  if printf 'rollback:%s (was %s)\n' "${1:0:12}" "$previous" >"${VERSION_FILE}.new" 2>/dev/null &&
     mv -f "${VERSION_FILE}.new" "$VERSION_FILE"; then
    emit INFO "version marker now reads rollback:${1:0:12} (was ${previous})"
  else
    rm -f "${VERSION_FILE}.new" 2>/dev/null || true
    emit ALERT "could not update $VERSION_FILE after rollback; it still names ${previous}"
  fi
}

# pin_after_rollback <text>: stop subrouter-autoupdate.sh from reinstalling
# the worker that was just removed.
pin_after_rollback() {
  mkdir -p "$(dirname "$UPGRADE_INHIBIT_FILE")" 2>/dev/null || true
  printf '%s\n' "$1" >"$UPGRADE_INHIBIT_FILE" 2>/dev/null || true
  chmod 0600 "$UPGRADE_INHIBIT_FILE" 2>/dev/null || true
  emit ALERT "worker autoupdate paused by $UPGRADE_INHIBIT_FILE until a human clears it"
}

# Only a real tag may become the version marker after a bake rollback;
# anything else gets the rollback:<sha> label.
is_release_tag() { [[ "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; }

# hot_swap_back asks the supervisor for a new generation from the binary now
# on disk, so a bake rollback never closes the listener. A restart is the
# fallback when the control socket does not answer.
hot_swap_back() {
  local socket
  socket="$(bake_control_socket)"
  if [ -n "$socket" ] && [ -S "$socket" ] &&
     curl -fsS --max-time 120 --unix-socket "$socket" -X POST "http://localhost/_subrouter/upgrade" >/dev/null 2>&1; then
    emit INFO "supervisor switched to the restored worker; the listener stayed up"
    return 0
  fi
  emit ALERT "control socket ${socket:-unknown} did not take the restored worker; restarting ${LABEL}"
  restart_service
}

# bake_rollback <reason>: a baking release regressed while health still
# answers. Put last-good back behind the live listener and pin it.
bake_rollback() {
  local reason="$1" version previous live_sha good_sha
  version="$(bake_state_field version)"
  previous="$(bake_state_field previous_version)"
  live_sha="$(sha_of "$BIN")"
  good_sha="$(sha_of "$LAST_GOOD")"
  if [ "$good_sha" = "missing" ] || [ "$good_sha" = "$live_sha" ]; then
    emit ALERT "bake gate: ${version:-the new worker} regressed (${reason}) but there is no different last-good worker to restore; this needs a human"
    return 1
  fi
  emit ALERT "bake gate: rolling back ${version:-worker ${live_sha:0:12}} to ${previous:-last-good ${good_sha:0:12}}: ${reason}"
  cp -p "$BIN" "${BIN}.rejected-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
  pin_after_rollback "pinned at ${previous:-rollback:${good_sha:0:12}} by subrouter-guard.sh bake gate: rolled back ${version:-worker ${live_sha:0:12}} at ${now_iso} (${reason}); clear with subrouter-deploy.sh unpin"
  if ! { install -m 0755 "$LAST_GOOD" "${BIN}.rollback" && mv -f "${BIN}.rollback" "$BIN"; }; then
    rm -f "${BIN}.rollback"
    emit ALERT "could not write ${BIN}; bake rollback failed"
    return 1
  fi
  if is_release_tag "$previous" &&
     printf '%s\n' "$previous" >"${VERSION_FILE}.new" 2>/dev/null &&
     mv -f "${VERSION_FILE}.new" "$VERSION_FILE"; then
    emit INFO "version marker now reads ${previous}"
  else
    rm -f "${VERSION_FILE}.new" 2>/dev/null || true
    record_rollback_version "$good_sha"
  fi
  bake_mark rolled_back "$reason"
  hot_swap_back
  if wait_health; then
    emit INFO "bake rollback done: ${previous:-last-good} is serving"
  else
    emit ALERT "health did not answer after the bake rollback; the health-down path takes over next cycle"
  fi
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
HELD_DEPLOY_LOCK=0
release_locks() {
  if [ "$HELD_DEPLOY_LOCK" -eq 1 ]; then
    rm -f "$DEPLOY_LOCK_DIR/owner" 2>/dev/null || true
    rmdir "$DEPLOY_LOCK_DIR" 2>/dev/null || true
  fi
  rmdir "$GUARD_LOCK_DIR" 2>/dev/null || true
}
trap release_locks EXIT

# subrouter-deploy.sh and subrouter-autoupdate.sh own the outcome while they
# hold the deploy lock: they swap the binary, wait for the new generation, and
# revert on their own. A guard tick inside that window would either record the
# untested candidate as last-good or restart the service under them, so stand
# down and say so. Otherwise the guard takes the same lock for this tick, so
# neither can start a swap between its health probe and its promotion.
if mkdir "$DEPLOY_LOCK_DIR" 2>/dev/null; then
  HELD_DEPLOY_LOCK=1
  printf 'subrouter-guard.sh pid %s\n' "$$" >"$DEPLOY_LOCK_DIR/owner" 2>/dev/null || true
elif [ -n "$(find "$DEPLOY_LOCK_DIR" -maxdepth 0 -mmin "-${DEPLOY_LOCK_GRACE_MINS}" 2>/dev/null)" ]; then
  emit INFO "$(sed -n '1p' "$DEPLOY_LOCK_DIR/owner" 2>/dev/null | grep . || echo subrouter-deploy.sh) holds $DEPLOY_LOCK_DIR; standing down this cycle"
  exit 0
elif [ -d "$DEPLOY_LOCK_DIR" ]; then
  emit ALERT "$DEPLOY_LOCK_DIR is older than ${DEPLOY_LOCK_GRACE_MINS}m; acting without it"
fi

if probe_health; then
  rm -f "$STRIKES_FILE"
  if [ "$BAKE_GATE" -eq 1 ] && bake_is_baking; then
    decision="$(bake_evaluate "$(bake_fetch_traffic)" 2>/dev/null || true)"
    action="${decision%%$'\t'*}"
    reason="${decision#*$'\t'}"
    case "$action" in
      rollback)
        bake_rollback "$reason"
        exit 0
        ;;
      promote)
        live_sha="$(sha_of "$BIN")"
        mkdir -p "$(dirname "$LAST_GOOD")"
        if cp -p "$BIN" "${LAST_GOOD}.new" && mv -f "${LAST_GOOD}.new" "$LAST_GOOD"; then
          bake_mark promoted "$reason"
          emit INFO "bake gate: promoted $(bake_state_field version) (${live_sha:0:12}) to last-good: ${reason}"
        else
          rm -f "${LAST_GOOD}.new"
          emit ALERT "bake gate: could not record last-good worker ${live_sha:0:12}; still baking"
        fi
        exit 0
        ;;
      continue)
        emit INFO "bake gate: $(bake_state_field version) baking: ${reason}"
        exit 0
        ;;
      *)
        # An unreadable state file must not promote an unbaked worker.
        emit ALERT "bake gate: could not evaluate $RELEASE_STATE_FILE; last-good left unchanged"
        exit 0
        ;;
    esac
  fi
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
    record_rollback_version "$good_sha"
    if [ "$BAKE_GATE" -eq 1 ] && bake_is_baking; then
      bake_mark rolled_back "health down ${strikes} consecutive checks"
    fi
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
