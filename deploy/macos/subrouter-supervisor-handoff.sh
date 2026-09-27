#!/usr/bin/env bash
# subrouter-supervisor-handoff.sh replaces the supervisor (or its plist) on a
# macOS host without closing the public port and without cutting streams.
#
# A supervisor owns the listener, so replacing it used to mean a launchd
# bootout and bootstrap: about a minute of refused connections and every
# in-flight stream cut (2026-09-22). macOS has no pidfd_getfd, so the old
# process cannot hand its listener over. pf can hand over the traffic instead:
#
#   1. start a bridge supervisor, as its own LaunchDaemon, on a private port;
#   2. load an rdr anchor that sends NEW connections for the public port to the
#      bridge. Existing connections keep their pf state and stay on the old
#      supervisor, because state lookup happens before translation;
#   3. boot out the old job. On SIGTERM it closes its listener and drains its
#      streams (up to --drain-timeout); launchd waits for it;
#   4. bootstrap the job with the new supervisor/plist. It binds the public
#      port while pf still sends new connections to the bridge, and it is
#      probed through an rdr exception on a reserved source port;
#   5. drop the rdr; new connections reach the new supervisor, the bridge's
#      connections keep their translated state;
#   6. boot out the bridge, which drains the same way, and remove it.
#
# If step 4 fails, the old binary and plist are restored and bootstrapped again
# while the bridge still serves, so a bad candidate never closes the port.
#
# Invoked detached by `subrouter-deploy.sh handoff-supervisor`. Progress goes to
# stdout; the final line is "HANDOFF OK" or "HANDOFF FAILED: <reason>".
set -uo pipefail

LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter-team}"
PLIST="${SUBROUTER_PLIST:-/Library/LaunchDaemons/${LABEL}.plist}"
SUPERVISOR_BIN="${SUBROUTER_SUPERVISOR_BIN:-/usr/local/libexec/subrouter-supervisor}"
BIN="${SUBROUTER_BIN:-/usr/local/bin/subrouter}"
LAUNCHCTL="${SUBROUTER_LAUNCHCTL:-launchctl}"
PFCTL="${SUBROUTER_PFCTL:-pfctl}"
PF_ANCHOR="${SUBROUTER_HANDOFF_PF_ANCHOR:-com.apple/subrouter-handoff}"
BRIDGE_LABEL="${SUBROUTER_HANDOFF_BRIDGE_LABEL:-${LABEL}.handoff}"
BRIDGE_PLIST="${SUBROUTER_HANDOFF_BRIDGE_PLIST:-/Library/LaunchDaemons/${BRIDGE_LABEL}.plist}"
# Distinct paths, so nothing that matches the real supervisor or worker by path
# (restart-daemon, the guard) can touch the bridge.
BRIDGE_SUPERVISOR="${SUPERVISOR_BIN}-handoff"
BRIDGE_WORKER="${BIN}-handoff"
PROBE_PORTS="${SUBROUTER_HANDOFF_PROBE_PORTS:-45990-45999}"
STEP_TIMEOUT_SECS="${SUBROUTER_HANDOFF_STEP_TIMEOUT_SECS:-90}"
DRAIN_TIMEOUT_SECS="${SUBROUTER_HANDOFF_DRAIN_TIMEOUT_SECS:-660}"
BOOTSTRAP_TIMEOUT_SECS="${SUBROUTER_HANDOFF_BOOTSTRAP_TIMEOUT_SECS:-300}"

CANDIDATE_SUPERVISOR="${1:?candidate supervisor binary}"
CANDIDATE_PLIST="${2:?candidate plist}"
MAINTENANCE="${SUBROUTER_MAINTENANCE_FILE:-/var/lib/subrouter-verify/maintenance}"
# The caller sets the sentinel so the watchdogs stand down; this detached script
# outlives the caller and clears it on every exit path.
trap 'rm -f "$MAINTENANCE" 2>/dev/null || true' EXIT

say() { printf '%s handoff: %s\n' "$(date -u +%H:%M:%S)" "$*"; }
fail() { say "$*"; printf 'HANDOFF FAILED: %s\n' "$*"; exit 1; }

plist_value() { # plist_value <plist> <flag>: value after <flag> in ProgramArguments
  python3 - "$1" "$2" <<'PY'
import plistlib, sys
with open(sys.argv[1], "rb") as stream:
    arguments = plistlib.load(stream).get("ProgramArguments") or []
for index, argument in enumerate(arguments):
    if argument == "--":
        break
    if argument == sys.argv[2] and index + 1 < len(arguments):
        print(arguments[index + 1]); break
    if argument.startswith(sys.argv[2] + "="):
        print(argument.split("=", 1)[1]); break
PY
}

PUBLIC_ADDR="$(plist_value "$PLIST" --addr)"
PUBLIC_PORT="${PUBLIC_ADDR##*:}"
case "$PUBLIC_ADDR" in
  :*|0.0.0.0:*|"[::]:"*) ;;
  *) fail "public --addr $PUBLIC_ADDR is not a wildcard; the handoff redirects wildcard listeners only" ;;
esac
BRIDGE_PORT="${SUBROUTER_HANDOFF_BRIDGE_PORT:-$((PUBLIC_PORT + 1))}"
CONTROL_SOCKET="$(plist_value "$PLIST" --control-socket)"
BRIDGE_CONTROL="${CONTROL_SOCKET%.sock}-handoff.sock"
NEW_CONTROL="$(plist_value "$CANDIDATE_PLIST" --control-socket)"
[ -n "$PUBLIC_PORT" ] && [ -n "$CONTROL_SOCKET" ] && [ -n "$NEW_CONTROL" ] || fail "cannot read --addr/--control-socket from the plists"

status_ok() { # status_ok <control-socket>: accepting with an active worker
  curl -fsS --max-time 5 --unix-socket "$1" http://localhost/_subrouter/supervisor-status 2>/dev/null \
    | python3 -c 'import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get("accepting") and (d.get("active") or {}).get("id") else 1)' 2>/dev/null
}
port_health() { # port_health <port> [curl args...]
  local port="$1"; shift
  curl -fsS --max-time 5 "$@" "http://127.0.0.1:${port}/_subrouter/health" >/dev/null 2>&1
}
wait_for() { # wait_for <seconds> <description> <command...>
  local seconds="$1" description="$2"; shift 2
  local deadline=$((SECONDS + seconds))
  while [ "$SECONDS" -lt "$deadline" ]; do
    "$@" && return 0
    sleep 1
  done
  say "timed out after ${seconds}s waiting for ${description}"
  return 1
}
job_loaded() { "$LAUNCHCTL" print "system/$1" >/dev/null 2>&1; }
job_pid() { "$LAUNCHCTL" print "system/$1" 2>/dev/null | awk '/^\tpid = /{print $3; exit}'; }

# Every address this host serves the public port on: loopback plus the tailnet
# addresses (100.64.0.0/10, fd7a:115c:a1e0::/48). pf already blocks the port
# from anywhere else, so those are the only addresses clients can reach.
redirect_rules() {
  ifconfig -a | python3 -c '
import ipaddress, re, sys
public, bridge, probe = sys.argv[1:4]
tail4 = ipaddress.ip_network("100.64.0.0/10")
tail6 = ipaddress.ip_network("fd7a:115c:a1e0::/48")
iface, targets = None, []
for line in sys.stdin:
    m = re.match(r"^(\S+): flags", line)
    if m:
        iface = m.group(1); continue
    m = re.match(r"^\s+inet6? (\S+)", line)
    if not m or iface is None:
        continue
    raw = m.group(1).split("%")[0]
    try:
        ip = ipaddress.ip_address(raw)
    except ValueError:
        continue
    if ip.is_loopback or ip in tail4 or ip in tail6:
        targets.append((iface, ip))
# The probe exception lets this script reach the new supervisor on the public
# port while every other new connection still goes to the bridge.
for fam in ("inet", "inet6"):
    print(f"no rdr on lo0 {fam} proto tcp from any port {probe} to any port {public}")
seen = set()
for iface, ip in targets:
    fam = "inet6" if ip.version == 6 else "inet"
    for on in {iface, "lo0"}:
        key = (on, str(ip))
        if key in seen:
            continue
        seen.add(key)
        print(f"rdr pass on {on} {fam} proto tcp from any to {ip} port {public} -> {ip} port {bridge}")
# Nobody may reach the bridge directly except through the redirect.
print(f"block drop in quick on ! lo0 proto tcp from any to any port {bridge}")
' "$PUBLIC_PORT" "$BRIDGE_PORT" "${PROBE_PORTS/-/:}"
}
load_redirect() {
  local rules
  rules="$(redirect_rules)" || return 1
  grep -q '^rdr pass' <<<"$rules" || { say "found no loopback or tailnet address to redirect"; return 1; }
  printf '%s\n' "$rules" | "$PFCTL" -a "$PF_ANCHOR" -f - 2>/dev/null || return 1
  "$PFCTL" -a "$PF_ANCHOR" -s nat 2>/dev/null | grep -q "port ${BRIDGE_PORT}"
}
# Load an empty ruleset into the anchor. Never `pfctl -F`: flushing states would
# cut every connection that pf already translated to the bridge.
drop_redirect() { printf '' | "$PFCTL" -a "$PF_ANCHOR" -f - >/dev/null 2>&1; }
redirect_active() { "$PFCTL" -a "$PF_ANCHOR" -s nat 2>/dev/null | grep -q "port ${BRIDGE_PORT}"; }

write_bridge_plist() {
  python3 - "$PLIST" "$BRIDGE_PLIST" "$BRIDGE_LABEL" "$BRIDGE_SUPERVISOR" "$BRIDGE_WORKER" "$BRIDGE_PORT" "$BRIDGE_CONTROL" "$CANDIDATE_PLIST" <<'PY'
import plistlib, sys
source, target, label, supervisor, worker, port, control, candidate = sys.argv[1:9]
with open(source, "rb") as stream:
    doc = plistlib.load(stream)
with open(candidate, "rb") as stream:
    # The bridge runs the candidate's supervisor flags, so it serves exactly
    # what the new job will serve.
    doc["ProgramArguments"] = list(plistlib.load(stream)["ProgramArguments"])
args = doc["ProgramArguments"]
args[0] = supervisor
doc["Program"] = supervisor
def replace(flag, value):
    for i, arg in enumerate(args):
        if arg == "--":
            break
        if arg == flag and i + 1 < len(args):
            args[i + 1] = value; return
        if arg.startswith(flag + "="):
            args[i] = flag + "=" + value; return
    raise SystemExit(f"{flag} missing from ProgramArguments")
replace("--addr", ":" + port)
replace("--control-socket", control)
replace("--worker-bin", worker)
doc["Label"] = label
doc["KeepAlive"] = True
doc["RunAtLoad"] = True
with open(target, "wb") as stream:
    plistlib.dump(doc, stream)
PY
}

remove_bridge() {
  "$LAUNCHCTL" bootout "system/${BRIDGE_LABEL}" >/dev/null 2>&1 || true
  wait_for "$DRAIN_TIMEOUT_SECS" "the bridge to drain and exit" bash -c "! $LAUNCHCTL print system/${BRIDGE_LABEL} >/dev/null 2>&1" || true
  rm -f "$BRIDGE_PLIST" "$BRIDGE_SUPERVISOR" "$BRIDGE_WORKER"
}

# Swap the main job to <supervisor> + <plist>, returning once it is loaded,
# accepting, and answering through the public port.
swap_main_job() { # swap_main_job <supervisor> <plist> <control-socket>
  local supervisor="$1" plist="$2" control="$3" old_pid
  old_pid="$(job_pid "$LABEL")"
  install -m 0755 "$supervisor" "${SUPERVISOR_BIN}.handoff-new" && mv -f "${SUPERVISOR_BIN}.handoff-new" "$SUPERVISOR_BIN" || return 1
  install -m 0644 "$plist" "${PLIST}.handoff-new" && mv -f "${PLIST}.handoff-new" "$PLIST" || return 1
  say "booting out ${LABEL} (pid ${old_pid:-none}); it drains its own streams while the bridge serves"
  "$LAUNCHCTL" bootout "system/${LABEL}" >/dev/null 2>&1 &
  local bootout_pid=$!
  # launchd drops the job from `print` before the process exits, and refuses a
  # bootstrap ("Input/output error") until it has. Wait for the process itself.
  if [ -n "$old_pid" ]; then
    wait_for "$DRAIN_TIMEOUT_SECS" "supervisor pid ${old_pid} to drain and exit" bash -c "! kill -0 $old_pid 2>/dev/null" || return 1
  fi
  # A job that never started (a crash-looping candidate) has no pid to wait
  # for, so also wait for bootout itself and for the job to leave the domain;
  # bootstrapping earlier fails while the old registration is still there.
  wait_for "$DRAIN_TIMEOUT_SECS" "launchctl bootout to finish" bash -c "! kill -0 $bootout_pid 2>/dev/null" || return 1
  wait "$bootout_pid" 2>/dev/null || true
  wait_for "$STEP_TIMEOUT_SECS" "${LABEL} to leave the launchd domain" bash -c "! $LAUNCHCTL print system/${LABEL} >/dev/null 2>&1" || return 1
  say "bootstrapping ${LABEL}"
  # launchd keeps refusing a bootstrap ("Input/output error") for a while after
  # a crash-looping job is booted out; the bridge serves meanwhile, so be patient.
  wait_for "$BOOTSTRAP_TIMEOUT_SECS" "${LABEL} to bootstrap" bash -c "$LAUNCHCTL bootstrap system '$PLIST' >/dev/null 2>&1; $LAUNCHCTL print system/${LABEL} >/dev/null 2>&1" || return 1
  wait_for "$STEP_TIMEOUT_SECS" "the new supervisor to accept" status_ok "$control" || return 1
  wait_for "$STEP_TIMEOUT_SECS" "the new supervisor to answer on :${PUBLIC_PORT}" port_health "$PUBLIC_PORT" --local-port "$PROBE_PORTS" || return 1
}

[ -x "$CANDIDATE_SUPERVISOR" ] || fail "$CANDIDATE_SUPERVISOR is not executable"
job_loaded "$LABEL" || fail "${LABEL} is not loaded; use restart-daemon for a service that is already down"
port_health "$PUBLIC_PORT" || fail "public health is down before the handoff"
if job_loaded "$BRIDGE_LABEL" || redirect_active; then
  fail "a previous handoff left ${BRIDGE_LABEL} or the ${PF_ANCHOR} redirect in place; finish or clean it up first"
fi

BACKUP_STAMP="$(date +%Y%m%d-%H%M%S)"
OLD_SUPERVISOR="${SUPERVISOR_BIN}.backup-${BACKUP_STAMP}"
OLD_PLIST="${PLIST}.backup-${BACKUP_STAMP}"
cp -p "$SUPERVISOR_BIN" "$OLD_SUPERVISOR" && cp -p "$PLIST" "$OLD_PLIST" || fail "cannot back up the live supervisor and plist"
say "backed up the live supervisor and plist with suffix .backup-${BACKUP_STAMP}"

# 1. Bridge.
install -m 0755 "$CANDIDATE_SUPERVISOR" "$BRIDGE_SUPERVISOR" && cp -p "$BIN" "$BRIDGE_WORKER" || fail "cannot stage the bridge binaries"
write_bridge_plist || { rm -f "$BRIDGE_SUPERVISOR" "$BRIDGE_WORKER"; fail "cannot write the bridge plist"; }
say "starting bridge ${BRIDGE_LABEL} on :${BRIDGE_PORT}"
"$LAUNCHCTL" bootstrap system "$BRIDGE_PLIST" >/dev/null 2>&1
if ! wait_for "$STEP_TIMEOUT_SECS" "the bridge to accept" status_ok "$BRIDGE_CONTROL" \
  || ! wait_for "$STEP_TIMEOUT_SECS" "the bridge to answer on :${BRIDGE_PORT}" port_health "$BRIDGE_PORT"; then
  remove_bridge
  fail "the bridge never became healthy; nothing else was changed"
fi

# 2. Redirect new connections to the bridge.
if ! load_redirect; then
  drop_redirect; remove_bridge
  fail "could not load the pf redirect; nothing else was changed"
fi
say "new connections now go to the bridge; existing ones stay on the old supervisor"
if ! wait_for 15 "public health through the bridge" port_health "$PUBLIC_PORT"; then
  drop_redirect; remove_bridge
  fail "public health failed through the bridge; the redirect was removed"
fi

# 3-4. Replace the main job behind the redirect.
if ! swap_main_job "$CANDIDATE_SUPERVISOR" "$CANDIDATE_PLIST" "$NEW_CONTROL"; then
  say "the candidate did not come up; restoring the previous supervisor and plist behind the bridge"
  if swap_main_job "$OLD_SUPERVISOR" "$OLD_PLIST" "$CONTROL_SOCKET"; then
    drop_redirect; remove_bridge
    fail "the candidate failed; the previous supervisor is back and the port never closed"
  fi
  fail "the candidate failed and the restore did not come up either; the bridge still serves through the redirect. Do not remove ${PF_ANCHOR} until ${LABEL} answers"
fi

# 5. Send new connections to the new supervisor.
drop_redirect
if ! wait_for 15 "public health on the new supervisor" port_health "$PUBLIC_PORT"; then
  load_redirect
  fail "public health failed after dropping the redirect; the redirect to the bridge was restored"
fi
say "new connections now reach the new supervisor"

# 6. Retire the bridge.
remove_bridge
say "bridge drained and removed"
printf 'HANDOFF OK\n'
