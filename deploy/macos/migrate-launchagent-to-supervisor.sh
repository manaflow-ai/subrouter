#!/usr/bin/env bash
# Put a per-user Subrouter LaunchAgent behind the supervisor, so upgrading the
# binary stops cutting connections that coding agents are actively streaming on.
#
# The existing migrate-launchdaemon-to-supervisor.sh handles the system-wide
# LaunchDaemon (/Library/LaunchDaemons, `system` domain, root). A developer
# machine runs a per-user LaunchAgent instead (~/Library/LaunchAgents,
# `gui/<uid>` domain, no sudo), which that script cannot touch.
#
# Without the supervisor, replacing ~/bin/subrouter and restarting the agent
# closes every established connection: an agent mid-turn loses its response.
# With it, the supervisor owns the listener, health-checks the replacement, and
# lets the old worker finish the connections it already accepted.
#
# The one-time transition below still drops in-flight connections, because the
# unsupervised process owns those file descriptors. Run it when no agent is
# mid-turn. Every upgrade after it is non-disruptive.
set -euo pipefail

LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter}"
PLIST="${SUBROUTER_PLIST:-$HOME/Library/LaunchAgents/${LABEL}.plist}"
WORKER_BIN="${SUBROUTER_BIN:-$HOME/bin/subrouter}"
SUPERVISOR_BIN="${SUBROUTER_SUPERVISOR_BIN:-$HOME/bin/subrouter-supervisor}"
STATE_DIR="${SUBROUTER_STATE_DIR:-$HOME/.subrouter}"
CONTROL_SOCKET="${SUBROUTER_CONTROL_SOCKET:-${STATE_DIR}/supervisor.sock}"
DOMAIN="gui/$(id -u)"

activate=0
[ "${1:-}" = "--activate" ] && activate=1

die() { echo "migrate-launchagent-to-supervisor: $*" >&2; exit 1; }

[ -f "$PLIST" ] || die "$PLIST not found"
[ -x "$WORKER_BIN" ] || die "$WORKER_BIN is not executable"
"$WORKER_BIN" help 2>/dev/null | grep -q ' supervise ' \
  || die "$WORKER_BIN does not support supervise; upgrade it first"

# A separate copy, because routine upgrades replace the worker and must never
# replace the supervisor that is holding the listener.
mkdir -p "$(dirname "$SUPERVISOR_BIN")"
cp -f "$WORKER_BIN" "${SUPERVISOR_BIN}.new"
chmod 0755 "${SUPERVISOR_BIN}.new"
mv -f "${SUPERVISOR_BIN}.new" "$SUPERVISOR_BIN"
# An ad-hoc signature keeps macOS from killing the copy with OS_REASON_CODESIGNING.
codesign -s - -f "$SUPERVISOR_BIN" >/dev/null 2>&1 || true

prepared="${PLIST}.supervised"
python3 - "$PLIST" "$prepared" "$SUPERVISOR_BIN" "$WORKER_BIN" "$CONTROL_SOCKET" <<'PY'
import plistlib
import sys

source, destination, supervisor_bin, worker_bin, control_socket = sys.argv[1:6]
with open(source, "rb") as handle:
    plist = plistlib.load(handle)

arguments = list(plist.get("ProgramArguments") or [])
if not arguments:
    raise SystemExit("existing plist has no ProgramArguments")

# The supervisor owns the public address and supplies `serve` plus the worker's
# private socket itself, so strip both from the inherited arguments.
public_addr = None
filtered = []
index = 1 if len(arguments) > 1 and arguments[1] == "serve" else 1
filtered_source = arguments[index:]
i = 0
while i < len(filtered_source):
    argument = filtered_source[i]
    if argument == "--addr":
        if i + 1 >= len(filtered_source):
            raise SystemExit("existing --addr has no value")
        public_addr = filtered_source[i + 1]
        i += 2
        continue
    if argument.startswith("--addr="):
        public_addr = argument.split("=", 1)[1]
        i += 1
        continue
    filtered.append(argument)
    i += 1

if not public_addr:
    raise SystemExit("could not find --addr in the existing plist")

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
# The supervisor must outlive its draining workers.
plist["ExitTimeOut"] = 600
plist["ThrottleInterval"] = 10

with open(destination, "wb") as handle:
    plistlib.dump(plist, handle, fmt=plistlib.FMT_XML, sort_keys=False)
print(public_addr)
PY

public_addr="$(python3 - "$prepared" <<'PY'
import plistlib, sys
with open(sys.argv[1], "rb") as handle:
    plist = plistlib.load(handle)
arguments = plist["ProgramArguments"]
print(arguments[arguments.index("--addr") + 1])
PY
)"
health_url="http://${public_addr}/_subrouter/health"
case "$public_addr" in
  0.0.0.0:*) health_url="http://127.0.0.1:${public_addr##*:}/_subrouter/health" ;;
esac

echo "prepared $prepared"
if [ "$activate" -eq 0 ]; then
  cat <<EOF

Review the plist above, then activate with:
  $0 --activate

Activation restarts the agent once, which drops connections currently in
flight. Do it when no coding agent is mid-turn. Every upgrade after it
preserves connections.
EOF
  exit 0
fi

backup="${PLIST}.backup-$(date +%Y%m%d-%H%M%S)"
cp -a "$PLIST" "$backup"

rollback() {
  echo "rolling back to the unsupervised agent" >&2
  cp -a "$backup" "$PLIST"
  launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
  launchctl bootstrap "$DOMAIN" "$PLIST" 2>/dev/null || true
}

cp -a "$prepared" "$PLIST"
launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
# bootout returns before the job is gone; bootstrapping too early fails.
for _ in $(seq 1 30); do
  launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1 || break
  sleep 1
done
if ! launchctl bootstrap "$DOMAIN" "$PLIST"; then
  rollback
  die "supervised agent failed to bootstrap"
fi

for _ in $(seq 1 30); do
  if curl -fsS --max-time 2 "$health_url" >/dev/null 2>&1; then
    echo "supervised Subrouter healthy at $health_url"
    echo "control socket: $CONTROL_SOCKET"
    echo "backup: $backup"
    echo
    echo "Upgrades are now non-disruptive:"
    echo "  curl -fsS --unix-socket $CONTROL_SOCKET -X POST http://localhost/_subrouter/upgrade"
    exit 0
  fi
  sleep 1
done

rollback
die "supervised agent never became healthy"
