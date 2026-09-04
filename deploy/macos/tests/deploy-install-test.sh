#!/usr/bin/env bash
# Exercises subrouter-deploy.sh against a fake supervisor control socket and a
# file-backed health probe. Nothing here touches a real service.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY="$HERE/../subrouter-deploy.sh"
failures=0

check() {
  if [ "$2" -eq 0 ]; then printf 'ok   %s\n' "$1"
  else printf 'FAIL %s\n' "$1"; failures=$((failures + 1)); fi
}

start_fake_supervisor() { # start_fake_supervisor <upgrade-exit-code>
  UPGRADE_MODE="$1"
  python3 - "$ROOT/control.sock" "$UPGRADE_MODE" "$ROOT/health" "$ROOT/upgrade.calls" "$SUBROUTER_LAST_GOOD" "$ROOT/bin/subrouter" <<'PY' &
import http.server, json, os, socket, socketserver, sys, threading

path, mode, health, calls, last_good, live = sys.argv[1:7]

class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        with open(calls, "a") as stream:
            stream.write(self.path + "\n")
        if mode == "clobber_fail":
            # Stand in for a subrouter-guard tick that promotes whatever binary
            # is on disk while the old generation still answers health.
            os.makedirs(os.path.dirname(last_good), exist_ok=True)
            with open(live, "rb") as source, open(last_good, "wb") as target:
                target.write(source.read())
        if mode in ("fail", "clobber_fail"):
            self.send_response(500)
            self.end_headers()
            self.wfile.write(b"not ready")
            return
        if mode == "unhealthy":
            # The generation switches, but the public port stops answering.
            try:
                os.remove(health)
            except FileNotFoundError:
                pass
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

setup() { # setup <upgrade-mode>
  ROOT="$(mktemp -d)"
  mkdir -p "$ROOT/bin" "$ROOT/state" "$ROOT/etc"
  printf '#!/bin/sh\nexit 0\n' >"$ROOT/bin/subrouter"; chmod 0755 "$ROOT/bin/subrouter"
  printf '#!/bin/sh\n# candidate\nexit 0\n' >"$ROOT/candidate"; chmod 0755 "$ROOT/candidate"
  printf 'ok\n' >"$ROOT/health"
  : >"$ROOT/upgrade.calls"
  export SUBROUTER_BIN="$ROOT/bin/subrouter"
  export SUBROUTER_DEPLOY_STATE="$ROOT/state"
  export SUBROUTER_LAST_GOOD="$ROOT/state/subrouter.last-good"
  export SUBROUTER_VERSION_FILE="$ROOT/etc/subrouter-version"
  export SUBROUTER_HEALTH_URL="file://$ROOT/health"
  export SUBROUTER_CONTROL_SOCKET="$ROOT/control.sock"
  export SUBROUTER_UPGRADE_INHIBIT_FILE="$ROOT/transaction/upgrade-inhibited"
  export SUBROUTER_DEPLOY_LOCK_DIR="$ROOT/state/deploy.lock"
  export SUBROUTER_DEPLOY_HEALTH_TIMEOUT_SECS=3
  start_fake_supervisor "$1"
}

teardown() { kill "$FAKE_PID" 2>/dev/null; wait "$FAKE_PID" 2>/dev/null; rm -rf "$ROOT"; }

# A launchd stand-in for the restart path: it records every verb, refuses the
# first bootstraps the way a draining job does, and only then reports the
# service in the domain.
install_restart_launchctl() {
  export SUBROUTER_SUPERVISOR_BIN="$ROOT/bin/subrouter-supervisor"
  export SUBROUTER_MAINTENANCE_FILE="$ROOT/state/maintenance"
  export SUBROUTER_LAUNCHCTL="$ROOT/bin/launchctl"
  export LAUNCHCTL_CALLS="$ROOT/calls"
  export HEALTH_FILE="$ROOT/health"
  export IN_DOMAIN_FILE="$ROOT/in-domain"
  : >"$LAUNCHCTL_CALLS"
  rm -f "$HEALTH_FILE" "$IN_DOMAIN_FILE"
  cat >"$ROOT/bin/launchctl" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$LAUNCHCTL_CALLS"
if [ "${1:-}" = "bootstrap" ]; then
  attempts="$(grep -c '^bootstrap ' "$LAUNCHCTL_CALLS")"
  if [ "${attempts}" -ge "${BOOTSTRAP_SUCCEEDS_ON:-1}" ]; then
    printf 'ok\n' >"$HEALTH_FILE"
    printf 'in-domain\n' >"$IN_DOMAIN_FILE"
  fi
  exit 0
fi
[ "${1:-}" = "print" ] && { [ -f "$IN_DOMAIN_FILE" ] || exit 1; exit 0; }
exit 0
FAKE
  chmod 0755 "$ROOT/bin/launchctl"
}

# 1. A candidate that becomes ready is installed and recorded.
setup ok
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 >/dev/null 2>&1
rc=$?
[ "$rc" -eq 0 ] && cmp -s "$ROOT/bin/subrouter" "$ROOT/candidate"
check "a ready candidate is installed" $?
[ "$(cat "$SUBROUTER_VERSION_FILE")" = "v9.9.9" ]
check "the version label is recorded" $?
[ -f "$SUBROUTER_LAST_GOOD" ]
check "the previous worker is saved as last-good" $?
[ ! -e "$SUBROUTER_UPGRADE_INHIBIT_FILE" ]
check "a clean deploy leaves no autoupdate pin behind" $?
teardown

# 2. A candidate that never becomes ready is rolled back inside the script.
setup fail
before="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
bash "$DEPLOY" install "$ROOT/candidate" >/dev/null 2>&1
rc=$?
after="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
[ "$rc" -ne 0 ] && [ "$before" = "$after" ]
check "a candidate that fails readiness is reverted and reported" $?
[ ! -s "$SUBROUTER_VERSION_FILE" ] 2>/dev/null || [ ! -f "$SUBROUTER_VERSION_FILE" ]
check "a failed install does not record a version" $?
teardown

# 3. A candidate that switches but kills public health is rolled back too.
setup unhealthy
before="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
bash "$DEPLOY" install "$ROOT/candidate" >/dev/null 2>&1
rc=$?
after="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
[ "$rc" -ne 0 ] && [ "$before" = "$after" ]
check "a candidate that breaks public health is reverted" $?
[ "$(wc -l <"$ROOT/upgrade.calls")" -ge 2 ]
check "the restored binary is switched back in through the control socket" $?
teardown

# 4. An operator pin survives a deploy.
setup ok
mkdir -p "$(dirname "$SUBROUTER_UPGRADE_INHIBIT_FILE")"
printf 'pinned by hand\n' >"$SUBROUTER_UPGRADE_INHIBIT_FILE"
bash "$DEPLOY" install "$ROOT/candidate" >/dev/null 2>&1
grep -q "pinned by hand" "$SUBROUTER_UPGRADE_INHIBIT_FILE" 2>/dev/null
check "an existing autoupdate pin is restored after a deploy" $?
teardown

# 5. An install is refused while public health is already down.
setup ok
rm -f "$ROOT/health"
before="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
bash "$DEPLOY" install "$ROOT/candidate" >/dev/null 2>&1
rc=$?
after="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
[ "$rc" -ne 0 ] && [ "$before" = "$after" ]
check "install is refused during an outage" $?
teardown

# 6. rollback puts the recorded last-good binary back.
setup ok
mkdir -p "$ROOT/state"
printf '#!/bin/sh\nexit 0\n# good\n' >"$SUBROUTER_LAST_GOOD"; chmod 0755 "$SUBROUTER_LAST_GOOD"
bash "$DEPLOY" rollback >/dev/null 2>&1
rc=$?
[ "$rc" -eq 0 ] && cmp -s "$ROOT/bin/subrouter" "$SUBROUTER_LAST_GOOD"
check "rollback installs the recorded last-good worker" $?
teardown

# 7. Regression: the rollback source must be private to this deploy. Sharing
# $LAST_GOOD with subrouter-guard.sh let a guard tick record the untested
# candidate mid-install, so the "rollback" restored the candidate over itself.
setup clobber_fail
before="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
bash "$DEPLOY" install "$ROOT/candidate" >/dev/null 2>&1
after="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
[ "$before" = "$after" ]
check "rollback survives last-good being overwritten mid-install" $?
teardown

# 8. The restart sequence must finish even when the caller that started it is
# killed. An interrupted `bootout` left the service out of the launchd domain
# with the port closed on 2026-09-04.
setup ok
export SUBROUTER_SUPERVISOR_BIN="$ROOT/bin/subrouter-supervisor"
export SUBROUTER_MAINTENANCE_FILE="$ROOT/state/maintenance"
export SUBROUTER_LAUNCHCTL="$ROOT/bin/launchctl"
cat >"$ROOT/bin/launchctl" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$LAUNCHCTL_CALLS"
# The bootstrap step is what must still happen after the caller is gone.
[ "${1:-}" = "bootstrap" ] && printf 'ok\n' >"$HEALTH_FILE"
exit 0
FAKE
chmod 0755 "$ROOT/bin/launchctl"
export LAUNCHCTL_CALLS="$ROOT/calls" HEALTH_FILE="$ROOT/health"
: >"$LAUNCHCTL_CALLS"
rm -f "$ROOT/health"
# Kill the caller one second in: the detached sequence must still bootstrap.
bash "$DEPLOY" restart-daemon >/dev/null 2>&1 &
caller=$!
sleep 1
kill -KILL "$caller" 2>/dev/null
for _ in $(seq 1 20); do grep -q "^bootstrap system " "$LAUNCHCTL_CALLS" && break; sleep 1; done
grep -q "^bootout system/" "$LAUNCHCTL_CALLS" && grep -q "^bootstrap system " "$LAUNCHCTL_CALLS"
check "a killed caller still leaves the service bootstrapped" $?
teardown

# 8. The restart sequence must finish even when the caller that started it is
# killed. An interrupted `bootout` left the service out of the launchd domain
# with the port closed on 2026-09-04.
setup ok
install_restart_launchctl
bash "$DEPLOY" restart-daemon >/dev/null 2>&1 &
caller=$!
sleep 1
kill -KILL "$caller" 2>/dev/null
for _ in $(seq 1 20); do grep -q "^bootstrap system " "$LAUNCHCTL_CALLS" && break; sleep 1; done
grep -q "^bootout system/" "$LAUNCHCTL_CALLS" && grep -q "^bootstrap system " "$LAUNCHCTL_CALLS"
check "a killed caller still leaves the service bootstrapped" $?
for _ in $(seq 1 10); do [ -e "$SUBROUTER_MAINTENANCE_FILE" ] || break; sleep 1; done
[ ! -e "$SUBROUTER_MAINTENANCE_FILE" ]
check "a killed restart does not leave the watchdog muzzled" $?
teardown

# 9. launchd refuses a bootstrap while the old job drains. Doing it once is
# what left the service out of the domain with the port closed.
setup ok
install_restart_launchctl
export BOOTSTRAP_SUCCEEDS_ON=3
bash "$DEPLOY" restart-daemon >/dev/null 2>&1
for _ in $(seq 1 30); do [ -f "$HEALTH_FILE" ] && break; sleep 1; done
[ "$(grep -c '^bootstrap ' "$LAUNCHCTL_CALLS")" -ge 3 ] && [ -f "$HEALTH_FILE" ]
check "bootstrap is retried until the job is in the launchd domain" $?
unset BOOTSTRAP_SUCCEEDS_ON
teardown

if [ "$failures" -ne 0 ]; then printf '%d check(s) failed\n' "$failures"; exit 1; fi
printf 'all checks passed\n'
