#!/usr/bin/env bash
# Exercises subrouter-guard.sh decision paths against a fake launchd and a
# file-backed health probe. Nothing here touches the real service.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="$HERE/../subrouter-guard.sh"
failures=0

check() { # check <description> <condition-result>
  if [ "$2" -eq 0 ]; then
    printf 'ok   %s\n' "$1"
  else
    printf 'FAIL %s\n' "$1"
    failures=$((failures + 1))
  fi
}

setup() {
  ROOT="$(mktemp -d)"
  mkdir -p "$ROOT/state" "$ROOT/bin"
  printf 'live\n' >"$ROOT/bin/subrouter"
  chmod 0755 "$ROOT/bin/subrouter"
  cat >"$ROOT/bin/launchctl" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$LAUNCHCTL_CALLS"
[ "${1:-}" = "print" ] && exit "${LAUNCHCTL_PRINT_EXIT:-0}"
exit 0
FAKE
  chmod 0755 "$ROOT/bin/launchctl"
  : >"$ROOT/calls"
  export LAUNCHCTL_CALLS="$ROOT/calls"
  export SUBROUTER_VERIFY_STATE="$ROOT/state"
  export SUBROUTER_BIN="$ROOT/bin/subrouter"
  export SUBROUTER_SUPERVISOR_BIN="$ROOT/bin/subrouter-supervisor"
  export SUBROUTER_LAST_GOOD="$ROOT/state/subrouter.last-good"
  export SUBROUTER_VERSION_FILE="$ROOT/etc/subrouter-version"
  mkdir -p "$ROOT/etc"
  printf 'v9.9.9\n' >"$SUBROUTER_VERSION_FILE"
  export SUBROUTER_HEALTH_URL="file://$ROOT/health"
  export SUBROUTER_PLIST="$ROOT/service.plist"
  export SUBROUTER_LAUNCHCTL="$ROOT/bin/launchctl"
  export SUBROUTER_UPGRADE_INHIBIT_FILE="$ROOT/transaction/upgrade-inhibited"
  export SUBROUTER_DEPLOY_LOCK_DIR="$ROOT/state/deploy.lock"
  export SUBROUTER_GUARD_LOCK_DIR="$ROOT/state/guard.lock"
  export SUBROUTER_GUARD_HEALTH_WAIT_SECS=1
  export SUBROUTER_GUARD_RESTART_WAIT_SECS=1
  export SUBROUTER_GUARD_PROBE_TIMEOUT_SECS=2
  : >"$ROOT/service.plist"
}

teardown() { rm -rf "$ROOT"; }

healthy() { printf 'ok\n' >"$ROOT/health"; }
unhealthy() { rm -f "$ROOT/health"; }

# 1. A healthy pass records the serving binary as last-good.
setup
healthy
bash "$GUARD" >/dev/null 2>&1
[ -f "$SUBROUTER_LAST_GOOD" ] && [ "$(cat "$SUBROUTER_LAST_GOOD")" = "live" ]
check "healthy pass records last-good" $?

# 2. A single failed probe only counts a strike.
unhealthy
bash "$GUARD" >/dev/null 2>&1
[ "$(cat "$ROOT/state/guard.strikes")" = "1" ] && [ ! -s "$LAUNCHCTL_CALLS" ]
check "first failure takes no action" $?
teardown

# 3. Two failures on a binary that differs from last-good roll it back.
setup
healthy
bash "$GUARD" >/dev/null 2>&1          # records last-good = "live"
printf 'broken\n' >"$ROOT/bin/subrouter"
unhealthy
bash "$GUARD" >/dev/null 2>&1
bash "$GUARD" >/dev/null 2>&1
[ "$(cat "$ROOT/bin/subrouter")" = "live" ]
check "second failure restores the last-good worker" $?
grep -q "^bootout system/" "$LAUNCHCTL_CALLS" && grep -q "^bootstrap system " "$LAUNCHCTL_CALLS"
check "rollback restarts the service with bootout then bootstrap" $?
ls "$ROOT"/bin/subrouter.rejected-* >/dev/null 2>&1
check "rollback keeps the rejected binary for inspection" $?
[ -f "$SUBROUTER_UPGRADE_INHIBIT_FILE" ]
check "rollback pauses worker autoupdate so a bad release cannot flap" $?
live_sha="$(printf 'live\n' | shasum -a 256 | awk '{print $1}')"
[ "$(cat "$SUBROUTER_VERSION_FILE")" = "rollback:${live_sha:0:12} (was v9.9.9)" ]
check "rollback rewrites the version marker to name the restored worker" $?
teardown

# 3b. A second rollback keeps the original release instead of nesting labels.
setup
printf 'rollback:0123456789ab (was v9.9.9)\n' >"$SUBROUTER_VERSION_FILE"
healthy
bash "$GUARD" >/dev/null 2>&1
printf 'broken\n' >"$ROOT/bin/subrouter"
unhealthy
bash "$GUARD" >/dev/null 2>&1
bash "$GUARD" >/dev/null 2>&1
live_sha="$(printf 'live\n' | shasum -a 256 | awk '{print $1}')"
[ "$(cat "$SUBROUTER_VERSION_FILE")" = "rollback:${live_sha:0:12} (was v9.9.9)" ]
check "repeated rollback does not nest version labels" $?
teardown

# 4. Two failures on the last-good binary restart without touching the binary.
setup
healthy
bash "$GUARD" >/dev/null 2>&1
unhealthy
bash "$GUARD" >/dev/null 2>&1
bash "$GUARD" >/dev/null 2>&1
[ "$(cat "$ROOT/bin/subrouter")" = "live" ] && ! ls "$ROOT"/bin/subrouter.rejected-* >/dev/null 2>&1
check "no rollback when the live worker is already last-good" $?
grep -q "^bootout system/" "$LAUNCHCTL_CALLS"
check "unhealthy last-good worker still gets a restart" $?
[ ! -f "$SUBROUTER_UPGRADE_INHIBIT_FILE" ]
check "a plain restart leaves worker autoupdate enabled" $?
[ "$(cat "$SUBROUTER_VERSION_FILE")" = "v9.9.9" ]
check "a plain restart leaves the version marker alone" $?
teardown

# 5. A fresh maintenance sentinel suppresses recovery entirely.
setup
healthy
bash "$GUARD" >/dev/null 2>&1
printf 'broken\n' >"$ROOT/bin/subrouter"
unhealthy
: >"$ROOT/state/maintenance"
bash "$GUARD" >/dev/null 2>&1
bash "$GUARD" >/dev/null 2>&1
# A read-only `print` is not a recovery action; assert on the mutating verbs.
[ "$(cat "$ROOT/bin/subrouter")" = "broken" ] && ! grep -qE "^(bootout|bootstrap|kickstart)" "$LAUNCHCTL_CALLS"
check "fresh maintenance sentinel blocks rollback and restart" $?
teardown

# 6. Recovery that restores health clears the strike counter.
setup
healthy
bash "$GUARD" >/dev/null 2>&1
printf 'broken\n' >"$ROOT/bin/subrouter"
unhealthy
bash "$GUARD" >/dev/null 2>&1
export SUBROUTER_GUARD_HEALTH_WAIT_SECS=10
( sleep 1; healthy ) &
bash "$GUARD" >/dev/null 2>&1
wait
[ ! -f "$ROOT/state/guard.strikes" ]
check "successful recovery clears the strike counter" $?
teardown

# 6b. A sentinel must not muzzle recovery once the service is gone from the
# launchd domain. An interrupted bootout leaves exactly that state, and it kept
# the router down on 2026-09-04.
setup
healthy
bash "$GUARD" >/dev/null 2>&1
unhealthy
: >"$ROOT/state/maintenance"
export LAUNCHCTL_PRINT_EXIT=1   # the service is not in the launchd domain
bash "$GUARD" >/dev/null 2>&1
bash "$GUARD" >/dev/null 2>&1
[ ! -s "$LAUNCHCTL_CALLS" ] || ! grep -q "^bootstrap system " "$LAUNCHCTL_CALLS"
check "a fresh sentinel still holds recovery for the grace window" $?
touch -t "$(date -v-10M +%Y%m%d%H%M 2>/dev/null || date -d '10 minutes ago' +%Y%m%d%H%M)" "$ROOT/state/maintenance"
bash "$GUARD" >/dev/null 2>&1
grep -q "^bootstrap system " "$LAUNCHCTL_CALLS"
check "after the grace window a missing service is bootstrapped anyway" $?
unset LAUNCHCTL_PRINT_EXIT
teardown

# 7. A running deploy owns the outcome: no promotion, no restart, no rollback.
setup
healthy
mkdir -p "$SUBROUTER_DEPLOY_LOCK_DIR"
printf 'candidate\n' >"$ROOT/bin/subrouter"
bash "$GUARD" >/dev/null 2>&1
[ ! -f "$SUBROUTER_LAST_GOOD" ]
check "no last-good promotion while a deploy holds the lock" $?
unhealthy
bash "$GUARD" >/dev/null 2>&1
bash "$GUARD" >/dev/null 2>&1
[ "$(cat "$ROOT/bin/subrouter")" = "candidate" ] && ! grep -qE "^(bootout|bootstrap|kickstart)" "$LAUNCHCTL_CALLS"
check "no rollback or restart while a deploy holds the lock" $?
teardown

# 8. A stale deploy lock must not disable the guard for ever.
setup
healthy
bash "$GUARD" >/dev/null 2>&1
mkdir -p "$SUBROUTER_DEPLOY_LOCK_DIR"
# Backdate the lock past the grace window.
touch -t "$(date -v-30M +%Y%m%d%H%M 2>/dev/null || date -d '30 minutes ago' +%Y%m%d%H%M)" "$SUBROUTER_DEPLOY_LOCK_DIR"
printf 'broken\n' >"$ROOT/bin/subrouter"
unhealthy
bash "$GUARD" >/dev/null 2>&1
bash "$GUARD" >/dev/null 2>&1
[ "$(cat "$ROOT/bin/subrouter")" = "live" ]
check "a stale deploy lock does not block recovery" $?
teardown

# 9. A second concurrent guard run does nothing.
setup
healthy
mkdir -p "$SUBROUTER_GUARD_LOCK_DIR"
bash "$GUARD" >/dev/null 2>&1
[ ! -f "$SUBROUTER_LAST_GOOD" ]
check "a concurrent guard run stands down" $?
teardown

# 10. A guard tick during an autoupdate must not promote the untested
# candidate. subrouter-autoupdate.sh swaps the binary and then asks the
# supervisor for a new generation; the old generation still answers health in
# between. The fake supervisor below runs a guard tick inside that window.
AUTOUPDATE="$HERE/../subrouter-autoupdate.sh"
start_guard_ticking_supervisor() {
  python3 - "$ROOT/control.sock" "$GUARD" "$ROOT/guard-during-upgrade.log" <<'PY' &
import http.server, json, os, socket, socketserver, subprocess, sys

path, guard, log = sys.argv[1:4]

class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        tick = subprocess.run(["bash", guard], capture_output=True, text=True)
        with open(log, "a") as stream:
            stream.write(tick.stdout + tick.stderr)
        body = json.dumps({"active": {"id": "gen-2"}}).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    address_family = socket.AF_UNIX
    def server_bind(self):
        try:
            os.remove(path)
        except FileNotFoundError:
            pass
        socketserver.TCPServer.server_bind(self)

Server(path, Handler).serve_forever()
PY
  FAKE_PID=$!
  for _ in $(seq 1 50); do [ -S "$ROOT/control.sock" ] && break; sleep 0.1; done
}

setup
healthy
bash "$GUARD" >/dev/null 2>&1          # records last-good = "live"
mkdir -p "$ROOT/releases/v10.0.0" "$ROOT/deploy-state"
arch=amd64; case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; esac
asset="subrouter_10.0.0_darwin_${arch}"
printf '#!/bin/sh\n# release v10.0.0\nexit 0\n' >"$ROOT/releases/v10.0.0/$asset"
(cd "$ROOT/releases/v10.0.0" && shasum -a 256 "$asset" >SHA256SUMS)
printf '{"tag_name":"v10.0.0"}\n' >"$ROOT/latest.json"
start_guard_ticking_supervisor
SUBROUTER_RELEASE_API_URL="file://$ROOT/latest.json" \
SUBROUTER_RELEASE_DOWNLOAD_URL="file://$ROOT/releases" \
SUBROUTER_CONTROL_SOCKET="$ROOT/control.sock" \
SUBROUTER_DEPLOY_STATE="$ROOT/deploy-state" \
SUBROUTER_MUTATION_LOCK_FILE="$ROOT/mutation.lock" \
  bash "$AUTOUPDATE" >"$ROOT/autoupdate.log" 2>&1
rc=$?
kill "$FAKE_PID" 2>/dev/null; wait "$FAKE_PID" 2>/dev/null
[ "$rc" -eq 0 ] && cmp -s "$ROOT/bin/subrouter" "$ROOT/releases/v10.0.0/$asset" \
  && [ "$(cat "$SUBROUTER_VERSION_FILE")" = "v10.0.0" ]
check "autoupdate installs the release while the guard ticks mid-swap" $?
grep -q "subrouter-autoupdate.sh pid .* holds .*deploy.lock; standing down" "$ROOT/guard-during-upgrade.log"
check "a guard tick during an autoupdate stands down on the shared deploy lock" $?
[ "$(cat "$SUBROUTER_LAST_GOOD")" = "live" ]
check "a guard tick during an autoupdate does not promote the candidate" $?
[ ! -d "$SUBROUTER_DEPLOY_LOCK_DIR" ]
check "autoupdate releases the deploy lock when it finishes" $?
bash "$GUARD" >/dev/null 2>&1
cmp -s "$SUBROUTER_LAST_GOOD" "$ROOT/releases/v10.0.0/$asset"
check "the next guard tick promotes the release once the update is done" $?
teardown

# 11. While the guard holds the deploy lock, autoupdate defers without
# touching the binary.
setup
healthy
mkdir -p "$SUBROUTER_DEPLOY_LOCK_DIR"
printf 'subrouter-guard.sh pid 1\n' >"$SUBROUTER_DEPLOY_LOCK_DIR/owner"
mkdir -p "$ROOT/releases/v10.0.0"
arch=amd64; case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; esac
asset="subrouter_10.0.0_darwin_${arch}"
printf '#!/bin/sh\n# release v10.0.0\nexit 0\n' >"$ROOT/releases/v10.0.0/$asset"
(cd "$ROOT/releases/v10.0.0" && shasum -a 256 "$asset" >SHA256SUMS)
printf '{"tag_name":"v10.0.0"}\n' >"$ROOT/latest.json"
SUBROUTER_RELEASE_API_URL="file://$ROOT/latest.json" \
SUBROUTER_RELEASE_DOWNLOAD_URL="file://$ROOT/releases" \
SUBROUTER_CONTROL_SOCKET="$ROOT/control.sock" \
SUBROUTER_DEPLOY_STATE="$ROOT/deploy-state" \
SUBROUTER_MUTATION_LOCK_FILE="$ROOT/mutation.lock" \
  bash "$AUTOUPDATE" >"$ROOT/autoupdate.log" 2>&1
rc=$?
[ "$rc" -eq 0 ] && [ "$(cat "$ROOT/bin/subrouter")" = "live" ] \
  && [ ! -e "$ROOT/deploy-state/backups" ] \
  && [ -f "$SUBROUTER_DEPLOY_LOCK_DIR/owner" ] \
  && grep -q "subrouter-guard.sh pid 1 holds .*; worker update deferred" "$ROOT/autoupdate.log"
check "autoupdate defers while the guard holds the deploy lock" $?
teardown

if [ "$failures" -ne 0 ]; then
  printf '%d check(s) failed\n' "$failures"
  exit 1
fi
printf 'all checks passed\n'
