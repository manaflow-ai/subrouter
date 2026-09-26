#!/usr/bin/env bash
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/../../.." && pwd)"
MIGRATE="$ROOT/deploy/macos/migrate-launchagent-to-supervisor.sh"
ROLLBACK="$ROOT/deploy/macos/rollback-launchagent-supervisor.sh"
TRANSITION_LIB="$ROOT/deploy/macos/launchagent-transition-lib.sh"
MUTATION_LIB="$ROOT/deploy/macos/mutation-lease-lib.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/subrouter-launchagent-test.XXXXXX")"
export TMP
cleanup_launchagent_test() {
  local status=$? pid_file pid command
  if [ -f "$TMP/state" ]; then
    kill "$(cut -d "|" -f 2 "$TMP/state")" 2>/dev/null || true
  fi
  for pid_file in "$TMP/stale-group.pid" "$TMP/pgid-reuse-group.pid" \
    "$TMP/pgid-reuse-token.pid"; do
    [ -s "$pid_file" ] || continue
    kill -KILL "$(cat "$pid_file")" 2>/dev/null || true
  done
  for pid_file in "$TMP/watchdog-kill-callback.pid" "$TMP/leader-exit-child.pid" \
    "$TMP/session-escape-child.pid" "$TMP/mutation-lease-adopt-child.pid"; do
    [ -s "$pid_file" ] || continue
    kill -KILL "$(cat "$pid_file")" 2>/dev/null || true
  done
  for pid_file in "$TMP"/functional-canary/*.pid; do
    [ -f "$pid_file" ] || continue
    pid="$(cat "$pid_file" 2>/dev/null || true)"
    case "$pid" in ''|*[!0-9]*) continue ;; esac
    command="$(/bin/ps -p "$pid" -o command= 2>/dev/null || true)"
    case "$command" in
      *"$TMP/functional-canary"*|*subrouter-functional-canary-test-timeout*)
        kill -KILL "$pid" 2>/dev/null || true
        ;;
    esac
  done
  if [ "${KEEP_SUBROUTER_TEST_TMP:-0}" = 1 ]; then
    echo "kept $TMP" >&2
  else
    rm -rf "$TMP"
  fi
  case "${SUBROUTER_LOCAL_DATA_SOCKET:-}" in
    /private/tmp/srlt-*/data.sock) rm -rf "$(dirname "$SUBROUTER_LOCAL_DATA_SOCKET")" ;;
  esac
  return "$status"
}
trap cleanup_launchagent_test EXIT INT TERM

rollback_help="$($ROLLBACK --help)"
case "$rollback_help" in
  *'--rollback-artifact DEST ARTIFACT SHA MODE'*'--expected-file-sha256 PATH SHA'*'identity-checked legacy LaunchAgent'*) ;;
  *) echo "rollback --help did not describe the required identity inputs" >&2; exit 1 ;;
esac
echo "PASS rollback --help is self-describing and exits zero"

mkdir -p "$TMP/bin" "$TMP/home/Library/LaunchAgents" \
  "$TMP/home/.subrouter/codex/accounts" \
  "$TMP/home/.subrouter-retiring/codex/accounts"

cat >"$TMP/fake-supervisor-listener.py" <<'PY'
#!/usr/bin/env python3
import os
import signal
import socket
import sys
import time

path = sys.argv[1]
os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
if os.path.lexists(path):
    os.unlink(path)
listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
listener.bind(path)
os.chmod(path, 0o600)
listener.listen()
stopping = False
def stop(*_):
    global stopping
    stopping = True
signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
while not stopping:
    time.sleep(0.05)
listener.close()
if os.path.lexists(path):
    os.unlink(path)
PY
chmod 0700 "$TMP/fake-supervisor-listener.py"

cat >"$TMP/bin/launchctl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  print)
    [ -f "$FAKE_LAUNCHD_STATE" ] || exit 113
    IFS='|' read -r program pid <"$FAKE_LAUNCHD_STATE"
    kill -0 "$pid" 2>/dev/null || exit 113
    printf 'program = %s\n' "$program"
    if [ "$program" = "$SUBROUTER_SUPERVISOR_BIN" ] \
      && [ -n "${FAKE_SUPERVISOR_EMPTY_PID_COUNT:-}" ]; then
      attempt=0
      [ ! -s "$FAKE_SUPERVISOR_PRINT_ATTEMPT_FILE" ] \
        || attempt="$(tr -d '[:space:]' <"$FAKE_SUPERVISOR_PRINT_ATTEMPT_FILE")"
      if [ "$attempt" -lt "$FAKE_SUPERVISOR_EMPTY_PID_COUNT" ]; then
        printf '%s\n' "$((attempt + 1))" >"$FAKE_SUPERVISOR_PRINT_ATTEMPT_FILE"
        exit 0
      fi
    fi
    printf 'pid = %s\n' "$pid"
    ;;
  bootout)
    [ -f "$FAKE_LAUNCHD_STATE" ] || exit 113
    IFS='|' read -r program pid <"$FAKE_LAUNCHD_STATE"
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    if [ "${FAKE_BOOTOUT_REPLACE_PID:-0}" = 1 ]; then
      sleep 300 9>&- &
      printf '%s|%s\n' "$program" "$!" >"$FAKE_LAUNCHD_STATE"
    else
      rm -f "$FAKE_LAUNCHD_STATE"
    fi
    [ "$program" != "$SUBROUTER_SUPERVISOR_BIN" ] || rm -f "$SUBROUTER_CONTROL_SOCKET"
    ;;
  bootstrap)
    if [ "${FAKE_BOOTSTRAP_FAIL_ONCE:-0}" = 1 ] && [ ! -f "${FAKE_LAUNCHD_STATE}.bootstrap-failed" ]; then
      : >"${FAKE_LAUNCHD_STATE}.bootstrap-failed"
      exit 5
    fi
    program="$(/usr/libexec/PlistBuddy -c 'Print :Program' "$3")"
    if [ "$program" != "$SUBROUTER_SUPERVISOR_BIN" ]; then
      if [ "${FAKE_REQUIRE_BINDING_ABSENT_BEFORE_LEGACY_BOOTSTRAP:-0}" = 1 ] \
        && { [ -e "$HOME/.subrouter/codex/.local-serving-store.json" ] \
          || [ -L "$HOME/.subrouter/codex/.local-serving-store.json" ]; }; then
        exit 88
      fi
      if [ -n "${FAKE_EXPECTED_BINDING_BEFORE_LEGACY_BOOTSTRAP:-}" ] \
        && ! cmp -s "$FAKE_EXPECTED_BINDING_BEFORE_LEGACY_BOOTSTRAP" \
          "$HOME/.subrouter/codex/.local-serving-store.json"; then
        exit 89
      fi
    fi
    if [ -n "${FAKE_ROLLBACK_TRAFFIC_FILE:-}" ] \
      && [ -n "${FAKE_ROLLBACK_OVERLAP_SENTINEL:-}" ] \
      && [ "$program" != "$SUBROUTER_SUPERVISOR_BIN" ]; then
      before_size="$(wc -c <"$FAKE_ROLLBACK_TRAFFIC_FILE" 2>/dev/null || echo 0)"
      sleep 0.15
      after_size="$(wc -c <"$FAKE_ROLLBACK_TRAFFIC_FILE" 2>/dev/null || echo 0)"
      if [ "$before_size" != "$after_size" ]; then
        printf 'callback traffic overlapped rollback\n' >"$FAKE_ROLLBACK_OVERLAP_SENTINEL"
      fi
    fi
    if [ "$program" = "$SUBROUTER_SUPERVISOR_BIN" ]; then
      "$TMP/fake-supervisor-listener.py" "$SUBROUTER_LOCAL_DATA_SOCKET" 9>&- &
    else
      sleep 300 9>&- &
    fi
    printf '%s|%s\n' "$program" "$!" >"$FAKE_LAUNCHD_STATE"
    [ "$program" != "$SUBROUTER_SUPERVISOR_BIN" ] || : >"$SUBROUTER_CONTROL_SOCKET"
    ;;
  *) exit 2 ;;
esac
SH
cat >"$TMP/bin/curl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case " $* " in
  *" --unix-socket "*)
    if [ -n "${FAKE_CONTROL_SOCKET_FAIL_COUNT:-}" ]; then
      attempt=0
      [ ! -s "$FAKE_CONTROL_SOCKET_ATTEMPT_FILE" ] \
        || attempt="$(tr -d '[:space:]' <"$FAKE_CONTROL_SOCKET_ATTEMPT_FILE")"
      if [ "$attempt" -lt "$FAKE_CONTROL_SOCKET_FAIL_COUNT" ]; then
        printf '%s\n' "$((attempt + 1))" >"$FAKE_CONTROL_SOCKET_ATTEMPT_FILE"
        exit 7
      fi
    fi
    worker_pid="${FAKE_ACTIVE_WORKER_PID:-4242}"
    if [ -n "${FAKE_ACTIVE_WORKER_PID_FILE:-}" ] && [ -s "$FAKE_ACTIVE_WORKER_PID_FILE" ]; then
      worker_pid="$(tr -d '[:space:]' <"$FAKE_ACTIVE_WORKER_PID_FILE")"
    fi
    printf '{"accepting":true,"retiring":false,"active":{"id":"candidate"},"active_worker":{"id":"candidate","pid":%s,"process_start_identity":"darwin:100:%s","identity_kind":"darwin-cdhash-sha256","executable_identity":"%s"},"backends":[{}]}\n' "$worker_pid" "$worker_pid" "${FAKE_ACTIVE_WORKER_CDHASH:-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}"
    ;;
  *)
    if [ -n "${FAKE_CONCURRENT_BINDING_PATH:-}" ] \
      && [ -n "${FAKE_CONCURRENT_BINDING_SENTINEL:-}" ] \
      && [ ! -e "$FAKE_CONCURRENT_BINDING_SENTINEL" ]; then
      printf '%s\n' 'operator-selected-third-binding' >"$FAKE_CONCURRENT_BINDING_PATH"
      chmod 0600 "$FAKE_CONCURRENT_BINDING_PATH"
      : >"$FAKE_CONCURRENT_BINDING_SENTINEL"
    fi
    if [ -n "${FAKE_RESTART_DURING_HTTP_AFTER_CALLS:-}" ]; then
      http_attempt=0
      [ ! -s "$FAKE_RESTART_DURING_HTTP_ATTEMPT_FILE" ] \
        || http_attempt="$(tr -d '[:space:]' <"$FAKE_RESTART_DURING_HTTP_ATTEMPT_FILE")"
      http_attempt=$((http_attempt + 1))
      printf '%s\n' "$http_attempt" >"$FAKE_RESTART_DURING_HTTP_ATTEMPT_FILE"
    fi
    if [ -n "${FAKE_RESTART_DURING_HTTP_AFTER_CALLS:-}" ] \
      && [ "$http_attempt" -gt "$FAKE_RESTART_DURING_HTTP_AFTER_CALLS" ] \
      && [ ! -e "$FAKE_RESTART_DURING_HTTP_SENTINEL" ]; then
      : >"$FAKE_RESTART_DURING_HTTP_SENTINEL"
      IFS='|' read -r program old_pid <"$FAKE_LAUNCHD_STATE"
      sleep 300 9>&- &
      new_pid=$!
      printf '%s|%s\n' "$program" "$new_pid" >"$FAKE_LAUNCHD_STATE"
      kill "$old_pid" 2>/dev/null || true
    fi
    ;;
esac
exit 0
SH
cat >"$TMP/bin/codesign" <<'SH'
#!/bin/sh
case " $* " in
  *" -d "*) printf '%s\n' 'CDHash=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' >&2 ;;
esac
exit 0
SH
cat >"$TMP/bin/lsof" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ -f "$FAKE_LAUNCHD_STATE" ] || exit 1
IFS='|' read -r program pid <"$FAKE_LAUNCHD_STATE"
kill -0 "$pid" 2>/dev/null || exit 1
case " $* " in
  *" -t "*) printf '%s\n' "$pid" ;;
  *) printf 'fake-listener %s\n' "$pid" ;;
esac
SH
cat >"$TMP/bin/ps" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
pid="" field=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -p) pid="$2"; shift 2 ;;
    -o) field="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -f "$FAKE_LAUNCHD_STATE" ] || exit 1
IFS='|' read -r program state_pid <"$FAKE_LAUNCHD_STATE"
[ "$pid" = "$state_pid" ] || exit 1
case "$field" in
  command=)
    if [ "$program" = "$SUBROUTER_SUPERVISOR_BIN" ]; then
      printf '%s supervise --control-socket %s --worker-bin %s -- --addr 127.0.0.1:43199\n' \
        "$program" "$SUBROUTER_CONTROL_SOCKET" "$SUBROUTER_BIN"
    else
      printf '%s serve --addr 127.0.0.1:43199\n' "$program"
    fi
    ;;
  lstart=) printf '%s\n' 'Sun Aug 30 12:00:00 2026' ;;
  ppid=) printf '%s\n' 1 ;;
  *) exit 2 ;;
esac
SH
cat >"$TMP/bin/stat" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}|${2:-}" in
  '-f|%HT') printf '%s\n' Socket ;;
  '-f|%Lp')
    if [ "${3:-}" = "$SUBROUTER_CONTROL_SOCKET" ]; then printf '%s\n' 600; else /usr/bin/stat "$@"; fi
    ;;
  '-f|%u') printf '%s\n' "$(id -u)" ;;
  *) exec /usr/bin/stat "$@" ;;
esac
SH
cat >"$TMP/bin/plutil" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [ -n "${FAKE_MUTATE_BINDING_BEFORE_LEGACY_BOOTSTRAP:-}" ] \
  && { [ -z "${FAKE_MUTATE_BINDING_SENTINEL:-}" ] \
    || [ ! -e "$FAKE_MUTATE_BINDING_SENTINEL" ]; }; then
  temporary="${FAKE_MUTATE_BINDING_BEFORE_LEGACY_BOOTSTRAP}.fake-next"
  printf '%s\n' 'operator-selected-third-binding' \
    >"$temporary"
  chmod 0600 "$temporary"
  mv -f "$temporary" "$FAKE_MUTATE_BINDING_BEFORE_LEGACY_BOOTSTRAP"
  [ -z "${FAKE_MUTATE_BINDING_SENTINEL:-}" ] \
    || : >"$FAKE_MUTATE_BINDING_SENTINEL"
fi
exec /usr/bin/plutil "$@"
SH
chmod +x "$TMP/bin/launchctl" "$TMP/bin/curl" "$TMP/bin/codesign" "$TMP/bin/lsof" "$TMP/bin/ps" "$TMP/bin/stat" "$TMP/bin/plutil"

legacy="$TMP/home/bin/subrouter-legacy"
legacy_dependency="$TMP/home/bin/subrouter-legacy-worker"
worker="$TMP/home/bin/subrouter"
supervisor="$TMP/home/bin/subrouter-supervisor"
mkdir -p "$(dirname "$legacy")"
cat >"$legacy_dependency" <<'SH'
#!/bin/sh
exit 0
SH
cat >"$legacy" <<EOF
#!/bin/sh
exec "$legacy_dependency" \\
  "\$@"
EOF
cat >"$worker" <<'SH'
#!/bin/sh
if [ -n "${FAKE_WORKER_LOG:-}" ]; then
  printf '%s|%s|%s\n' "${SUBROUTER_STATE_DIR:-}" "$*" "${SUBROUTER_RETIRING_STATE_DIR:-}" >>"$FAKE_WORKER_LOG"
fi
case "${1:-}" in
  help) echo ' subrouter supervise --worker-bin PATH ' ;;
  doctor) echo '{"status":"ok"}' ;;
  codex)
    if [ "${FAKE_ISOLATION_FAIL:-0}" = 1 ]; then
      echo '{"comparison":{"ok":false}}'
      exit 77
    fi
    echo '{"comparison":{"ok":true}}'
    ;;
  daemon)
    [ "${2:-}" = bind-state ] || exit 64
    [ -z "${SUBROUTER_STATE_DIR+x}" ] || exit 65
    [ "${4:-}" = --local-data-socket ] || exit 66
    python3 - "$HOME/.subrouter/codex/.local-serving-store.json" "${3:-}" "${5:-}" <<'PY'
import json
import os
import sys

path, state_dir, local_data_socket = sys.argv[1:]
socket_stat = os.stat(local_data_socket, follow_symlinks=False)
os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
temporary = path + ".next"
with open(temporary, "w", encoding="utf-8") as output:
    json.dump(
        {
            "schema": "subrouter.local-serving-store/v2",
            "accounts_dir": os.path.realpath(os.path.join(state_dir, "codex", "accounts")),
            "local_data_socket": local_data_socket,
            "local_data_socket_identity": f"unix:{socket_stat.st_dev}:{socket_stat.st_ino}",
        },
        output,
        separators=(",", ":"),
    )
    output.write("\n")
    output.flush()
    os.fsync(output.fileno())
os.chmod(temporary, 0o600)
os.replace(temporary, path)
PY
    ;;
esac
exit 0
SH
chmod +x "$legacy" "$legacy_dependency" "$worker"
cat >"$TMP/preflight" <<'SH'
#!/bin/sh
[ -z "${FAKE_PREFLIGHT_LOG:-}" ] || printf 'invoked\n' >>"$FAKE_PREFLIGHT_LOG"
exit 0
SH
cat >"$TMP/canary-fail" <<'SH'
#!/bin/sh
exit 1
SH
cat >"$TMP/canary-ok" <<'SH'
#!/bin/sh
[ -z "${SUBROUTER_STATE_DIR+x}" ] || exit 81
python3 - "$HOME/.subrouter/codex/.local-serving-store.json" "$FAKE_EXPECTED_CANDIDATE_STATE" <<'PY'
import json
import os
import sys

path, state_dir = sys.argv[1:]
with open(path, encoding="utf-8") as source:
    binding = json.load(source)
expected = os.path.realpath(os.path.join(state_dir, "codex", "accounts"))
socket = os.environ["SUBROUTER_LOCAL_DATA_SOCKET"]
socket_stat = os.stat(socket, follow_symlinks=False)
if binding != {
    "schema": "subrouter.local-serving-store/v2",
    "accounts_dir": expected,
    "local_data_socket": socket,
    "local_data_socket_identity": f"unix:{socket_stat.st_dev}:{socket_stat.st_ino}",
}:
    raise SystemExit(82)
PY
SH
cat >"$TMP/canary-switch-worker" <<'SH'
#!/bin/sh
printf '%s\n' 4343 >"$FAKE_ACTIVE_WORKER_PID_FILE"
SH
cat >"$TMP/canary-wait" <<'SH'
#!/bin/sh
exec sleep 300
SH
cat >"$TMP/canary-traffic" <<'SH'
#!/bin/sh
trap '' TERM
printf '%s\n' "$$" >"$FAKE_CANARY_TRAFFIC_PID_FILE"
while :; do
  printf 'traffic\n' >>"$FAKE_CANARY_TRAFFIC_FILE"
  sleep 0.02
done
SH
cat >"$TMP/canary-leader-exits" <<'SH'
#!/bin/sh
(
  trap '' TERM
  while :; do
    printf 'traffic\n' >>"$FAKE_CANARY_TRAFFIC_FILE"
    sleep 0.02
  done
) &
printf '%s\n' "$!" >"$FAKE_CANARY_TRAFFIC_PID_FILE"
exit 0
SH
cat >"$TMP/canary-session-escape.py" <<'PY'
#!/usr/bin/env python3
import os
import signal
import subprocess
import sys
import time

child_code = """
import os
import signal
import time
signal.signal(signal.SIGTERM, signal.SIG_IGN)
with open(os.environ['FAKE_CANARY_TRAFFIC_FILE'], 'a', encoding='ascii') as output:
    while True:
        output.write('traffic\\n')
        output.flush()
        os.fsync(output.fileno())
        time.sleep(0.02)
"""
child = subprocess.Popen([sys.executable, "-c", child_code], start_new_session=True)
with open(os.environ["FAKE_CANARY_TRAFFIC_PID_FILE"], "w", encoding="ascii") as output:
    output.write(f"{child.pid}\n")
    output.flush()
    os.fsync(output.fileno())
PY
chmod +x "$TMP/preflight" "$TMP/canary-fail" "$TMP/canary-ok" "$TMP/canary-switch-worker" \
  "$TMP/canary-wait" "$TMP/canary-traffic" "$TMP/canary-leader-exits" \
  "$TMP/canary-session-escape.py"

label="test.subrouter.launchagent"
plist="$TMP/home/Library/LaunchAgents/$label.plist"
write_plist() {
  local program="$1" mode="$2" dependency="${3:-}" dependency_entry=""
  [ -z "$dependency" ] || dependency_entry="<string>$dependency</string>"
  cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>$label</string>
<key>Program</key><string>$program</string>
<key>ProgramArguments</key><array><string>$program</string><string>$mode</string><string>--addr</string><string>127.0.0.1:43199</string>$dependency_entry</array>
<key>EnvironmentVariables</key><dict><key>SUBROUTER_STATE_DIR</key><string>$TMP/home/.subrouter-retiring</string></dict>
</dict></plist>
EOF
}

write_wrapper_plist() {
  cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>$label</string>
<key>Program</key><string>$legacy</string>
<key>ProgramArguments</key><array><string>$legacy</string></array>
<key>EnvironmentVariables</key><dict>
  <key>SUBROUTER_STATE_DIR</key><string>$TMP/home/.subrouter-retiring</string>
  <key>LEGACY_ONLY</key><string>preserved</string>
</dict>
</dict></plist>
EOF
}

stop_fake_job() {
  if [ -f "$FAKE_LAUNCHD_STATE" ]; then
    kill "$(cut -d '|' -f 2 "$FAKE_LAUNCHD_STATE")" 2>/dev/null || true
    rm -f "$FAKE_LAUNCHD_STATE"
  fi
}

find_functional_canary_watchdog() {
  local parent_pid="$1" child_pid command
  while IFS= read -r child_pid; do
    case "$child_pid" in ''|*[!0-9]*) continue ;; esac
    command="$(/bin/ps -p "$child_pid" -o command= 2>/dev/null || true)"
    case "$command" in
      *functional-canary-process-group/process-group*) printf '%s\n' "$child_pid"; return 0 ;;
    esac
  done < <(/usr/bin/pgrep -P "$parent_pid" 2>/dev/null || true)
  return 1
}

reset_legacy() {
  stop_fake_job
  rm -rf "${plist}.supervisor-transaction"
  rm -f "${FAKE_LAUNCHD_STATE}.bootstrap-failed"
  rm -f "$TMP/home/.subrouter/codex/.local-serving-store.json"
  write_plist "$legacy" serve
  launchctl bootstrap "gui/$(id -u)" "$plist"
  "$MIGRATE" >/dev/null
}

functional_canary_root="$TMP/functional-canary"
functional_canary_runner="$ROOT/deploy/macos/run-functional-canary.py"
functional_canary_leg="$functional_canary_root/fixture-leg.py"
functional_canary_manifest="$functional_canary_root/manifest.json"
functional_canary_evidence="$functional_canary_root/evidence.json"
functional_canary_order="$functional_canary_root/order"
functional_canary_leader_pid="$functional_canary_root/leader.pid"
functional_canary_descendant_pid="$functional_canary_root/descendant.pid"
functional_canary_ready="$functional_canary_root/descendant.ready"
functional_canary_secret='synthetic-functional-canary-secret-that-must-not-escape'
mkdir -m 0700 "$functional_canary_root"
cat >"$functional_canary_leg" <<'PY'
#!/usr/bin/env python3
import json
import os
import signal
import subprocess
import sys
import time

LEG_SCHEMA = "subrouter.launchagent-functional-canary-leg/v1"


def write_private(path, value):
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.write(descriptor, value.encode())
    finally:
        os.close(descriptor)


name = os.environ["SUBROUTER_CANARY_LEG_NAME"]
with open(os.environ["SUBROUTER_CANARY_LEG_CONFIG_FILE"], encoding="utf-8") as source:
    config = json.load(source)
if config["expected_leg"] != name:
    raise SystemExit(91)
with open(config["order_file"], "a", encoding="utf-8") as order:
    order.write(name + "\n")

mode = config["mode"]
if mode == "semantic-failure":
    print(config["secret"], file=sys.stderr)
    print(json.dumps({"schema": LEG_SCHEMA, "leg": name, "ok": False}, separators=(",", ":")))
    raise SystemExit(0)
if mode == "timeout":
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    child_code = (
        "import signal,sys,time; "
        "signal.signal(signal.SIGTERM, signal.SIG_IGN); "
        "open(sys.argv[1], 'w').write('ready\\n'); "
        "time.sleep(30)"
    )
    child = subprocess.Popen(
        [sys.executable, "-c", child_code, config["ready_file"],
         "subrouter-functional-canary-test-timeout"],
        start_new_session=True,
    )
    deadline = time.monotonic() + 2
    while not os.path.exists(config["ready_file"]) and time.monotonic() < deadline:
        time.sleep(0.01)
    if not os.path.exists(config["ready_file"]):
        child.kill()
        child.wait()
        raise SystemExit(92)
    write_private(config["leader_pid_file"], str(os.getpid()) + "\n")
    write_private(config["descendant_pid_file"], str(child.pid) + "\n")
    time.sleep(30)

print(json.dumps({"schema": LEG_SCHEMA, "leg": name, "ok": True}, separators=(",", ":")))
PY
chmod 0700 "$functional_canary_leg" "$worker"

prepare_functional_canary() {
  local scenario="$1"
  rm -f "$functional_canary_root"/config-*.json \
    "$functional_canary_manifest" "$functional_canary_evidence" \
    "$functional_canary_order" "$functional_canary_leader_pid" \
    "$functional_canary_descendant_pid" "$functional_canary_ready"
  python3 - "$scenario" "$functional_canary_root" "$functional_canary_leg" \
    "$worker" "$functional_canary_secret" <<'PY'
import hashlib
import json
import os
import sys

scenario, root, executable, worker, secret = sys.argv[1:]
legs = (
    "peer-health-readiness",
    "authenticated-routed-codex",
    "sticky-reuse",
    "safe-failover-reuse",
    "authenticated-routed-claude",
    "existing-session-next-turn",
)


def digest(path):
    with open(path, "rb") as source:
        return hashlib.sha256(source.read()).hexdigest()


def write_private(path, document):
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.write(descriptor, (json.dumps(document, separators=(",", ":")) + "\n").encode())
    finally:
        os.close(descriptor)


order_file = os.path.join(root, "order")
descriptor = os.open(order_file, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
os.close(descriptor)
# Process creation on a busy developer Mac can occasionally take more than five
# seconds even for this no-op fixture. Keep the semantic timeout test explicit
# while giving success legs enough scheduling headroom to avoid a false red.
timeout = 30 if scenario == "timeout" else 15
manifest_legs = []
for index, name in enumerate(legs):
    mode = "success"
    if scenario == "semantic-failure" and name == "sticky-reuse":
        mode = "semantic-failure"
    elif scenario == "timeout" and index == 0:
        mode = "timeout"
    config = {
        "expected_leg": name,
        "mode": mode,
        "order_file": order_file,
    }
    if mode == "semantic-failure":
        config["secret"] = secret
    if mode == "timeout":
        config.update({
            "leader_pid_file": os.path.join(root, "leader.pid"),
            "descendant_pid_file": os.path.join(root, "descendant.pid"),
            "ready_file": os.path.join(root, "descendant.ready"),
        })
    config_path = os.path.join(root, f"config-{index}.json")
    write_private(config_path, config)
    manifest_legs.append({
        "name": name,
        "executable": executable,
        "executable_sha256": digest(executable),
        "config_file": config_path,
        "config_sha256": digest(config_path),
        "timeout_seconds": timeout,
    })

manifest = {
    "schema": "subrouter.launchagent-functional-canary/v1",
    "source_git_oid_unverified": "a" * 40,
    "candidate_worker": {"path": worker, "sha256": digest(worker)},
    "evidence_file": os.path.join(root, "evidence.json"),
    "total_timeout_seconds": timeout * len(legs),
    "legs": manifest_legs,
}
write_private(os.path.join(root, "manifest.json"), manifest)
PY
}

assert_functional_canary_order() {
  python3 - "$functional_canary_order" "$@" <<'PY'
import pathlib
import sys

observed = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").splitlines()
expected = sys.argv[2:]
if observed != expected:
    raise SystemExit(f"functional canary order {observed!r}, expected {expected!r}")
PY
}

assert_functional_canary_evidence() {
  local expected_status="$1"
  shift
  python3 - "$functional_canary_evidence" "$functional_canary_manifest" \
    "$worker" "$functional_canary_leg" "$functional_canary_root" \
    "$expected_status" "$@" <<'PY'
import hashlib
import json
import os
import sys

evidence_path, manifest_path, worker, executable, root, expected_status, *expected_legs = sys.argv[1:]
with open(evidence_path, encoding="utf-8") as source:
    evidence = json.load(source)


def digest(path):
    with open(path, "rb") as source:
        return hashlib.sha256(source.read()).hexdigest()


assert evidence["schema"] == "subrouter.launchagent-functional-canary/v1"
assert evidence["status"] == expected_status
assert evidence["manifest_sha256"] == digest(manifest_path)
assert evidence["candidate_worker_sha256"] == digest(worker)
assert evidence["source_git_oid_unverified"] == "a" * 40
assert len(evidence["run_id"]) == 64
assert [leg["name"] for leg in evidence["legs"]] == expected_legs
for name, leg in zip(expected_legs, evidence["legs"]):
    index = (
        "peer-health-readiness",
        "authenticated-routed-codex",
        "sticky-reuse",
        "safe-failover-reuse",
        "authenticated-routed-claude",
        "existing-session-next-turn",
    ).index(name)
    assert leg["ok"] is True
    assert leg["executable_sha256"] == digest(executable)
    assert leg["config_sha256"] == digest(os.path.join(root, f"config-{index}.json"))
PY
}

rollback_bundle_count() {
  find "$(dirname "$plist")" -maxdepth 1 -type d \
    -name "$(basename "$plist").rollback-bundle-*" | wc -l | tr -d ' '
}

rollback_identity_count() {
  find "$(dirname "$plist")" -maxdepth 2 -type f -name 'legacy.plist.identity' \
    | wc -l | tr -d ' '
}

assert_exact_legacy_rollback() {
  local snapshot="$1"
  cmp -s "$snapshot" "$plist"
  [ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
  [ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$plist")" = "$legacy" ]
  [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
  [ ! -e "${plist}.supervisor-transaction" ]
}

wait_for_pid_gone() {
  local pid="$1" attempts=0
  while kill -0 "$pid" 2>/dev/null; do
    # A drained callback can remain as a zombie until launchd reaps it. A
    # zombie cannot execute or write traffic, so do not turn its reaping delay
    # into a false transaction failure on a busy macOS runner.
    local state
    state="$(/bin/ps -p "$pid" -o state= 2>/dev/null || true)"
    case "$state" in
      Z*) return 0 ;;
    esac
    attempts=$((attempts + 1))
    [ "$attempts" -lt 100 ] || return 1
    sleep 0.05
  done
}

wait_for_crash_released_mutation_lease() {
  local lock_file="$1" attempts=0
  while ! /usr/bin/lockf -s -k -t 0 "$lock_file" /usr/bin/true 2>/dev/null; do
    attempts=$((attempts + 1))
    [ "$attempts" -lt 1000 ] || {
      echo "crashed migration did not release mutation lease $lock_file" >&2
      return 1
    }
    sleep 0.01
  done
}

export PATH="$TMP/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export HOME="$TMP/home"
export FAKE_LAUNCHD_STATE="$TMP/state"
export SUBROUTER_LABEL="$label"
export SUBROUTER_PLIST="$plist"
export SUBROUTER_BIN="$worker"
export SUBROUTER_SUPERVISOR_BIN="$supervisor"
export SUBROUTER_STATE_DIR="$TMP/home/.subrouter"
export SUBROUTER_LOCAL_DATA_SOCKET="/private/tmp/srlt-$$/data.sock"
mkdir -m 0700 "$(dirname "$SUBROUTER_LOCAL_DATA_SOCKET")"
export SUBROUTER_CONTROL_SOCKET="$TMP/home/.subrouter/supervisor.sock"
export FAKE_EXPECTED_CANDIDATE_STATE="$SUBROUTER_STATE_DIR"
export SUBROUTER_ABSENCE_ATTEMPTS=10 SUBROUTER_ABSENCE_INTERVAL=0.01
export SUBROUTER_BOOTSTRAP_ATTEMPTS=2 SUBROUTER_BOOTSTRAP_INTERVAL=0.01
export SUBROUTER_HEALTH_ATTEMPTS=2 SUBROUTER_HEALTH_INTERVAL=0.01

cat >"$TMP/mutation-lease-adopt-child" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. "$SUBROUTER_MUTATION_LEASE_LIB"
subrouter_mutation_lease_is_held_by_parent "$SUBROUTER_MUTATION_LOCK_FILE"
printf '%s\n' "$$" >"$SUBROUTER_MUTATION_ADOPT_READY_FILE"
while [ ! -e "$SUBROUTER_MUTATION_ADOPT_RELEASE_FILE" ]; do sleep 0.01; done
SH
chmod 0700 "$TMP/mutation-lease-adopt-child"

write_plist "$legacy" serve
launchctl bootstrap "gui/$(id -u)" "$plist"
"$MIGRATE" >"$TMP/prepare.out" 2>"$TMP/prepare.err"
[ "$(/usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables:SUBROUTER_STATE_DIR' "${plist}.supervised")" = "$SUBROUTER_STATE_DIR" ]
mutation_lock="${plist}.supervisor-mutation.lock"
adopt_lock="$TMP/adopted-mutation.lock"
adopt_ready="$TMP/adopted-mutation.ready"
adopt_release="$TMP/adopted-mutation.release"
SUBROUTER_MUTATION_LEASE_LIB="$MUTATION_LIB" \
SUBROUTER_MUTATION_LOCK_FILE="$adopt_lock" \
SUBROUTER_MUTATION_ADOPT_READY_FILE="$adopt_ready" \
SUBROUTER_MUTATION_ADOPT_RELEASE_FILE="$adopt_release" \
bash -c '
  set -euo pipefail
  . "$SUBROUTER_MUTATION_LEASE_LIB"
  acquire_subrouter_mutation_lease "$SUBROUTER_MUTATION_LOCK_FILE"
  export SUBROUTER_MUTATION_LEASE_OWNER_PID="$$"
  export SUBROUTER_MUTATION_LEASE_HELPER_PID="$SUBROUTER_MUTATION_LEASE_PID"
  export SUBROUTER_MUTATION_LEASE_CONTROL_DIR
  "$1"
  : # Keep this owner shell alive; do not let bash tail-exec the adopted child.
' bash "$TMP/mutation-lease-adopt-child" &
adopt_owner_pid=$!
for _ in $(seq 1 1000); do [ -s "$adopt_ready" ] && break; sleep 0.01; done
[ -s "$adopt_ready" ] || { echo "mutation lease adoption did not become ready" >&2; exit 1; }
adopt_child_pid="$(cat "$adopt_ready")"
if /usr/bin/lockf -s -k -t 0 "$adopt_lock" /usr/bin/true; then
  echo "mutation lease was not held before owner death" >&2
  exit 1
fi
kill -KILL "$adopt_owner_pid"
wait "$adopt_owner_pid" 2>/dev/null || true
kill -0 "$adopt_child_pid"
sleep 0.1
if /usr/bin/lockf -s -k -t 0 "$adopt_lock" /usr/bin/true; then
  echo "adopted mutation lease was released while rollback child remained live" >&2
  exit 1
fi
: >"$adopt_release"
wait_for_pid_gone "$adopt_child_pid"
adopt_release_observed=0
for _ in $(seq 1 200); do
  if /usr/bin/lockf -s -k -t 0 "$adopt_lock" /usr/bin/true; then
    adopt_release_observed=1
    break
  fi
  sleep 0.01
done
[ "$adopt_release_observed" -eq 1 ] || { echo "adopted mutation lease did not release after rollback child exit" >&2; exit 1; }
echo "PASS rollback child retained the mutation lease across migration owner death"

/usr/bin/lockf -k "$mutation_lock" /bin/sleep 30 &
mutation_lock_holder=$!
for _ in $(seq 1 100); do
  if ! /usr/bin/lockf -s -k -t 0 "$mutation_lock" /usr/bin/true; then
    break
  fi
  sleep 0.01
done
if "$MIGRATE" >"$TMP/mutation-lock.out" 2>"$TMP/mutation-lock.err"; then
  echo "migration unexpectedly ran while updater mutation lease was held" >&2
  kill "$mutation_lock_holder" 2>/dev/null || true
  exit 1
fi
kill "$mutation_lock_holder" 2>/dev/null || true
wait "$mutation_lock_holder" 2>/dev/null || true
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
echo "PASS updater mutation lease excluded migration before any file change"

if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-fail" \
  SUBROUTER_CANARY_TIMEOUT=0 "$MIGRATE" --activate \
  >"$TMP/invalid-timeout.out" 2>"$TMP/invalid-timeout.err"; then
  echo "invalid canary timeout unexpectedly accepted" >&2
  exit 1
fi
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
grep -q 'functional canary timeout must be a positive integer' "$TMP/invalid-timeout.err"
echo "PASS invalid callback timeout was rejected before activation"

if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-fail" \
  "$MIGRATE" --activate >"$TMP/migrate.out" 2>"$TMP/migrate.err"; then
  echo "canary failure unexpectedly accepted" >&2
  exit 1
fi
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
grep -q 'functional canary failed; legacy LaunchAgent restored' "$TMP/migrate.err"
identity_manifest="$(find "$(dirname "$plist")" -maxdepth 2 -name 'legacy.plist.identity' -print -quit)"
[ -n "$identity_manifest" ]
grep -Fq "  $legacy_dependency  " "$identity_manifest"
[ ! -e "$TMP/home/.subrouter/codex/.local-serving-store.json" ]
echo "PASS canary failure automatically restored the exact legacy plist and absent serving-store binding"

for rollback_phase in rollback_legacy_bootstrap_requested rollback_legacy_accepted rollback_binding_restored; do
  reset_legacy
  rollback_snapshot="$TMP/rollback-phase-$rollback_phase.plist"
  cp -p "$plist" "$rollback_snapshot"
  if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-fail" \
    SUBROUTER_ROLLBACK_FAULT_INJECT_HARD_PHASE="$rollback_phase" \
    "$MIGRATE" --activate >"$TMP/rollback-phase-$rollback_phase.out" \
    2>"$TMP/rollback-phase-$rollback_phase.err"; then
    echo "rollback hard fault at $rollback_phase unexpectedly succeeded" >&2
    exit 1
  fi
  assert_exact_legacy_rollback "$rollback_snapshot"
done
echo "PASS rollback hard faults after legacy bootstrap and before pointer restore recovered exact healthy legacy"

reset_legacy
prior_serving_store_binding="$TMP/prior-serving-store-binding.json"
python3 - "$TMP/home/.subrouter/codex/.local-serving-store.json" \
  "$TMP/home/.subrouter-retiring" <<'PY'
import json
import os
import sys

path, state_dir = sys.argv[1:]
with open(path, "w", encoding="utf-8") as output:
    json.dump(
        {
            "schema": "subrouter.local-serving-store/v1",
            "accounts_dir": os.path.realpath(os.path.join(state_dir, "codex", "accounts")),
        },
        output,
        separators=(",", ":"),
    )
    output.write("\n")
os.chmod(path, 0o400)
PY
cp -p "$TMP/home/.subrouter/codex/.local-serving-store.json" "$prior_serving_store_binding"
if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-fail" \
  "$MIGRATE" --activate >"$TMP/prior-binding-rollback.out" \
  2>"$TMP/prior-binding-rollback.err"; then
  echo "canary failure with a prior serving-store binding unexpectedly accepted" >&2
  exit 1
fi
cmp -s "$prior_serving_store_binding" "$TMP/home/.subrouter/codex/.local-serving-store.json"
[ "$(stat -f '%Lp' "$TMP/home/.subrouter/codex/.local-serving-store.json")" = 400 ]
echo "PASS canary failure restored the exact prior serving-store binding bytes and mode"

for rollback_phase in rollback_legacy_bootstrap_requested rollback_legacy_accepted rollback_binding_restored; do
  reset_legacy
  cp -p "$prior_serving_store_binding" "$TMP/home/.subrouter/codex/.local-serving-store.json"
  rollback_snapshot="$TMP/prior-binding-phase-$rollback_phase.plist"
  cp -p "$plist" "$rollback_snapshot"
  if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-fail" \
    SUBROUTER_ROLLBACK_FAULT_INJECT_HARD_PHASE="$rollback_phase" \
    "$MIGRATE" --activate >"$TMP/prior-binding-phase-$rollback_phase.out" \
    2>"$TMP/prior-binding-phase-$rollback_phase.err"; then
    echo "prior-binding rollback hard fault at $rollback_phase unexpectedly succeeded" >&2
    exit 1
  fi
  assert_exact_legacy_rollback "$rollback_snapshot"
  cmp -s "$prior_serving_store_binding" "$TMP/home/.subrouter/codex/.local-serving-store.json"
  [ "$(stat -f '%Lp' "$TMP/home/.subrouter/codex/.local-serving-store.json")" = 400 ]
done
echo "PASS rollback hard faults restored exact prior binding bytes and mode at every post-bootstrap phase"

if PYTHONOPTIMIZE=1 \
  FAKE_ACTIVE_WORKER_CDHASH=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
  "$MIGRATE" --activate >"$TMP/worker-identity-mismatch.out" 2>"$TMP/worker-identity-mismatch.err"; then
  echo "mismatched active worker process unexpectedly accepted" >&2
  exit 1
fi
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
grep -q 'supervised agent failed structural acceptance' "$TMP/worker-identity-mismatch.err"
echo "PASS active worker kernel identity mismatch restored exact legacy before canary"

for rollback_phase in rollback_legacy_bootstrap_requested rollback_legacy_accepted rollback_binding_restored; do
  reset_legacy
  rollback_snapshot="$TMP/pre-candidate-rollback-phase-$rollback_phase.plist"
  cp -p "$plist" "$rollback_snapshot"
  if FAKE_ACTIVE_WORKER_CDHASH=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
    SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
    SUBROUTER_ROLLBACK_FAULT_INJECT_HARD_PHASE="$rollback_phase" \
    "$MIGRATE" --activate >"$TMP/pre-candidate-rollback-phase-$rollback_phase.out" \
    2>"$TMP/pre-candidate-rollback-phase-$rollback_phase.err"; then
    echo "pre-candidate rollback hard fault at $rollback_phase unexpectedly succeeded" >&2
    exit 1
  fi
  assert_exact_legacy_rollback "$rollback_snapshot"
done
echo "PASS pre-candidate rollback hard faults selected the initial recovery generation"

for rollback_phase in rollback_legacy_bootstrap_requested rollback_legacy_accepted rollback_binding_restored; do
  reset_legacy
  rollback_snapshot="$TMP/pre-candidate-fresh-reentry-$rollback_phase.plist"
  cp -p "$plist" "$rollback_snapshot"
  if FAKE_ACTIVE_WORKER_CDHASH=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
    SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
    SUBROUTER_ROLLBACK_FAULT_INJECT_HARD_OWNER_PHASE="$rollback_phase" \
    "$MIGRATE" --activate >"$TMP/pre-candidate-fresh-reentry-$rollback_phase.out" \
    2>"$TMP/pre-candidate-fresh-reentry-$rollback_phase.err"; then
    echo "pre-candidate owner hard fault at $rollback_phase unexpectedly succeeded" >&2
    exit 1
  fi
  wait_for_crash_released_mutation_lease "$mutation_lock"
  [ -d "${plist}.supervisor-transaction" ]
  [ "$(cat "${plist}.supervisor-transaction/phase")" = "$rollback_phase" ]
  [ "$(cat "${plist}.supervisor-transaction/recovery-generation")" = initial ]
  if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
    "$MIGRATE" --activate >"$TMP/pre-candidate-fresh-reentry-$rollback_phase.reentry.out" \
    2>"$TMP/pre-candidate-fresh-reentry-$rollback_phase.reentry.err"; then
    echo "fresh reentry at pre-candidate rollback phase $rollback_phase unexpectedly continued activation" >&2
    exit 1
  fi
  grep -q "recovered interrupted transaction phase $rollback_phase to legacy" \
    "$TMP/pre-candidate-fresh-reentry-$rollback_phase.reentry.err"
  assert_exact_legacy_rollback "$rollback_snapshot"
done
echo "PASS fresh-process reentry selected initial recovery at every pre-candidate rollback phase"

reset_legacy
control_attempt_file="$TMP/control-socket-attempts"
FAKE_CONTROL_SOCKET_FAIL_COUNT=2 FAKE_CONTROL_SOCKET_ATTEMPT_FILE="$control_attempt_file" \
  SUBROUTER_HEALTH_ATTEMPTS=5 SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" \
  SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" "$MIGRATE" --activate \
  >"$TMP/control-socket-convergence.out" 2>"$TMP/control-socket-convergence.err"
[ "$(cat "$control_attempt_file")" = 2 ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
grep -q -- '--serving-store-binding' "$TMP/control-socket-convergence.out"
grep -q -- '--expected-serving-store-binding-sha256' "$TMP/control-socket-convergence.out"
python3 - "$TMP/home/.subrouter/codex/.local-serving-store.json" "$SUBROUTER_STATE_DIR" <<'PY'
import json
import os
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    binding = json.load(source)
assert binding["accounts_dir"] == os.path.realpath(
    os.path.join(sys.argv[2], "codex", "accounts")
)
assert binding["schema"] == "subrouter.local-serving-store/v2"
assert binding["local_data_socket"] == os.environ["SUBROUTER_LOCAL_DATA_SOCKET"]
socket_stat = os.stat(binding["local_data_socket"], follow_symlinks=False)
assert binding["local_data_socket_identity"] == f"unix:{socket_stat.st_dev}:{socket_stat.st_ino}"
PY
echo "PASS supervisor acceptance published the default-shell candidate binding before a state-unset canary"

reset_legacy
concurrent_binding="$TMP/home/.subrouter/codex/.local-serving-store.json"
concurrent_binding_sentinel="$TMP/concurrent-binding-written"
rm -f "$concurrent_binding_sentinel"
if FAKE_CONCURRENT_BINDING_PATH="$concurrent_binding" \
  FAKE_CONCURRENT_BINDING_SENTINEL="$concurrent_binding_sentinel" \
  SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
  "$MIGRATE" --activate >"$TMP/concurrent-binding.out" \
  2>"$TMP/concurrent-binding.err"; then
  echo "concurrent pre-publication serving-store change unexpectedly accepted" >&2
  exit 1
fi
[ -e "$concurrent_binding_sentinel" ]
grep -q '^operator-selected-third-binding$' "$concurrent_binding"
grep -q 'serving-store binding changed before publication' "$TMP/concurrent-binding.err"
[ -d "${plist}.supervisor-transaction" ]
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$supervisor" ]
rm -f "$concurrent_binding"
if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
  "$MIGRATE" --activate >"$TMP/concurrent-binding-recovery.out" \
  2>"$TMP/concurrent-binding-recovery.err"; then
  echo "concurrent binding recovery unexpectedly continued activation" >&2
  exit 1
fi
grep -q 'recovered interrupted transaction phase recovery_candidate_committed to legacy' \
  "$TMP/concurrent-binding-recovery.err"
[ ! -e "$concurrent_binding" ]
echo "PASS activation refused a concurrent pointer change without clobbering it and retained recoverable journal state"

reset_legacy
supervisor_print_attempt_file="$TMP/supervisor-print-attempts"
FAKE_SUPERVISOR_EMPTY_PID_COUNT=2 FAKE_SUPERVISOR_PRINT_ATTEMPT_FILE="$supervisor_print_attempt_file" \
  SUBROUTER_HEALTH_ATTEMPTS=5 SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" \
  SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" "$MIGRATE" --activate \
  >"$TMP/delayed-supervisor-pid.out" 2>"$TMP/delayed-supervisor-pid.err"
[ "$(cat "$supervisor_print_attempt_file")" = 2 ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
echo "PASS delayed launchd supervisor PID visibility converged before structural acceptance"

reset_legacy
restart_sentinel="$TMP/restart-during-http"
restart_http_attempt_file="$TMP/restart-during-http-attempts"
if FAKE_RESTART_DURING_HTTP_AFTER_CALLS=0 \
  FAKE_RESTART_DURING_HTTP_ATTEMPT_FILE="$restart_http_attempt_file" \
  FAKE_RESTART_DURING_HTTP_SENTINEL="$restart_sentinel" \
  SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
  "$MIGRATE" --activate >"$TMP/restart-during-http.out" 2>"$TMP/restart-during-http.err"; then
  echo "supervisor restart during HTTP acceptance unexpectedly succeeded" >&2
  exit 1
fi
[ -e "$restart_sentinel" ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
grep -q 'supervised agent failed structural acceptance' "$TMP/restart-during-http.err"
echo "PASS supervisor restart during HTTP acceptance restored exact legacy"

reset_legacy
post_restart_sentinel="$TMP/restart-during-post-canary-http"
post_restart_attempt_file="$TMP/restart-during-post-canary-http-attempts"
if FAKE_RESTART_DURING_HTTP_AFTER_CALLS=2 \
  FAKE_RESTART_DURING_HTTP_ATTEMPT_FILE="$post_restart_attempt_file" \
  FAKE_RESTART_DURING_HTTP_SENTINEL="$post_restart_sentinel" \
  SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
  "$MIGRATE" --activate >"$TMP/restart-during-post-canary-http.out" \
  2>"$TMP/restart-during-post-canary-http.err"; then
  echo "supervisor restart during post-canary HTTP acceptance unexpectedly succeeded" >&2
  exit 1
fi
[ -e "$post_restart_sentinel" ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
grep -q 'candidate acceptance changed during canary' "$TMP/restart-during-post-canary-http.err"
echo "PASS supervisor restart during post-canary HTTP acceptance restored exact legacy"

reset_legacy
worker_pid_file="$TMP/active-worker.pid"
printf '%s\n' 4242 >"$worker_pid_file"
if FAKE_ACTIVE_WORKER_PID_FILE="$worker_pid_file" \
  SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-switch-worker" \
  "$MIGRATE" --activate >"$TMP/worker-continuity.out" 2>"$TMP/worker-continuity.err"; then
  echo "active worker replacement during canary unexpectedly accepted" >&2
  exit 1
fi
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
grep -q 'candidate acceptance changed during canary' "$TMP/worker-continuity.err"
echo "PASS active worker PID/start continuity change restored exact legacy after canary"

backup="$TMP/manual-backup.plist"
cp -p "$plist" "$backup"
backup_sha="$(shasum -a 256 "$backup" | awk '{print $1}')"
legacy_sha="$(shasum -a 256 "$legacy" | awk '{print $1}')"
legacy_dependency_sha="$(shasum -a 256 "$legacy_dependency" | awk '{print $1}')"
manual_bundle="$TMP/manual-bundle"
mkdir -m 0700 "$manual_bundle"
legacy_artifact="$manual_bundle/$legacy_sha-legacy"
dependency_artifact="$manual_bundle/$legacy_dependency_sha-worker"
cp -p "$legacy" "$legacy_artifact"
cp -p "$legacy_dependency" "$dependency_artifact"
manual_serving_store_binding="$TMP/home/.subrouter/codex/.local-serving-store.json"
manual_serving_store_prior="$manual_bundle/local-serving-store.before.json"
manual_serving_store_candidate="$manual_bundle/local-serving-store.candidate.json"
printf '%s\n' 'prior-serving-store-binding' >"$manual_serving_store_prior"
printf '%s\n' 'candidate-serving-store-binding' >"$manual_serving_store_candidate"
chmod 0400 "$manual_serving_store_prior"
chmod 0600 "$manual_serving_store_candidate"
cp -p "$manual_serving_store_candidate" "$manual_serving_store_binding"
manual_serving_store_prior_sha="$(shasum -a 256 "$manual_serving_store_prior" | awk '{print $1}')"
manual_serving_store_candidate_sha="$(shasum -a 256 "$manual_serving_store_candidate" | awk '{print $1}')"
rollback_identity=(--backup "$backup" --backup-sha256 "$backup_sha" \
  --rollback-artifact "$legacy" "$legacy_artifact" "$legacy_sha" 755 \
  --rollback-artifact "$legacy_dependency" "$dependency_artifact" "$legacy_dependency_sha" 755 \
  --serving-store-binding "$manual_serving_store_binding" \
  --serving-store-binding-backup "$manual_serving_store_prior" \
    "$manual_serving_store_prior_sha" 400 \
  --expected-serving-store-binding-sha256 "$manual_serving_store_candidate_sha")
cat >"$legacy_dependency" <<'SH'
#!/bin/sh
# supervisor dependency bytes that overlap the legacy rollback destination
exit 0
SH
chmod 0755 "$legacy_dependency"
supervisor_dependency_sha="$(shasum -a 256 "$legacy_dependency" | awk '{print $1}')"
launchctl bootout "gui/$(id -u)/$label"
write_plist "$supervisor" supervise "$legacy_dependency"
launchctl bootstrap "gui/$(id -u)" "$plist"
mutation_lock_holder=""
/usr/bin/lockf -k "$mutation_lock" /bin/sleep 30 &
mutation_lock_holder=$!
for _ in $(seq 1 100); do
  if ! /usr/bin/lockf -s -k -t 0 "$mutation_lock" /usr/bin/true; then
    break
  fi
  sleep 0.01
done
if "$ROLLBACK" "${rollback_identity[@]}" --expected-program "$supervisor" \
  >"$TMP/rollback-mutation-lock.out" 2>"$TMP/rollback-mutation-lock.err"; then
  echo "standalone rollback unexpectedly ran while the mutation lease was held" >&2
  kill "$mutation_lock_holder" 2>/dev/null || true
  exit 1
fi
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$supervisor" ]
kill "$mutation_lock_holder" 2>/dev/null || true
wait "$mutation_lock_holder" 2>/dev/null || true
grep -q 'another deployment or worker update holds' "$TMP/rollback-mutation-lock.err"
echo "PASS standalone rollback mutation lease refused concurrent updater or deployment"

binding_recheck_sentinel="$TMP/binding-recheck-mutated"
if FAKE_MUTATE_BINDING_BEFORE_LEGACY_BOOTSTRAP="$manual_serving_store_binding" \
  FAKE_MUTATE_BINDING_SENTINEL="$binding_recheck_sentinel" \
  "$ROLLBACK" "${rollback_identity[@]}" --expected-program "$supervisor" \
  >"$TMP/binding-recheck.out" 2>"$TMP/binding-recheck.err"; then
  echo "standalone rollback unexpectedly bootstrapped after a concurrent binding change" >&2
  exit 1
fi
[ -e "$binding_recheck_sentinel" ]
grep -q '^operator-selected-third-binding$' "$manual_serving_store_binding"
grep -q 'serving-store binding changed immediately before legacy bootstrap; rollback withheld' \
  "$TMP/binding-recheck.err"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$supervisor" ]
[ "$(shasum -a 256 "$legacy_dependency" | awk '{print $1}')" = "$supervisor_dependency_sha" ]
cp -p "$manual_serving_store_candidate" "$manual_serving_store_binding"
echo "PASS standalone rollback preserved a concurrent binding and restored the captured supervisor"

launchctl bootout "gui/$(id -u)/$label"
rm -f "$binding_recheck_sentinel"
if FAKE_MUTATE_BINDING_BEFORE_LEGACY_BOOTSTRAP="$manual_serving_store_binding" \
  FAKE_MUTATE_BINDING_SENTINEL="$binding_recheck_sentinel" \
  "$ROLLBACK" "${rollback_identity[@]}" --expected-program "$supervisor" \
  >"$TMP/unloaded-binding-recheck.out" 2>"$TMP/unloaded-binding-recheck.err"; then
  echo "standalone rollback unexpectedly bootstrapped from an unloaded supervisor state" >&2
  exit 1
fi
[ -e "$binding_recheck_sentinel" ]
grep -q '^operator-selected-third-binding$' "$manual_serving_store_binding"
grep -q 'installed supervisor plist restored, and job left unloaded' \
  "$TMP/unloaded-binding-recheck.err"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
[ "$(shasum -a 256 "$legacy_dependency" | awk '{print $1}')" = "$supervisor_dependency_sha" ]
if launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1; then
  echo "originally unloaded supervisor unexpectedly became loaded" >&2
  exit 1
fi
cp -p "$manual_serving_store_candidate" "$manual_serving_store_binding"
launchctl bootstrap "gui/$(id -u)" "$plist"
echo "PASS standalone rollback restored an unloaded supervisor plist without loading its job"

"$ROLLBACK" "${rollback_identity[@]}" --expected-program "$supervisor" \
  >"$TMP/rollback.out" 2>"$TMP/rollback.err"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
grep -q 'rollback LaunchAgent healthy and ready' "$TMP/rollback.out"
cmp -s "$manual_serving_store_prior" "$manual_serving_store_binding"
[ "$(stat -f '%Lp' "$manual_serving_store_binding")" = 400 ]
echo "PASS standalone rollback enforced identity and restored the exact legacy plist and pointer"

launchctl bootout "gui/$(id -u)/$label"
write_plist "$supervisor" supervise
launchctl bootstrap "gui/$(id -u)" "$plist"
rm -f "$manual_serving_store_binding"
printf '%s\n' 'operator-selected-third-binding' >"$manual_serving_store_binding"
chmod 0600 "$manual_serving_store_binding"
if "$ROLLBACK" "${rollback_identity[@]}" --expected-program "$supervisor" \
  >"$TMP/third-binding.out" 2>"$TMP/third-binding.err"; then
  echo "standalone rollback unexpectedly overwrote a third serving-store binding" >&2
  exit 1
fi
grep -q 'serving-store binding identity check failed; rollback withheld' \
  "$TMP/third-binding.err"
grep -q '^operator-selected-third-binding$' "$manual_serving_store_binding"
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$supervisor" ]
cp -p "$manual_serving_store_candidate" "$manual_serving_store_binding"
echo "PASS standalone rollback refused a concurrent third pointer identity before bootout"

IFS='|' read -r _ mismatch_pid <"$FAKE_LAUNCHD_STATE"
printf '%s|%s\n' "$TMP/home/bin/unexpected" "$mismatch_pid" >"$FAKE_LAUNCHD_STATE"
if "$ROLLBACK" "${rollback_identity[@]}" --expected-program "$supervisor" \
  >"$TMP/program-mismatch.out" 2>"$TMP/program-mismatch.err"; then
  echo "mismatched loaded program unexpectedly accepted" >&2
  exit 1
fi
grep -q 'runs unexpected program' "$TMP/program-mismatch.err"
stop_fake_job
echo "PASS standalone rollback rejected a mismatched loaded program"

write_plist "$supervisor" supervise
launchctl bootstrap "gui/$(id -u)" "$plist"
if FAKE_BOOTOUT_REPLACE_PID=1 "$ROLLBACK" "${rollback_identity[@]}" \
  --expected-program "$supervisor" >"$TMP/pid-mismatch.out" 2>"$TMP/pid-mismatch.err"; then
  echo "changed pid during removal unexpectedly accepted" >&2
  exit 1
fi
grep -q 'changed pid during removal' "$TMP/pid-mismatch.err"
stop_fake_job
echo "PASS standalone rollback rejected a changed pid during removal"

write_plist "$supervisor" supervise
launchctl bootstrap "gui/$(id -u)" "$plist"
tampered_backup="$TMP/tampered-backup.plist"
cp -p "$backup" "$tampered_backup"
printf '\n' >>"$tampered_backup"
if "$ROLLBACK" --backup "$tampered_backup" --backup-sha256 "$backup_sha" \
  --rollback-artifact "$legacy" "$legacy_artifact" "$legacy_sha" 755 \
  --rollback-artifact "$legacy_dependency" "$dependency_artifact" "$legacy_dependency_sha" 755 \
  --expected-program "$supervisor" >"$TMP/tampered-backup.out" 2>"$TMP/tampered-backup.err"; then
  echo "tampered rollback plist unexpectedly accepted" >&2
  exit 1
fi
grep -q 'rollback plist content identity check failed' "$TMP/tampered-backup.err"
launchctl print "gui/$(id -u)/$label" >/dev/null
stop_fake_job
echo "PASS standalone rollback rejected a tampered backup before bootout"

legacy_pristine="$TMP/legacy-pristine"
dependency_pristine="$TMP/dependency-pristine"
cp -p "$legacy" "$legacy_pristine"
cp -p "$legacy_dependency" "$dependency_pristine"
write_plist "$supervisor" supervise
launchctl bootstrap "gui/$(id -u)" "$plist"
printf '\n# changed\n' >>"$legacy"
printf '\n# upgraded dependency\n' >>"$legacy_dependency"
"$ROLLBACK" "${rollback_identity[@]}" --expected-program "$supervisor" \
  >"$TMP/changed-destination.out" 2>"$TMP/changed-destination.err"
[ "$(shasum -a 256 "$legacy" | awk '{print $1}')" = "$legacy_sha" ]
[ "$(shasum -a 256 "$legacy_dependency" | awk '{print $1}')" = "$legacy_dependency_sha" ]
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
cmp -s "$legacy" "$legacy_pristine"
cmp -s "$legacy_dependency" "$dependency_pristine"
echo "PASS standalone rollback exactly restored benignly upgraded destinations"

stop_fake_job
rm -f "$plist"
"$ROLLBACK" "${rollback_identity[@]}" >"$TMP/missing-plist.out" 2>"$TMP/missing-plist.err"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
grep -q 'rollback LaunchAgent healthy and ready' "$TMP/missing-plist.out"
echo "PASS standalone rollback recovered after installed plist absence"

for fault_phase in candidate_plist_installing candidate_plist_installed legacy_bootout_requested legacy_absent candidate_bootstrap_requested candidate_bootstrapped structural_accepted recovery_candidate_committed serving_store_binding_requested serving_store_bound canary_completed; do
  reset_legacy
  if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
    SUBROUTER_FAULT_INJECT_PHASE="$fault_phase" "$MIGRATE" --activate \
    >"$TMP/fault-$fault_phase.out" 2>"$TMP/fault-$fault_phase.err"; then
    echo "fault injection at $fault_phase unexpectedly succeeded" >&2
    exit 1
  fi
  [ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
  [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
  [ ! -e "$TMP/home/.subrouter/codex/.local-serving-store.json" ]
  [ ! -e "${plist}.supervisor-transaction" ]
done
echo "PASS TERM injection at every live transaction boundary restored healthy legacy"

for hard_phase in candidate_plist_installing candidate_bootstrap_requested recovery_candidate_committed serving_store_binding_requested; do
  reset_legacy
  if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
    SUBROUTER_FAULT_INJECT_HARD_PHASE="$hard_phase" "$MIGRATE" --activate \
    >"$TMP/hard-fault-$hard_phase.out" 2>"$TMP/hard-fault-$hard_phase.err"; then
    echo "hard fault injection at $hard_phase unexpectedly succeeded" >&2
    exit 1
  fi
  wait_for_crash_released_mutation_lease "$mutation_lock"
  [ -d "${plist}.supervisor-transaction" ]
  [ "$(cat "${plist}.supervisor-transaction/phase")" = "$hard_phase" ]
  case "$hard_phase" in
    candidate_plist_installing)
      [ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
      [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
      ;;
    candidate_bootstrap_requested)
      [ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
      if launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1; then
        echo "candidate unexpectedly bootstrapped before persisted intent boundary" >&2
        exit 1
      fi
      ;;
    serving_store_binding_requested)
      [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$supervisor" ]
      [ ! -e "$TMP/home/.subrouter/codex/.local-serving-store.json" ]
      ;;
    recovery_candidate_committed)
      [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$supervisor" ]
      candidate_artifact="$(find "$(dirname "$plist")" -maxdepth 2 -name local-serving-store.candidate.json -print | tail -n 1)"
      [ -n "$candidate_artifact" ]
      printf '%s\n' 'operator-selected-third-binding' >"$candidate_artifact"
      chmod 0600 "$candidate_artifact"
      ;;
  esac
  if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
    "$MIGRATE" --activate >"$TMP/reentry-$hard_phase.out" 2>"$TMP/reentry-$hard_phase.err"; then
    echo "reentry recovery at $hard_phase unexpectedly continued activation" >&2
    exit 1
  fi
  grep -q "recovered interrupted transaction phase $hard_phase to legacy" "$TMP/reentry-$hard_phase.err"
  [ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
  [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
  [ ! -e "$TMP/home/.subrouter/codex/.local-serving-store.json" ]
done
echo "PASS pre-mutation SIGKILL journals recovered exact legacy on reentry"

for mutation in candidate_plist_restore candidate_bootstrap serving_store_binding_publish; do
  reset_legacy
  if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
    SUBROUTER_FAULT_INJECT_HARD_AFTER_MUTATION="$mutation" "$MIGRATE" --activate \
    >"$TMP/post-mutation-$mutation.out" 2>"$TMP/post-mutation-$mutation.err"; then
    echo "post-mutation hard fault at $mutation unexpectedly succeeded" >&2
    exit 1
  fi
  wait_for_crash_released_mutation_lease "$mutation_lock"
  [ -d "${plist}.supervisor-transaction" ]
  [ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
  case "$mutation" in
    candidate_plist_restore)
      [ "$(cat "${plist}.supervisor-transaction/phase")" = candidate_plist_installing ]
      [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
      ;;
    candidate_bootstrap)
      [ "$(cat "${plist}.supervisor-transaction/phase")" = candidate_bootstrap_requested ]
      [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$supervisor" ]
      ;;
    serving_store_binding_publish)
      [ "$(cat "${plist}.supervisor-transaction/phase")" = serving_store_binding_requested ]
      [ -f "$TMP/home/.subrouter/codex/.local-serving-store.json" ]
      [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$supervisor" ]
      ;;
  esac
  if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
    "$MIGRATE" --activate >"$TMP/post-mutation-reentry-$mutation.out" \
    2>"$TMP/post-mutation-reentry-$mutation.err"; then
    echo "post-mutation reentry at $mutation unexpectedly continued activation" >&2
    exit 1
  fi
  grep -q 'recovered interrupted transaction phase .* to legacy' \
    "$TMP/post-mutation-reentry-$mutation.err"
  [ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
  [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
  [ ! -e "$TMP/home/.subrouter/codex/.local-serving-store.json" ]
done
echo "PASS post-mutation SIGKILL windows recovered exact healthy legacy on reentry"

reset_legacy
FAKE_BOOTSTRAP_FAIL_ONCE=1 SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" \
  SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" "$MIGRATE" --activate \
  >"$TMP/bootstrap-retry.out" 2>"$TMP/bootstrap-retry.err"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$supervisor" ]
echo "PASS bootstrap retry occurred only after classified full absence"

reset_legacy
export FAKE_WORKER_LOG="$TMP/worker.log"
SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" "$MIGRATE" --activate \
  >"$TMP/default-preflight.out" 2>"$TMP/default-preflight.err"
grep -Fq "$SUBROUTER_STATE_DIR|codex isolation-check --json --retiring-state-dir $TMP/home/.subrouter-retiring|" "$FAKE_WORKER_LOG"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
unset FAKE_WORKER_LOG
echo "PASS default preflight compared candidate and retiring state roots without shell evaluation"

reset_legacy
export FAKE_WORKER_LOG="$TMP/additive-worker.log"
export FAKE_PREFLIGHT_LOG="$TMP/additive-preflight.log"
SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
  "$MIGRATE" --activate >"$TMP/additive-preflight.out" 2>"$TMP/additive-preflight.err"
grep -Fq "$SUBROUTER_STATE_DIR|codex isolation-check --json --retiring-state-dir $TMP/home/.subrouter-retiring|" "$FAKE_WORKER_LOG"
grep -q '^invoked$' "$FAKE_PREFLIGHT_LOG"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
unset FAKE_WORKER_LOG FAKE_PREFLIGHT_LOG
echo "PASS deployment preflight was additive to mandatory credential isolation"

reset_legacy
rollback_bundles_before="$(find "$(dirname "$plist")" -maxdepth 1 -type d -name "$(basename "$plist").rollback-bundle-*" | wc -l | tr -d ' ')"
export FAKE_PREFLIGHT_LOG="$TMP/rejected-preflight.log"
if FAKE_ISOLATION_FAIL=1 SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" \
  SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" "$MIGRATE" --activate \
  >"$TMP/isolation-before-callback.out" 2>"$TMP/isolation-before-callback.err"; then
  echo "failed credential isolation unexpectedly reached activation" >&2
  exit 1
fi
grep -q 'Codex isolation preflight failed' "$TMP/isolation-before-callback.err"
[ ! -e "$FAKE_PREFLIGHT_LOG" ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
[ ! -e "${plist}.supervisor-transaction" ]
rollback_bundles_after="$(find "$(dirname "$plist")" -maxdepth 1 -type d -name "$(basename "$plist").rollback-bundle-*" | wc -l | tr -d ' ')"
[ "$rollback_bundles_after" = "$rollback_bundles_before" ]
if find "$SUBROUTER_STATE_DIR" -maxdepth 1 -name '.codex.migrate-*' -print -quit | grep -q .; then
  echo "credential preflight failure created a migration directory" >&2
  exit 1
fi
unset FAKE_PREFLIGHT_LOG
echo "PASS failed credential isolation stopped before callback, bundle, or live mutation"

reset_legacy
prepare_functional_canary success
SUBROUTER_CANARY_MANIFEST_FILE="$functional_canary_manifest" \
  SUBROUTER_CANARY_CALLBACK="$functional_canary_runner" \
  "$MIGRATE" --activate \
  >"$TMP/functional-canary-success.out" 2>"$TMP/functional-canary-success.err"
assert_functional_canary_order \
  peer-health-readiness \
  authenticated-routed-codex \
  sticky-reuse \
  safe-failover-reuse \
  authenticated-routed-claude \
  existing-session-next-turn
assert_functional_canary_evidence passed \
  peer-health-readiness \
  authenticated-routed-codex \
  sticky-reuse \
  safe-failover-reuse \
  authenticated-routed-claude \
  existing-session-next-turn
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$supervisor" ]
[ ! -e "${plist}.supervisor-transaction" ]
python3 - "$plist" "${plist}.supervised" <<'PY'
import plistlib
import sys

for path in sys.argv[1:]:
    with open(path, "rb") as source:
        environment = plistlib.load(source).get("EnvironmentVariables", {})
    leaked = sorted(key for key in environment if key.startswith("SUBROUTER_CANARY_"))
    if leaked:
        raise SystemExit(f"canary environment leaked into {path}: {leaked}")
PY
echo "PASS real functional canary runner executed every private fixture leg in order without plist environment leakage"

reset_legacy
prepare_functional_canary semantic-failure
semantic_legacy_snapshot="$TMP/functional-canary-semantic-legacy.plist"
cp -p "$plist" "$semantic_legacy_snapshot"
semantic_bundles_before="$(rollback_bundle_count)"
semantic_identities_before="$(rollback_identity_count)"
if SUBROUTER_CANARY_MANIFEST_FILE="$functional_canary_manifest" \
  SUBROUTER_CANARY_CALLBACK="$functional_canary_runner" \
  "$MIGRATE" --activate \
  >"$TMP/functional-canary-semantic.out" 2>"$TMP/functional-canary-semantic.err"; then
  echo "semantic functional canary failure unexpectedly accepted" >&2
  exit 1
fi
assert_exact_legacy_rollback "$semantic_legacy_snapshot"
semantic_bundles_after="$(rollback_bundle_count)"
semantic_identities_after="$(rollback_identity_count)"
[ "$semantic_bundles_after" -eq "$((semantic_bundles_before + 1))" ]
[ "$semantic_identities_after" -eq "$((semantic_identities_before + 1))" ]
assert_functional_canary_order \
  peer-health-readiness \
  authenticated-routed-codex \
  sticky-reuse
assert_functional_canary_evidence failed \
  peer-health-readiness \
  authenticated-routed-codex
grep -q 'leg sticky-reuse did not return its exact success record' \
  "$TMP/functional-canary-semantic.err"
grep -q 'functional canary failed; legacy LaunchAgent restored' \
  "$TMP/functional-canary-semantic.err"
if grep -Fq "$functional_canary_secret" \
  "$TMP/functional-canary-semantic.out" \
  "$TMP/functional-canary-semantic.err" \
  "$functional_canary_evidence"; then
  echo "functional canary child output leaked its synthetic secret" >&2
  exit 1
fi
echo "PASS semantic functional canary failure was redacted, skipped later legs, and retained one exact rollback bundle"

reset_legacy
prepare_functional_canary timeout
timeout_legacy_snapshot="$TMP/functional-canary-timeout-legacy.plist"
cp -p "$plist" "$timeout_legacy_snapshot"
timeout_bundles_before="$(rollback_bundle_count)"
timeout_identities_before="$(rollback_identity_count)"
if SUBROUTER_CANARY_MANIFEST_FILE="$functional_canary_manifest" \
  SUBROUTER_CANARY_CALLBACK="$functional_canary_runner" \
  SUBROUTER_CANARY_TIMEOUT=10 \
  "$MIGRATE" --activate \
  >"$TMP/functional-canary-timeout.out" 2>"$TMP/functional-canary-timeout.err"; then
  echo "timed-out functional canary unexpectedly accepted" >&2
  exit 1
fi
assert_exact_legacy_rollback "$timeout_legacy_snapshot"
timeout_bundles_after="$(rollback_bundle_count)"
timeout_identities_after="$(rollback_identity_count)"
[ "$timeout_bundles_after" -eq "$((timeout_bundles_before + 1))" ]
[ "$timeout_identities_after" -eq "$((timeout_identities_before + 1))" ]
[ -s "$functional_canary_leader_pid" ]
[ -s "$functional_canary_descendant_pid" ]
timeout_leader_pid="$(tr -d '[:space:]' <"$functional_canary_leader_pid")"
timeout_descendant_pid="$(tr -d '[:space:]' <"$functional_canary_descendant_pid")"
wait_for_pid_gone "$timeout_leader_pid"
wait_for_pid_gone "$timeout_descendant_pid"
rm -f "$functional_canary_leader_pid" "$functional_canary_descendant_pid"
assert_functional_canary_order peer-health-readiness
assert_functional_canary_evidence failed
grep -q 'functional canary timed out after 10s' "$TMP/functional-canary-timeout.err"
grep -q 'functional canary failed; legacy LaunchAgent restored' \
  "$TMP/functional-canary-timeout.err"
echo "PASS outer functional canary timeout killed its TERM-ignoring process tree and restored exact legacy"

reset_legacy
prepare_functional_canary timeout
nested_escape_legacy_snapshot="$TMP/nested-escape-legacy.plist"
cp -p "$plist" "$nested_escape_legacy_snapshot"
SUBROUTER_CANARY_MANIFEST_FILE="$functional_canary_manifest" \
  SUBROUTER_CANARY_CALLBACK="$functional_canary_runner" \
  SUBROUTER_CANARY_TIMEOUT=60 \
  "$MIGRATE" --activate \
  >"$TMP/nested-escape.out" 2>"$TMP/nested-escape.err" &
nested_escape_migration_pid=$!
nested_escape_attempts=0
while [ ! -s "$functional_canary_descendant_pid" ]; do
  nested_escape_attempts=$((nested_escape_attempts + 1))
  [ "$nested_escape_attempts" -lt 200 ] \
    || { echo "nested runner escape fixture did not start" >&2; kill -KILL "$nested_escape_migration_pid"; exit 1; }
  sleep 0.05
done
nested_escape_watchdog_pid="$(find_functional_canary_watchdog "$nested_escape_migration_pid" || true)"
case "$nested_escape_watchdog_pid" in ''|*[!0-9]*)
  echo "nested runner escape watchdog was not found" >&2
  kill -KILL "$nested_escape_migration_pid" 2>/dev/null || true
  exit 1
  ;;
esac
nested_escape_leader_pid="$(cat "$functional_canary_leader_pid")"
nested_escape_descendant_pid="$(cat "$functional_canary_descendant_pid")"
kill -KILL "$nested_escape_watchdog_pid"
if wait "$nested_escape_migration_pid"; then
  echo "nested runner watchdog SIGKILL unexpectedly accepted" >&2
  exit 1
fi
wait_for_pid_gone "$nested_escape_leader_pid"
wait_for_pid_gone "$nested_escape_descendant_pid"
assert_exact_legacy_rollback "$nested_escape_legacy_snapshot"
rm -f "$functional_canary_leader_pid" "$functional_canary_descendant_pid"
echo "PASS watchdog SIGKILL drained functional runner's session-escaped leg before rollback"

reset_legacy
immediate_signal_legacy_snapshot="$TMP/immediate-signal-legacy.plist"
immediate_signal_child_pid_file="$TMP/immediate-signal-child.pid"
cp -p "$plist" "$immediate_signal_legacy_snapshot"
immediate_signal_bundles_before="$(rollback_bundle_count)"
immediate_signal_identities_before="$(rollback_identity_count)"
if SUBROUTER_CANARY_CALLBACK="$TMP/canary-wait" \
  SUBROUTER_CANARY_TIMEOUT=60 \
  SUBROUTER_FAULT_INJECT_BOUNDED_NAME='functional canary' \
  SUBROUTER_FAULT_INJECT_BOUNDED_SIGNAL=TERM \
  SUBROUTER_FAULT_INJECT_BOUNDED_CHILD_PID_FILE="$immediate_signal_child_pid_file" \
  "$MIGRATE" --activate \
  >"$TMP/immediate-signal.out" 2>"$TMP/immediate-signal.err"; then
  echo "immediately signaled functional canary unexpectedly accepted" >&2
  exit 1
fi
[ -s "$immediate_signal_child_pid_file" ]
immediate_signal_child_pid="$(tr -d '[:space:]' <"$immediate_signal_child_pid_file")"
case "$immediate_signal_child_pid" in
  ''|*[!0-9]*)
    echo "immediate-signal callback pid was not numeric" >&2
    exit 1
    ;;
esac
wait_for_pid_gone "$immediate_signal_child_pid"
assert_exact_legacy_rollback "$immediate_signal_legacy_snapshot"
immediate_signal_bundles_after="$(rollback_bundle_count)"
immediate_signal_identities_after="$(rollback_identity_count)"
[ "$immediate_signal_bundles_after" -eq "$((immediate_signal_bundles_before + 1))" ]
[ "$immediate_signal_identities_after" -eq "$((immediate_signal_identities_before + 1))" ]
grep -q 'functional canary failed; legacy LaunchAgent restored' \
  "$TMP/immediate-signal.err"
echo "PASS signal pending at callback spawn killed the child before exact legacy rollback"

reset_legacy
prepublish_kill_legacy_snapshot="$TMP/prepublish-kill-legacy.plist"
prepublish_kill_callback_pid_file="$TMP/prepublish-kill-callback.pid"
prepublish_kill_traffic_file="$TMP/prepublish-kill-traffic.log"
prepublish_kill_overlap_sentinel="$TMP/prepublish-kill-rollback-overlap"
cp -p "$plist" "$prepublish_kill_legacy_snapshot"
: >"$prepublish_kill_traffic_file"
if FAKE_CANARY_TRAFFIC_PID_FILE="$prepublish_kill_callback_pid_file" \
  FAKE_CANARY_TRAFFIC_FILE="$prepublish_kill_traffic_file" \
  FAKE_ROLLBACK_TRAFFIC_FILE="$prepublish_kill_traffic_file" \
  FAKE_ROLLBACK_OVERLAP_SENTINEL="$prepublish_kill_overlap_sentinel" \
  SUBROUTER_CANARY_CALLBACK="$TMP/canary-traffic" \
  SUBROUTER_CANARY_TIMEOUT=60 \
  SUBROUTER_FAULT_INJECT_BOUNDED_WATCHDOG_SIGKILL_BEFORE_PUBLISH='functional canary' \
  "$MIGRATE" --activate \
  >"$TMP/prepublish-kill.out" 2>"$TMP/prepublish-kill.err"; then
  echo "pre-publication watchdog SIGKILL unexpectedly accepted" >&2
  exit 1
fi
[ ! -e "$prepublish_kill_callback_pid_file" ]
[ ! -s "$prepublish_kill_traffic_file" ]
[ ! -e "$prepublish_kill_overlap_sentinel" ]
assert_exact_legacy_rollback "$prepublish_kill_legacy_snapshot"
grep -q 'functional canary failed; legacy LaunchAgent restored' "$TMP/prepublish-kill.err"
echo "PASS pre-publication watchdog SIGKILL released no callback traffic before exact legacy rollback"

reset_legacy
watchdog_kill_legacy_snapshot="$TMP/watchdog-kill-legacy.plist"
watchdog_kill_callback_pid_file="$TMP/watchdog-kill-callback.pid"
watchdog_kill_traffic_file="$TMP/watchdog-kill-traffic.log"
watchdog_kill_overlap_sentinel="$TMP/watchdog-kill-rollback-overlap"
cp -p "$plist" "$watchdog_kill_legacy_snapshot"
: >"$watchdog_kill_traffic_file"
FAKE_CANARY_TRAFFIC_PID_FILE="$watchdog_kill_callback_pid_file" \
  FAKE_CANARY_TRAFFIC_FILE="$watchdog_kill_traffic_file" \
  FAKE_ROLLBACK_TRAFFIC_FILE="$watchdog_kill_traffic_file" \
  FAKE_ROLLBACK_OVERLAP_SENTINEL="$watchdog_kill_overlap_sentinel" \
  SUBROUTER_CANARY_CALLBACK="$TMP/canary-traffic" \
  SUBROUTER_CANARY_TIMEOUT=60 \
  "$MIGRATE" --activate \
  >"$TMP/watchdog-kill.out" 2>"$TMP/watchdog-kill.err" &
watchdog_kill_migration_pid=$!
watchdog_kill_attempts=0
while [ ! -s "$watchdog_kill_callback_pid_file" ]; do
  watchdog_kill_attempts=$((watchdog_kill_attempts + 1))
  if [ "$watchdog_kill_attempts" -ge 200 ]; then
    echo "functional canary did not reach watchdog SIGKILL fixture" >&2
    kill -KILL "$watchdog_kill_migration_pid" 2>/dev/null || true
    exit 1
  fi
  sleep 0.05
done
watchdog_kill_watchdog_pid="$(find_functional_canary_watchdog "$watchdog_kill_migration_pid" || true)"
case "$watchdog_kill_watchdog_pid" in ''|*[!0-9]*)
  echo "functional canary watchdog was not found for SIGKILL fixture" >&2
  kill -KILL "$watchdog_kill_migration_pid" 2>/dev/null || true
  exit 1
  ;;
esac
watchdog_kill_callback_pid="$(tr -d '[:space:]' <"$watchdog_kill_callback_pid_file")"
kill -KILL "$watchdog_kill_watchdog_pid"
if wait "$watchdog_kill_migration_pid"; then
  echo "watchdog-only SIGKILL unexpectedly accepted" >&2
  exit 1
fi
wait_for_pid_gone "$watchdog_kill_watchdog_pid"
wait_for_pid_gone "$watchdog_kill_callback_pid"
assert_exact_legacy_rollback "$watchdog_kill_legacy_snapshot"
[ ! -e "$watchdog_kill_overlap_sentinel" ]
watchdog_kill_traffic_size="$(wc -c <"$watchdog_kill_traffic_file")"
sleep 0.2
[ "$(wc -c <"$watchdog_kill_traffic_file")" = "$watchdog_kill_traffic_size" ]
grep -q 'functional canary failed; legacy LaunchAgent restored' "$TMP/watchdog-kill.err"
echo "PASS watchdog-only SIGKILL drained callback traffic before exact legacy rollback"

reset_legacy
leader_exit_legacy_snapshot="$TMP/leader-exit-legacy.plist"
leader_exit_child_pid_file="$TMP/leader-exit-child.pid"
leader_exit_traffic_file="$TMP/leader-exit-traffic.log"
leader_exit_overlap_sentinel="$TMP/leader-exit-rollback-overlap"
cp -p "$plist" "$leader_exit_legacy_snapshot"
: >"$leader_exit_traffic_file"
FAKE_CANARY_TRAFFIC_PID_FILE="$leader_exit_child_pid_file" \
  FAKE_CANARY_TRAFFIC_FILE="$leader_exit_traffic_file" \
  FAKE_ROLLBACK_TRAFFIC_FILE="$leader_exit_traffic_file" \
  FAKE_ROLLBACK_OVERLAP_SENTINEL="$leader_exit_overlap_sentinel" \
  SUBROUTER_CANARY_CALLBACK="$TMP/canary-leader-exits" \
  SUBROUTER_CANARY_TIMEOUT=60 \
  "$MIGRATE" --activate \
  >"$TMP/leader-exit.out" 2>"$TMP/leader-exit.err" &
leader_exit_migration_pid=$!
leader_exit_attempts=0
while [ ! -s "$leader_exit_child_pid_file" ]; do
  leader_exit_attempts=$((leader_exit_attempts + 1))
  [ "$leader_exit_attempts" -lt 200 ] \
    || { echo "leader-exit fixture did not start" >&2; kill -KILL "$leader_exit_migration_pid"; exit 1; }
  sleep 0.05
done
leader_exit_watchdog_pid="$(find_functional_canary_watchdog "$leader_exit_migration_pid" || true)"
case "$leader_exit_watchdog_pid" in ''|*[!0-9]*)
  echo "leader-exit watchdog was not found" >&2
  kill -KILL "$leader_exit_migration_pid" 2>/dev/null || true
  exit 1
  ;;
esac
leader_exit_child_pid="$(cat "$leader_exit_child_pid_file")"
kill -KILL "$leader_exit_watchdog_pid"
if wait "$leader_exit_migration_pid"; then
  echo "leader-exit watchdog SIGKILL unexpectedly accepted" >&2
  exit 1
fi
wait_for_pid_gone "$leader_exit_child_pid"
assert_exact_legacy_rollback "$leader_exit_legacy_snapshot"
[ ! -e "$leader_exit_overlap_sentinel" ]
leader_exit_traffic_size="$(wc -c <"$leader_exit_traffic_file")"
sleep 0.2
[ "$(wc -c <"$leader_exit_traffic_file")" = "$leader_exit_traffic_size" ]
echo "PASS stable group anchor drained descendants after callback leader exit before rollback"

reset_legacy
session_escape_legacy_snapshot="$TMP/session-escape-legacy.plist"
session_escape_child_pid_file="$TMP/session-escape-child.pid"
session_escape_traffic_file="$TMP/session-escape-traffic.log"
session_escape_overlap_sentinel="$TMP/session-escape-rollback-overlap"
cp -p "$plist" "$session_escape_legacy_snapshot"
: >"$session_escape_traffic_file"
FAKE_CANARY_TRAFFIC_PID_FILE="$session_escape_child_pid_file" \
  FAKE_CANARY_TRAFFIC_FILE="$session_escape_traffic_file" \
  FAKE_ROLLBACK_TRAFFIC_FILE="$session_escape_traffic_file" \
  FAKE_ROLLBACK_OVERLAP_SENTINEL="$session_escape_overlap_sentinel" \
  SUBROUTER_CANARY_CALLBACK="$TMP/canary-session-escape.py" \
  SUBROUTER_CANARY_TIMEOUT=60 \
  "$MIGRATE" --activate \
  >"$TMP/session-escape.out" 2>"$TMP/session-escape.err" &
session_escape_migration_pid=$!
session_escape_attempts=0
while [ ! -s "$session_escape_child_pid_file" ]; do
  session_escape_attempts=$((session_escape_attempts + 1))
  [ "$session_escape_attempts" -lt 200 ] \
    || { echo "session-escape fixture did not start" >&2; kill -KILL "$session_escape_migration_pid"; exit 1; }
  sleep 0.05
done
session_escape_watchdog_pid="$(find_functional_canary_watchdog "$session_escape_migration_pid" || true)"
case "$session_escape_watchdog_pid" in ''|*[!0-9]*)
  echo "session-escape watchdog was not found" >&2
  kill -KILL "$session_escape_migration_pid" 2>/dev/null || true
  exit 1
  ;;
esac
session_escape_child_pid="$(cat "$session_escape_child_pid_file")"
kill -KILL "$session_escape_watchdog_pid"
if wait "$session_escape_migration_pid"; then
  echo "session-escape watchdog SIGKILL unexpectedly accepted" >&2
  exit 1
fi
wait_for_pid_gone "$session_escape_child_pid"
assert_exact_legacy_rollback "$session_escape_legacy_snapshot"
[ ! -e "$session_escape_overlap_sentinel" ]
session_escape_traffic_size="$(wc -c <"$session_escape_traffic_file")"
sleep 0.2
[ "$(wc -c <"$session_escape_traffic_file")" = "$session_escape_traffic_size" ]
echo "PASS watchdog SIGKILL drained session-escaped callback descendant before rollback"

reset_legacy
session_escape_timeout_legacy_snapshot="$TMP/session-escape-timeout-legacy.plist"
session_escape_timeout_child_pid_file="$TMP/session-escape-timeout-child.pid"
session_escape_timeout_traffic_file="$TMP/session-escape-timeout-traffic.log"
session_escape_timeout_overlap_sentinel="$TMP/session-escape-timeout-rollback-overlap"
cp -p "$plist" "$session_escape_timeout_legacy_snapshot"
: >"$session_escape_timeout_traffic_file"
if FAKE_CANARY_TRAFFIC_PID_FILE="$session_escape_timeout_child_pid_file" \
  FAKE_CANARY_TRAFFIC_FILE="$session_escape_timeout_traffic_file" \
  FAKE_ROLLBACK_TRAFFIC_FILE="$session_escape_timeout_traffic_file" \
  FAKE_ROLLBACK_OVERLAP_SENTINEL="$session_escape_timeout_overlap_sentinel" \
  SUBROUTER_CANARY_CALLBACK="$TMP/canary-session-escape.py" \
  SUBROUTER_CANARY_TIMEOUT=1 \
  "$MIGRATE" --activate \
  >"$TMP/session-escape-timeout.out" 2>"$TMP/session-escape-timeout.err"; then
  echo "session-escape timeout unexpectedly accepted" >&2
  exit 1
fi
[ -s "$session_escape_timeout_child_pid_file" ]
session_escape_timeout_child_pid="$(cat "$session_escape_timeout_child_pid_file")"
wait_for_pid_gone "$session_escape_timeout_child_pid"
assert_exact_legacy_rollback "$session_escape_timeout_legacy_snapshot"
[ ! -e "$session_escape_timeout_overlap_sentinel" ]
session_escape_timeout_traffic_size="$(wc -c <"$session_escape_timeout_traffic_file")"
sleep 0.2
[ "$(wc -c <"$session_escape_timeout_traffic_file")" = "$session_escape_timeout_traffic_size" ]
grep -q 'functional canary timed out after 1s' "$TMP/session-escape-timeout.err"
echo "PASS watchdog timeout cleanup drained token-marked session escape after anchor death before rollback"

stale_group_state="$TMP/stale-group-state.json"
python3 - "$TMP/stale-group.pid" <<'PY' &
import os
import sys
import time

os.setsid()
with open(sys.argv[1], "w", encoding="ascii") as output:
    output.write(f"{os.getpid()}\n")
    output.flush()
    os.fsync(output.fileno())
time.sleep(300)
PY
stale_group_launcher_pid=$!
stale_group_attempts=0
while [ ! -s "$TMP/stale-group.pid" ]; do
  stale_group_attempts=$((stale_group_attempts + 1))
  [ "$stale_group_attempts" -lt 100 ] || { echo "stale group fixture did not start" >&2; exit 1; }
  sleep 0.02
done
stale_group_pid="$(cat "$TMP/stale-group.pid")"
printf '{"callback_token":"%s","leader_start":"darwin:stale:identity","pgid":%s}\n' \
  "$(printf '0%.0s' {1..64})" \
  "$stale_group_pid" >"$stale_group_state"
bash -c '. "$1"; drain_bounded_process_group "stale identity test" "$2"' \
  bash "$TRANSITION_LIB" "$stale_group_state"
kill -0 "$stale_group_pid"
kill -KILL "$stale_group_pid"
wait "$stale_group_launcher_pid" 2>/dev/null || true
rm -f "$TMP/stale-group.pid"
echo "PASS stale persisted process-group identity never signaled a live replacement"

pgid_reuse_state="$TMP/pgid-reuse-state.json"
pgid_reuse_identity="$TMP/pgid-reuse-leader-start"
pgid_reuse_token="$(printf '1%.0s' {1..64})"
python3 - "$TMP/pgid-reuse-group.pid" "$pgid_reuse_identity" <<'PY' &
import ctypes
import os
import sys
import time

pid_file, identity_file = sys.argv[1:]
os.setsid()

if sys.platform == "darwin":
    class DarwinBSDInfo(ctypes.Structure):
        _fields_ = [
            ("pbi_flags", ctypes.c_uint32), ("pbi_status", ctypes.c_uint32),
            ("pbi_xstatus", ctypes.c_uint32), ("pbi_pid", ctypes.c_uint32),
            ("pbi_ppid", ctypes.c_uint32), ("pbi_uid", ctypes.c_uint32),
            ("pbi_gid", ctypes.c_uint32), ("pbi_ruid", ctypes.c_uint32),
            ("pbi_rgid", ctypes.c_uint32), ("pbi_svuid", ctypes.c_uint32),
            ("pbi_svgid", ctypes.c_uint32), ("rfu_1", ctypes.c_uint32),
            ("pbi_comm", ctypes.c_char * 16), ("pbi_name", ctypes.c_char * 32),
            ("pbi_nfiles", ctypes.c_uint32), ("pbi_pgid", ctypes.c_uint32),
            ("pbi_pjobc", ctypes.c_uint32), ("e_tdev", ctypes.c_uint32),
            ("e_tpgid", ctypes.c_uint32), ("pbi_nice", ctypes.c_int32),
            ("pbi_start_tvsec", ctypes.c_uint64),
            ("pbi_start_tvusec", ctypes.c_uint64),
        ]

    libproc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
    proc_pidinfo = libproc.proc_pidinfo
    proc_pidinfo.argtypes = [
        ctypes.c_int, ctypes.c_int, ctypes.c_uint64,
        ctypes.c_void_p, ctypes.c_int,
    ]
    proc_pidinfo.restype = ctypes.c_int
    info = DarwinBSDInfo()
    size = ctypes.sizeof(info)
    if proc_pidinfo(os.getpid(), 3, 0, ctypes.byref(info), size) != size:
        raise SystemExit("PGID reuse fixture could not read its start identity")
    leader_start = f"darwin:{info.pbi_start_tvsec}:{info.pbi_start_tvusec}"
elif sys.platform.startswith("linux"):
    fields = open(f"/proc/{os.getpid()}/stat", encoding="ascii").read().rsplit(")", 1)[1].split()
    leader_start = f"linux:{fields[19]}"
else:
    raise SystemExit("PGID reuse fixture requires Darwin or Linux")

with open(pid_file, "w", encoding="ascii") as output:
    output.write(f"{os.getpid()}\n")
    output.flush()
    os.fsync(output.fileno())
with open(identity_file, "w", encoding="ascii") as output:
    output.write(f"{leader_start}\n")
    output.flush()
    os.fsync(output.fileno())
time.sleep(300)
PY
pgid_reuse_group_launcher_pid=$!
pgid_reuse_attempts=0
while [ ! -s "$pgid_reuse_identity" ]; do
  pgid_reuse_attempts=$((pgid_reuse_attempts + 1))
  [ "$pgid_reuse_attempts" -lt 100 ] \
    || { echo "PGID reuse fixture did not start" >&2; exit 1; }
  sleep 0.02
done
pgid_reuse_group_pid="$(cat "$TMP/pgid-reuse-group.pid")"
pgid_reuse_leader_start="$(cat "$pgid_reuse_identity")"
printf '{"callback_token":"%s","leader_start":"%s","pgid":%s}\n' \
  "$pgid_reuse_token" "$pgid_reuse_leader_start" "$pgid_reuse_group_pid" \
  >"$pgid_reuse_state"
SUBROUTER_BOUNDED_CALLBACK_TOKEN="$pgid_reuse_token" \
  python3 -c 'import time; time.sleep(300)' &
pgid_reuse_token_pid=$!
printf '%s\n' "$pgid_reuse_token_pid" >"$TMP/pgid-reuse-token.pid"
SUBROUTER_FAULT_INJECT_BOUNDED_DRAIN_PGID_REUSE_AFTER_SAMPLE="pgid reuse simulation" \
  bash -c '. "$1"; drain_bounded_process_group "pgid reuse simulation" "$2"' \
  bash "$TRANSITION_LIB" "$pgid_reuse_state"
kill -0 "$pgid_reuse_group_pid"
wait_for_pid_gone "$pgid_reuse_token_pid"
kill -KILL "$pgid_reuse_group_pid"
wait "$pgid_reuse_group_launcher_pid" 2>/dev/null || true
rm -f "$TMP/pgid-reuse-group.pid" "$TMP/pgid-reuse-token.pid"
echo "PASS simulated PID/PGID reuse never signaled the unrelated group while token descendants drained"

reset_legacy
prepare_functional_canary timeout
orphan_legacy_snapshot="$TMP/functional-canary-orphan-legacy.plist"
cp -p "$plist" "$orphan_legacy_snapshot"
SUBROUTER_CANARY_MANIFEST_FILE="$functional_canary_manifest" \
  SUBROUTER_CANARY_CALLBACK="$functional_canary_runner" \
  SUBROUTER_CANARY_TIMEOUT=60 \
  "$MIGRATE" --activate \
  >"$TMP/functional-canary-orphan.out" 2>"$TMP/functional-canary-orphan.err" &
orphan_migration_pid=$!
orphan_attempts=0
while [ ! -s "$functional_canary_descendant_pid" ]; do
  orphan_attempts=$((orphan_attempts + 1))
  if [ "$orphan_attempts" -ge 200 ]; then
    echo "functional canary did not reach its orphan fixture" >&2
    kill -KILL "$orphan_migration_pid" 2>/dev/null || true
    exit 1
  fi
  sleep 0.05
done
orphan_leader_pid="$(tr -d '[:space:]' <"$functional_canary_leader_pid")"
orphan_descendant_pid="$(tr -d '[:space:]' <"$functional_canary_descendant_pid")"
kill -KILL "$orphan_migration_pid"
wait "$orphan_migration_pid" 2>/dev/null || true
wait_for_pid_gone "$orphan_leader_pid"
wait_for_pid_gone "$orphan_descendant_pid"
wait_for_crash_released_mutation_lease "$mutation_lock"
if SUBROUTER_CANARY_MANIFEST_FILE="$functional_canary_manifest" \
  SUBROUTER_CANARY_CALLBACK="$functional_canary_runner" \
  "$MIGRATE" --activate \
  >"$TMP/functional-canary-orphan-recovery.out" \
  2>"$TMP/functional-canary-orphan-recovery.err"; then
  echo "interrupted functional canary recovery unexpectedly activated" >&2
  exit 1
fi
assert_exact_legacy_rollback "$orphan_legacy_snapshot"
grep -q 'recovered interrupted transaction phase serving_store_bound to legacy; rerun activation' \
  "$TMP/functional-canary-orphan-recovery.err"
rm -f "$functional_canary_leader_pid" "$functional_canary_descendant_pid"
echo "PASS migration death orphaned no functional-canary process and reentry restored exact legacy"

reset_legacy
prepare_functional_canary timeout
signal_legacy_snapshot="$TMP/functional-canary-signal-legacy.plist"
cp -p "$plist" "$signal_legacy_snapshot"
SUBROUTER_CANARY_MANIFEST_FILE="$functional_canary_manifest" \
  SUBROUTER_CANARY_CALLBACK="$functional_canary_runner" \
  SUBROUTER_CANARY_TIMEOUT=60 \
  "$MIGRATE" --activate \
  >"$TMP/functional-canary-signal.out" 2>"$TMP/functional-canary-signal.err" &
signal_migration_pid=$!
signal_attempts=0
while [ ! -s "$functional_canary_descendant_pid" ]; do
  signal_attempts=$((signal_attempts + 1))
  if [ "$signal_attempts" -ge 200 ]; then
    echo "functional canary did not reach its signal fixture" >&2
    kill -KILL "$signal_migration_pid" 2>/dev/null || true
    exit 1
  fi
  sleep 0.05
done
signal_watchdog_pid="$(find_functional_canary_watchdog "$signal_migration_pid" || true)"
case "$signal_watchdog_pid" in ''|*[!0-9]*)
  echo "functional canary watchdog was not found" >&2
  kill -KILL "$signal_migration_pid" 2>/dev/null || true
  exit 1
  ;;
esac
signal_leader_pid="$(tr -d '[:space:]' <"$functional_canary_leader_pid")"
signal_descendant_pid="$(tr -d '[:space:]' <"$functional_canary_descendant_pid")"
kill -INT "$signal_watchdog_pid" "$signal_migration_pid"
wait "$signal_migration_pid" 2>/dev/null || true
wait_for_pid_gone "$signal_watchdog_pid"
wait_for_pid_gone "$signal_leader_pid"
wait_for_pid_gone "$signal_descendant_pid"
assert_exact_legacy_rollback "$signal_legacy_snapshot"
rm -f "$functional_canary_leader_pid" "$functional_canary_descendant_pid"
echo "PASS simultaneous migration/watchdog interrupt killed the isolated canary group before rollback completed"

reset_legacy
if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
  "$MIGRATE" --activate --retiring-state-dir "$SUBROUTER_STATE_DIR" \
  >"$TMP/equal-root.out" 2>"$TMP/equal-root.err"; then
  echo "equal candidate and retiring roots unexpectedly accepted" >&2
  exit 1
fi
grep -q 'candidate and retiring state roots must be different' "$TMP/equal-root.err"
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
echo "PASS equal candidate and retiring state roots failed before live mutation"

worker_args_json="$TMP/worker-serve-args.json"
candidate_env_json="$TMP/candidate-env.json"
private_token_file="$TMP/private-token-file"
printf '%s\n' 'test-only-placeholder' >"$private_token_file"
chmod 0600 "$private_token_file"
cat >"$worker_args_json" <<EOF
["--quota-mode", "safe", "--token-file", "$private_token_file"]
EOF
cat >"$candidate_env_json" <<EOF
{"SUBROUTER_TOKEN_FILE": "$private_token_file"}
EOF
ln -s "$worker_args_json" "$TMP/worker-args-link.json"
reset_legacy
write_wrapper_plist
if "$MIGRATE" --public-addr 127.0.0.1:43199 \
  --worker-serve-args-json "$TMP/worker-args-link.json" \
  >"$TMP/wrapper-symlink.out" 2>"$TMP/wrapper-symlink.err"; then
  echo "symlink JSON input unexpectedly accepted" >&2
  exit 1
fi
grep -q 'must be a regular non-symlink file' "$TMP/wrapper-symlink.err"

cat >"$TMP/worker-args-duplicate.json" <<'EOF'
["serve", "--addr=127.0.0.1:1"]
EOF
if "$MIGRATE" --public-addr 127.0.0.1:43199 \
  --worker-serve-args-json "$TMP/worker-args-duplicate.json" \
  >"$TMP/wrapper-duplicate.out" 2>"$TMP/wrapper-duplicate.err"; then
  echo "embedded serve/addr arguments unexpectedly accepted" >&2
  exit 1
fi
grep -Eq 'must not embed (the serve subcommand|--addr)' "$TMP/wrapper-duplicate.err"

if "$MIGRATE" --public-addr ::1:43199 \
  --worker-serve-args-json "$worker_args_json" \
  >"$TMP/wrapper-unbracketed-ipv6.out" 2>"$TMP/wrapper-unbracketed-ipv6.err"; then
  echo "unbracketed IPv6 public address unexpectedly accepted" >&2
  exit 1
fi
grep -q 'IPv6 public address must use \[IPv6\]:PORT form' \
  "$TMP/wrapper-unbracketed-ipv6.err"
echo "PASS migration rejected an unbracketed IPv6 public address"

assert_candidate_env_rejected() {
  local name="$1" input="$2" pattern="$3"
  if "$MIGRATE" --public-addr 127.0.0.1:43199 \
    --worker-serve-args-json "$worker_args_json" --candidate-env-json "$input" \
    >"$TMP/env-$name.out" 2>"$TMP/env-$name.err"; then
    echo "unsafe candidate environment $name unexpectedly accepted" >&2
    exit 1
  fi
  grep -Eq "$pattern" "$TMP/env-$name.err"
}

cat >"$TMP/env-raw-secret.json" <<'EOF'
{"SUBROUTER_ADMIN_TOKEN": "raw-secret-value"}
EOF
assert_candidate_env_rejected raw-secret "$TMP/env-raw-secret.json" 'raw secrets and non-file overrides are forbidden'
cat >"$TMP/env-non-file-key.json" <<'EOF'
{"CANDIDATE_MODE": "isolated"}
EOF
assert_candidate_env_rejected non-file-key "$TMP/env-non-file-key.json" 'keys must match SUBROUTER_.*_FILE'
cat >"$TMP/env-relative.json" <<'EOF'
{"SUBROUTER_TOKEN_FILE": "relative/token"}
EOF
assert_candidate_env_rejected relative "$TMP/env-relative.json" 'must be an absolute path'
cat >"$TMP/env-missing.json" <<EOF
{"SUBROUTER_TOKEN_FILE": "$TMP/missing-token-file"}
EOF
assert_candidate_env_rejected missing "$TMP/env-missing.json" 'is not safely openable'
ln -s "$private_token_file" "$TMP/token-file-link"
cat >"$TMP/env-value-symlink.json" <<EOF
{"SUBROUTER_TOKEN_FILE": "$TMP/token-file-link"}
EOF
assert_candidate_env_rejected symlink "$TMP/env-value-symlink.json" 'is not safely openable'
unsafe_token_file="$TMP/unsafe-token-file"
printf '%s\n' 'test-only-placeholder' >"$unsafe_token_file"
chmod 0644 "$unsafe_token_file"
cat >"$TMP/env-unsafe-mode.json" <<EOF
{"SUBROUTER_TOKEN_FILE": "$unsafe_token_file"}
EOF
assert_candidate_env_rejected unsafe-mode "$TMP/env-unsafe-mode.json" 'has group or other permissions'

if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
  SUBROUTER_FAULT_INJECT_HARD_PHASE=candidate_plist_installing \
  "$MIGRATE" --activate --public-addr 127.0.0.1:43199 \
  --worker-serve-args-json "$worker_args_json" \
  --candidate-env-json "$candidate_env_json" \
  >"$TMP/wrapper-hard-fault.out" 2>"$TMP/wrapper-hard-fault.err"; then
  echo "wrapper-backed hard fault unexpectedly succeeded" >&2
  exit 1
fi
wait_for_crash_released_mutation_lease "$mutation_lock"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
  "$MIGRATE" --activate --public-addr 127.0.0.1:43199 \
  --worker-serve-args-json "$worker_args_json" \
  --candidate-env-json "$candidate_env_json" \
  >"$TMP/wrapper-hard-fault-reentry.out" 2>"$TMP/wrapper-hard-fault-reentry.err"; then
  echo "wrapper-backed hard-fault reentry unexpectedly continued activation" >&2
  exit 1
fi
grep -q 'recovered interrupted transaction phase candidate_plist_installing to legacy' \
  "$TMP/wrapper-hard-fault-reentry.err"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
[ ! -e "${plist}.supervisor-transaction" ]
echo "PASS wrapper-backed pre-install SIGKILL recovered exact legacy on reentry"

"$MIGRATE" --public-addr 127.0.0.1:43199 \
  --worker-serve-args-json "$worker_args_json" \
  --candidate-env-json "$candidate_env_json" \
  >"$TMP/wrapper-prepare.out" 2>"$TMP/wrapper-prepare.err"
wrapper_prepared="${plist}.supervised"
[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$wrapper_prepared")" = "$supervisor" ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:1' "$wrapper_prepared")" = supervise ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:3' "$wrapper_prepared")" = 127.0.0.1:43199 ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:10' "$wrapper_prepared")" = --upgrade-inhibit-file ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:11' "$wrapper_prepared")" = "${plist}.supervisor-transaction/upgrade-inhibited" ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:13' "$wrapper_prepared")" = --quota-mode ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables:SUBROUTER_TOKEN_FILE' "$wrapper_prepared")" = "$private_token_file" ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables:LEGACY_ONLY' "$wrapper_prepared")" = preserved ]
if /usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables:SUBROUTER_TOKEN_FILE' "$plist" >/dev/null 2>&1; then
  echo "candidate environment override leaked into retiring plist" >&2
  exit 1
fi

if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-fail" \
  "$MIGRATE" --activate --public-addr 127.0.0.1:43199 \
  --worker-serve-args-json "$worker_args_json" \
  --candidate-env-json "$candidate_env_json" \
  >"$TMP/wrapper-activate.out" 2>"$TMP/wrapper-activate.err"; then
  echo "wrapper-backed failing canary unexpectedly accepted" >&2
  exit 1
fi
grep -q 'functional canary failed; legacy LaunchAgent restored' "$TMP/wrapper-activate.err"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$plist")" = "$legacy" ]
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
echo "PASS wrapper-only plist prepared safely and canary failure restored exact legacy"

wrapper_backup="$TMP/wrapper-backup.plist"
cp -p "$plist" "$wrapper_backup"
wrapper_backup_sha="$(shasum -a 256 "$wrapper_backup" | awk '{print $1}')"
launchctl bootout "gui/$(id -u)/$label"
rm -f "$plist"
"$ROLLBACK" --backup "$wrapper_backup" --backup-sha256 "$wrapper_backup_sha" \
  --rollback-artifact "$legacy" "$legacy_artifact" "$legacy_sha" 755 \
  --rollback-artifact "$legacy_dependency" "$dependency_artifact" "$legacy_dependency_sha" 755 \
  --public-addr 127.0.0.1:43199 \
  >"$TMP/wrapper-missing-plist.out" 2>"$TMP/wrapper-missing-plist.err"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
grep -q 'rollback LaunchAgent healthy and ready' "$TMP/wrapper-missing-plist.out"
echo "PASS wrapper-backed standalone rollback recovered after installed plist absence"

launchctl bootout "gui/$(id -u)/$label"
write_plist "$supervisor" supervise
launchctl bootstrap "gui/$(id -u)" "$plist"
if "$ROLLBACK" --backup "$wrapper_backup" --backup-sha256 "$wrapper_backup_sha" \
  --rollback-artifact "$legacy" "$legacy_artifact" "$legacy_sha" 755 \
  --rollback-artifact "$legacy_dependency" "$dependency_artifact" "$legacy_dependency_sha" 755 \
  --public-addr ::1:43199 \
  >"$TMP/wrapper-invalid-address.out" 2>"$TMP/wrapper-invalid-address.err"; then
  echo "unbracketed IPv6 rollback address unexpectedly accepted" >&2
  exit 1
fi
grep -q 'IPv6 public address must use \[IPv6\]:PORT form' "$TMP/wrapper-invalid-address.err"
launchctl print "gui/$(id -u)/$label" >/dev/null
echo "PASS standalone rollback rejected an invalid address before bootout"

if "$ROLLBACK" --backup "$wrapper_backup" --backup-sha256 "$wrapper_backup_sha" \
  --rollback-artifact "$legacy" "$legacy_artifact" "$legacy_sha" 755 \
  --rollback-artifact "$legacy_dependency" "$dependency_artifact" "$legacy_dependency_sha" 755 \
  --public-addr 127.0.0.1:43200 \
  >"$TMP/wrapper-address-mismatch.out" 2>"$TMP/wrapper-address-mismatch.err"; then
  echo "mismatched public address override unexpectedly accepted" >&2
  exit 1
fi
grep -q 'public address override does not match installed plist' "$TMP/wrapper-address-mismatch.err"
echo "PASS standalone rollback rejected a public address override mismatch"
