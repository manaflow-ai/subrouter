#!/usr/bin/env bash
# Convert an existing macOS Subrouter LaunchDaemon to the stable supervisor
# layout. Preparation is non-disruptive. --activate performs the one-time
# service transition; all later worker updates are zero-disruption.
set -euo pipefail

LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter-team}"
PLIST="${SUBROUTER_PLIST:-/Library/LaunchDaemons/${LABEL}.plist}"
WORKER_BIN="${SUBROUTER_BIN:-/usr/local/bin/subrouter}"
SUPERVISOR_BIN="${SUBROUTER_SUPERVISOR_BIN:-/usr/local/libexec/subrouter-supervisor}"
CONTROL_SOCKET="${SUBROUTER_CONTROL_SOCKET:-}"
ACTIVATE=0
[ "${1:-}" = "--activate" ] && ACTIVATE=1

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
[ -f "$PLIST" ] || { echo "missing $PLIST" >&2; exit 1; }
[ -x "$WORKER_BIN" ] || { echo "missing $WORKER_BIN" >&2; exit 1; }

mkdir -p "$(dirname "$SUPERVISOR_BIN")"
if [ ! -x "$SUPERVISOR_BIN" ]; then
  install -m 0755 "$WORKER_BIN" "$SUPERVISOR_BIN"
fi
"$SUPERVISOR_BIN" help 2>/dev/null | grep -q ' supervise ' || {
  echo "$SUPERVISOR_BIN does not support supervise" >&2
  exit 1
}

prepared="${PLIST}.supervised"
transform_output="$(PLIST="$PLIST" PREPARED="$prepared" WORKER_BIN="$WORKER_BIN" \
SUPERVISOR_BIN="$SUPERVISOR_BIN" CONTROL_SOCKET="$CONTROL_SOCKET" python3 <<'PY'
import os
import plistlib

source = os.environ["PLIST"]
destination = os.environ["PREPARED"]
worker_bin = os.environ["WORKER_BIN"]
supervisor_bin = os.environ["SUPERVISOR_BIN"]
control_socket = os.environ["CONTROL_SOCKET"]

with open(source, "rb") as stream:
    plist = plistlib.load(stream)

if not control_socket:
    # A non-root service user cannot bind a unix socket inside root-owned
    # /var/run, so default to the service's own state directory.
    if plist.get("UserName"):
        state_dir = (plist.get("EnvironmentVariables") or {}).get(
            "SUBROUTER_STATE_DIR", "/var/lib/subrouter"
        )
        control_socket = os.path.join(state_dir, "supervisor.sock")
    else:
        control_socket = "/var/run/subrouter-supervisor.sock"

arguments = list(plist.get("ProgramArguments") or [])
if len(arguments) < 2 or arguments[1] != "serve":
    raise SystemExit("existing ProgramArguments must start with '<binary> serve'")

worker_args = arguments[2:]
public_addr = "127.0.0.1:31415"
filtered = []
i = 0
while i < len(worker_args):
    argument = worker_args[i]
    if argument == "--addr":
        if i + 1 >= len(worker_args):
            raise SystemExit("existing --addr has no value")
        public_addr = worker_args[i + 1]
        i += 2
        continue
    if argument.startswith("--addr="):
        public_addr = argument.split("=", 1)[1]
        i += 1
        continue
    filtered.append(argument)
    i += 1

plist["Program"] = supervisor_bin
plist["ProgramArguments"] = [
    supervisor_bin,
    "supervise",
    "--addr", public_addr,
    "--control-socket", control_socket,
    "--worker-bin", worker_bin,
    "--",
    *filtered,
]
plist["ProcessType"] = "Interactive"
plist["ThrottleInterval"] = 10
plist["ExitTimeOut"] = 600

temporary = destination + ".new"
with open(temporary, "wb") as stream:
    plistlib.dump(plist, stream, fmt=plistlib.FMT_XML, sort_keys=False)
os.chmod(temporary, 0o644)
os.replace(temporary, destination)
print(public_addr)
print(control_socket)
PY
)"
public_addr="$(printf '%s\n' "$transform_output" | sed -n 1p)"
CONTROL_SOCKET="$(printf '%s\n' "$transform_output" | sed -n 2p)"

plutil -lint "$prepared"
echo "Prepared $prepared (control socket: $CONTROL_SOCKET)"
if [ "$ACTIVATE" -ne 1 ]; then
  echo "Not activated. Re-run with --activate for the one-time listener transition."
  exit 0
fi

health_host="$public_addr"
case "$health_host" in
  0.0.0.0:*) health_host="127.0.0.1:${public_addr#0.0.0.0:}" ;;
  \[::\]:*) health_host="127.0.0.1:${public_addr#\[::\]:}" ;;
esac

wait_for_bootout() {
  # launchctl bootout returns before the job is unloaded; bootstrapping while
  # the old job is still terminating fails with "Input/output error".
  local i=0
  while launchctl print "system/${LABEL}" >/dev/null 2>&1; do
    i=$((i + 1))
    if [ "$i" -ge 120 ]; then
      echo "system/${LABEL} did not unload after bootout" >&2
      return 1
    fi
    sleep 1
  done
}

bootstrap_with_retry() {
  local i=0
  until launchctl bootstrap system "$PLIST"; do
    i=$((i + 1))
    if [ "$i" -ge 10 ]; then
      return 1
    fi
    sleep 2
  done
}

wait_healthy() {
  local i=0
  until curl -fsS "http://${health_host}/_subrouter/health" >/dev/null 2>&1; do
    i=$((i + 1))
    if [ "$i" -ge 60 ]; then
      return 1
    fi
    sleep 1
  done
}

backup="${PLIST}.backup-$(date +%Y%m%d-%H%M%S)"
cp -p "$PLIST" "$backup"

rollback() {
  echo "activation failed; rolling back to $backup" >&2
  launchctl bootout "system/${LABEL}" 2>/dev/null || true
  wait_for_bootout || true
  cp -p "$backup" "$PLIST"
  if bootstrap_with_retry && wait_healthy; then
    echo "rolled back to previous unsupervised service" >&2
  else
    echo "ROLLBACK FAILED; service is down. Restore manually from $backup" >&2
  fi
  exit 1
}

mv -f "$prepared" "$PLIST"
launchctl bootout "system/${LABEL}" 2>/dev/null || true
wait_for_bootout || rollback
bootstrap_with_retry || rollback

i=0
until curl -fsS --unix-socket "$CONTROL_SOCKET" "http://localhost/_subrouter/supervisor-status" >/dev/null 2>&1 \
  && curl -fsS "http://${health_host}/_subrouter/health" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then
    rollback
  fi
  sleep 1
done
echo "Activated supervised Subrouter. Backup: $backup"
