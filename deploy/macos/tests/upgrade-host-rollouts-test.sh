#!/usr/bin/env bash
# Runs the host side of `upgrade-host.sh --enable-rollouts` against a fake host
# tree: a local git remote holding this checkout's deploy/macos, a prebuilt
# stage, a fake sudo, a fake supervisor control socket, and a stand-in for the
# pf handoff. Nothing here touches a real service, sudo, pf or launchd.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPGRADE_HOST="$HERE/../upgrade-host.sh"
failures=0
FAKE_PID=""

check() {
  if [ "$2" -eq 0 ]; then printf 'ok   %s\n' "$1"
  else printf 'FAIL %s\n' "$1"; failures=$((failures + 1)); fi
}

ROOT="$(mktemp -d)"
cleanup() {
  [ -z "$FAKE_PID" ] || { kill "$FAKE_PID" 2>/dev/null; wait "$FAKE_PID" 2>/dev/null; }
  rm -rf "$ROOT"
}
trap cleanup EXIT

mkdir -p "$ROOT/path" "$ROOT/home" "$ROOT/scripts" "$ROOT/libexec" "$ROOT/state" "$ROOT/service-home" "$ROOT/etc" "$ROOT/LaunchDaemons"

# --- fake commands ------------------------------------------------------------
cat >"$ROOT/path/sudo" <<'FAKE'
#!/usr/bin/env bash
while [ $# -gt 0 ]; do
  case "$1" in -n|-H) shift ;; -u) shift 2 ;; *) break ;; esac
done
[ $# -gt 0 ] || exit 0
exec "$@"
FAKE
cat >"$ROOT/path/sysctl" <<'FAKE'
#!/usr/bin/env bash
case "$*" in *loadavg*) echo '{ 0.10 0.20 0.30 }' ;; *ncpu*) echo 8 ;; esac
FAKE
printf '#!/bin/sh\nexit 0\n' >"$ROOT/path/go"
chmod 0755 "$ROOT/path/"*
export PATH="$ROOT/path:$PATH"

# --- the host tree ------------------------------------------------------------
export HOME="$ROOT/home"
export SUBROUTER_PLIST="$ROOT/LaunchDaemons/ai.manaflow.subrouter-team.plist"
export SUBROUTER_BIN="$ROOT/scripts/subrouter"
export SUBROUTER_SUPERVISOR_BIN="$ROOT/libexec/subrouter-supervisor"
export SUBROUTER_SCRIPTS_DIR="$ROOT/scripts"
export SUBROUTER_DEPLOY_STATE="$ROOT/state"
export SUBROUTER_VERIFY_STATE="$ROOT/state"
export SUBROUTER_SERVICE_HOME="$ROOT/service-home"
export SUBROUTER_WORKER_CONFIG="$ROOT/service-home/worker-config.json"
export SUBROUTER_VERSION_FILE="$ROOT/etc/subrouter-version"
export SUBROUTER_HEALTH_URL="file://$ROOT/health"
export SUBROUTER_CONTROL_SOCKET="$ROOT/control.sock"
export SUBROUTER_MAINTENANCE_FILE="$ROOT/state/maintenance"
export SUBROUTER_HANDOFF_SCRIPT="$ROOT/fake-handoff.sh"
export SUBROUTER_HANDOFF_LOG="$ROOT/handoff.log"
export SUBROUTER_UPGRADE_LOG="$ROOT/upgrade.log"
export SUBROUTER_REPO_URL="$ROOT/remote.git"
export SUBROUTER_UPGRADE_WATCH_SECS=1
unset SUBROUTER_LAST_GOOD SUBROUTER_RELEASE_STATE SUBROUTER_UPGRADE_INHIBIT_FILE SUBROUTER_DEPLOY_LOCK_DIR SUBROUTER_DEPLOY_CANARY

printf '{"ok": true}\n' >"$ROOT/health"
printf 'main-000000000000\n' >"$SUBROUTER_VERSION_FILE"
printf 'account data\n' >"$ROOT/service-home/accounts.json"
printf '#!/bin/sh\n# live worker\nexit 0\n' >"$SUBROUTER_BIN"
printf '#!/bin/sh\n# old supervisor\nexit 0\n' >"$SUBROUTER_SUPERVISOR_BIN"
printf '#!/bin/sh\n# an older subrouter-deploy.sh\nexit 0\n' >"$ROOT/scripts/subrouter-deploy.sh"
printf '#!/bin/sh\n# an older subrouter-guard.sh\nexit 0\n' >"$ROOT/scripts/subrouter-guard.sh"
chmod 0755 "$SUBROUTER_BIN" "$SUBROUTER_SUPERVISOR_BIN" "$ROOT/scripts/"*.sh
cp -p "$ROOT/scripts/subrouter-deploy.sh" "$ROOT/old-deploy.sh"
python3 - "$SUBROUTER_PLIST" "$SUBROUTER_SUPERVISOR_BIN" "$SUBROUTER_CONTROL_SOCKET" "$SUBROUTER_BIN" <<'PY'
import plistlib, sys
path, supervisor, control, worker = sys.argv[1:5]
with open(path, "wb") as stream:
    plistlib.dump({"Label": "ai.manaflow.subrouter-team", "ProgramArguments": [
        supervisor, "supervise", "--addr", ":31415", "--control-socket", control,
        "--worker-bin", worker, "--", "--team", "--tailscale"]}, stream)
PY

# The pf handoff itself needs root, pf and launchd. The stand-in does what a
# successful one leaves behind: the candidate supervisor and plist in place,
# serving, with /_subrouter/canary available.
cat >"$SUBROUTER_HANDOFF_SCRIPT" <<FAKE
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"$ROOT/handoff.calls"
install -m 0755 "\$1" "$SUBROUTER_SUPERVISOR_BIN"
cp "\$2" "$SUBROUTER_PLIST"
: >"$ROOT/supervisor-has-canary"
rm -f "$SUBROUTER_MAINTENANCE_FILE"
echo "HANDOFF OK"
FAKE
chmod 0755 "$SUBROUTER_HANDOFF_SCRIPT"

python3 - "$SUBROUTER_CONTROL_SOCKET" "$ROOT/supervisor-has-canary" "$ROOT/calls" <<'PY' &
import http.server, json, os, socket, socketserver, sys
path, has_canary, calls = sys.argv[1:4]
state = {"state": "idle"}
class Handler(http.server.BaseHTTPRequestHandler):
    def reply(self, code, body):
        data = (json.dumps(body) if isinstance(body, dict) else body).encode()
        self.send_response(code)
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
    def do_GET(self):
        open(calls, "a").write("GET " + self.path + "\n")
        if not os.path.exists(has_canary):
            return self.reply(404, "404 page not found\n")
        body = {"state": state["state"], "weight": 5 if state["state"] == "canary" else 0, "incumbent": {"id": "gen-1"}}
        if state["state"] == "canary":
            body.update({"candidate": {"id": "gen-2"}, "release": "main-x canary 5% (<1m)", "steps": [5, 25, 100], "step": 1})
        self.reply(200, body)
    def do_POST(self):
        open(calls, "a").write("POST " + self.path + "\n")
        if self.path.startswith("/_subrouter/canary/start"):
            if not os.path.exists(has_canary):
                return self.reply(404, "404 page not found\n")
            state["state"] = "canary"
            return self.reply(200, {"state": "canary", "candidate": {"id": "gen-2"}, "release": "main-x canary 5% (<1m)"})
        self.reply(200, {"active": {"id": "gen-2"}})
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
for _ in $(seq 1 50); do [ -S "$SUBROUTER_CONTROL_SOCKET" ] && break; sleep 0.1; done
: >"$ROOT/calls"

# A remote holding this checkout's deploy/macos on main.
git init --quiet --bare "$ROOT/remote.git"
git init --quiet "$ROOT/src"
mkdir -p "$ROOT/src/deploy"
cp -R "$HERE/.." "$ROOT/src/deploy/macos"
rm -rf "$ROOT/src/deploy/macos/tests"
git -C "$ROOT/src" add -A
git -C "$ROOT/src" -c user.name=test -c user.email=test@example.invalid commit --quiet -m "deploy scripts"
git -C "$ROOT/src" push --quiet "$ROOT/remote.git" HEAD:refs/heads/main
SHA="$(git -C "$ROOT/src" rev-parse HEAD)"

# The build is prebuilt: a staged candidate that passes the preflight.
stage="$ROOT/state/upgrade/$SHA"
mkdir -p "$stage"
cat >"$stage/subrouter" <<'FAKE'
#!/bin/sh
# candidate built from main
[ "${1:-}" = "codex" ] && { echo '{"ok":true,"accounts_needing_migration":0}'; exit 0; }
exit 0
FAKE
chmod 0755 "$stage/subrouter"
printf 'main-%s\n' "${SHA:0:12}" >"$stage/label"

run_host() { bash "$UPGRADE_HOST" --on-host --ref main --wait-mins 1 "$@" >"$ROOT/run.out" 2>&1; }
installed_same() { # every rollout script at the scripts dir matches the repo
  local f
  for f in subrouter-deploy.sh subrouter-guard.sh subrouter-verify.sh subrouter-autoupdate.sh \
           release-bake-lib.sh mutation-lease-lib.sh subrouter-supervisor-handoff.sh; do
    cmp -s "$HERE/../$f" "$ROOT/scripts/$f" || return 1
  done
}

# 1. --plan changes nothing and says what it would do.
run_host --enable-rollouts --plan
rc=$?
[ "$rc" -eq 0 ] && cmp -s "$ROOT/scripts/subrouter-deploy.sh" "$ROOT/old-deploy.sh" && [ ! -e "$ROOT/handoff.calls" ] \
  && grep -q 'b. supervisor .*handoff-supervisor --adopt-worker-config' "$ROOT/run.out" \
  && grep -q 'plan only: nothing was changed' "$ROOT/run.out"
check "--enable-rollouts --plan reports the steps and changes nothing" $?

# 2. The adoption.
run_host --enable-rollouts
rc=$?
[ "$rc" -eq 0 ] && grep -q 'OK: rollouts enabled' "$ROOT/run.out"
check "--enable-rollouts succeeds on the fake host" $?
installed_same
check "a. main's deploy, guard, verify, autoupdate, bake, lease and handoff scripts are installed" $?
backup_dir="$(ls -d "$ROOT/state/upgrade-backups/"*Z 2>/dev/null | tail -n 1)"
cmp -s "$backup_dir/scripts/subrouter-deploy.sh" "$ROOT/old-deploy.sh" && [ -f "$backup_dir/subrouter-supervisor" ] && [ -f "$backup_dir/state.tgz" ]
check "a. the replaced scripts, supervisor and state are backed up" $?
cmp -s "$SUBROUTER_SUPERVISOR_BIN" "$stage/subrouter" && grep -q -- "$stage/subrouter" "$ROOT/handoff.calls"
check "b. the supervisor is handed off to the main build" $?
python3 -c '
import plistlib, sys
args = plistlib.load(open(sys.argv[1], "rb"))["ProgramArguments"]
head = args[:args.index("--")]
sys.exit(0 if head[head.index("--worker-config") + 1] == sys.argv[2] else 1)
' "$SUBROUTER_PLIST" "$SUBROUTER_WORKER_CONFIG"
check "b. the handoff wires --worker-config into the plist" $?
python3 -c '
import json, sys
doc = json.load(open(sys.argv[1]))
sys.exit(0 if doc["env"]["SUBROUTER_RELEASE_STATE"] == sys.argv[2] and doc["args"] == ["--team", "--tailscale"] else 1)
' "$SUBROUTER_WORKER_CONFIG" "$ROOT/state/release-state.json"
check "c. the worker config keeps the worker args and gains SUBROUTER_RELEASE_STATE" $?
[ "$(grep -c '^POST /_subrouter/upgrade$' "$ROOT/calls")" -eq 1 ]
check "c. the env change is one hot upgrade (reconfigure), no restart" $?
grep -q 'pinned by upgrade-host.sh --enable-rollouts' "$SUBROUTER_PLIST.supervisor-transaction/upgrade-inhibited" 2>/dev/null
check "d. autoupdate is pinned off" $?
pin_line="$(grep -n 'd. pinned autoupdate' "$ROOT/run.out" | cut -d: -f1)"
scripts_line="$(grep -n 'a. installed' "$ROOT/run.out" | cut -d: -f1)"
[ -n "$pin_line" ] && [ -n "$scripts_line" ] && [ "$pin_line" -lt "$scripts_line" ]
check "d. the pin goes in before the new scripts, so the new autoupdate cannot start a canary" $?
grep -q "turn it on with: sudo $ROOT/scripts/subrouter-deploy.sh unpin" "$ROOT/run.out"
check "d. the command that enables autoupdate later is printed" $?
cmp -s "$SUBROUTER_BIN" "$ROOT/state/upgrade-backups/$(basename "$backup_dir")/subrouter"
check "the worker binary is not touched" $?

# 3. Running it again is a no-op.
: >"$ROOT/calls"
run_host --enable-rollouts
rc=$?
[ "$rc" -eq 0 ] && grep -q 'a. scripts to install: none, all current' "$ROOT/run.out" \
  && grep -q 'b. .*already this build with --worker-config, skipped' "$ROOT/run.out" \
  && grep -q 'c. .*already set, skipped' "$ROOT/run.out" && ! grep -q '^POST' "$ROOT/calls" \
  && [ "$(wc -l <"$ROOT/handoff.calls")" -eq 1 ]
check "a second --enable-rollouts changes nothing" $?

# 3b. Step c waits for no canary to be pending: its reconfigure is a plain
# upgrade that would supersede the canary with the candidate binary.
cp -p "$SUBROUTER_WORKER_CONFIG" "$ROOT/worker-config.adopted"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); d["env"].pop("SUBROUTER_RELEASE_STATE"); json.dump(d, open(sys.argv[1],"w"))' "$SUBROUTER_WORKER_CONFIG"
printf '{"label":"main-x","resolved":""}\n' >"$ROOT/state/canary-rollout.json"
: >"$ROOT/calls"
run_host --enable-rollouts
rc=$?
[ "$rc" -ne 0 ] && grep -q 'a canary rollout is pending' "$ROOT/run.out" && ! grep -q '^POST /_subrouter/upgrade' "$ROOT/calls" \
  && ! grep -q SUBROUTER_RELEASE_STATE "$SUBROUTER_WORKER_CONFIG"
check "c. the reconfigure is refused while a canary rollout is pending" $?
rm -f "$ROOT/state/canary-rollout.json"
cp -p "$ROOT/worker-config.adopted" "$SUBROUTER_WORKER_CONFIG"
[ ! -d "$ROOT/state/deploy.lock" ]
check "a refused run leaves no deploy.lock behind" $?

# 3c. A failure right after deploy.lock is taken (here: writing its owner
# file) must not leak the lock, or the guard and every deploy stand down.
printf '# locally edited\n' >>"$ROOT/scripts/subrouter-verify.sh"
mkdir -p "$ROOT/failing-tee"
real_tee="$(command -v tee)"
cat >"$ROOT/failing-tee/tee" <<FAKE
#!/usr/bin/env bash
case "\$*" in *deploy.lock/owner*) exit 1 ;; esac
exec "$real_tee" "\$@"
FAKE
chmod 0755 "$ROOT/failing-tee/tee"
PATH="$ROOT/failing-tee:$PATH" run_host --enable-rollouts
rc=$?
[ "$rc" -ne 0 ] && [ ! -d "$ROOT/state/deploy.lock" ]
check "a. an error while holding deploy.lock releases it" $?
cp -p "$HERE/../subrouter-verify.sh" "$ROOT/scripts/subrouter-verify.sh"

# 4. The plain path still works after adoption, and now goes out as a canary.
: >"$ROOT/calls"
run_host
rc=$?
[ "$rc" -eq 0 ] && cmp -s "$SUBROUTER_BIN" "$stage/subrouter" \
  && grep -q '^POST /_subrouter/canary/start?steps=default$' "$ROOT/calls" && grep -q "OK: main-000000000000 -> main-${SHA:0:12}" "$ROOT/run.out"
check "the plain upgrade-host path installs through the canary after adoption" $?

if [ "$failures" -ne 0 ]; then
  printf '%d check(s) failed\n' "$failures"
  sed -n '1,200p' "$ROOT/run.out"
  exit 1
fi
printf 'all checks passed\n'
