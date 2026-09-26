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
  export SUBROUTER_WORKER_CONFIG="$ROOT/state/worker-config.json"
  export SUBROUTER_PLIST="$ROOT/team.plist"
  python3 - "$SUBROUTER_PLIST" "$SUBROUTER_WORKER_CONFIG" <<'PY'
import plistlib, sys
with open(sys.argv[1], "wb") as stream:
    plistlib.dump({"ProgramArguments": ["/usr/local/libexec/subrouter-supervisor", "supervise",
        "--worker-config", sys.argv[2], "--", "--flag"]}, stream)
PY
  make_repo
  start_fake_supervisor "$1"
}

# make_repo builds a local stand-in for the GitHub repository: REV_LIVE, a
# REV_CHILD that contains it, and REV_STALE on a branch cut before it.
make_repo() {
  local repo="$ROOT/repo"
  git init --quiet "$repo"
  git -C "$repo" -c user.name=t -c user.email=t@t commit --quiet --allow-empty -m base
  git -C "$repo" branch stale
  git -C "$repo" -c user.name=t -c user.email=t@t commit --quiet --allow-empty -m live
  REV_LIVE="$(git -C "$repo" rev-parse HEAD)"
  git -C "$repo" -c user.name=t -c user.email=t@t commit --quiet --allow-empty -m child
  REV_CHILD="$(git -C "$repo" rev-parse HEAD)"
  git -C "$repo" checkout --quiet stale
  git -C "$repo" -c user.name=t -c user.email=t@t commit --quiet --allow-empty -m stale
  REV_STALE="$(git -C "$repo" rev-parse HEAD)"
  export SUBROUTER_DEPLOY_REPO_URL="$repo"
  export SUBROUTER_DEPLOY_REPO_CACHE="$ROOT/state/subrouter.git"
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
bash "$DEPLOY" install "$ROOT/candidate" --revision "$REV_CHILD" --label v9.9.9 >/dev/null 2>&1
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
bash "$DEPLOY" install "$ROOT/candidate" --revision "$REV_CHILD" >/dev/null 2>&1
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
bash "$DEPLOY" install "$ROOT/candidate" --revision "$REV_CHILD" >/dev/null 2>&1
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
bash "$DEPLOY" install "$ROOT/candidate" --revision "$REV_CHILD" >/dev/null 2>&1
grep -q "pinned by hand" "$SUBROUTER_UPGRADE_INHIBIT_FILE" 2>/dev/null
check "an existing autoupdate pin is restored after a deploy" $?
teardown

# 5. An install is refused while public health is already down.
setup ok
rm -f "$ROOT/health"
before="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
bash "$DEPLOY" install "$ROOT/candidate" --revision "$REV_CHILD" >/dev/null 2>&1
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
bash "$DEPLOY" install "$ROOT/candidate" --revision "$REV_CHILD" >/dev/null 2>&1
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

# 10. A restart must not disarm the operator's autoupdate pin. This host is
# pinned because the next release cannot become ready on it, and a restart that
# cleared the pin let the updater install that release two minutes later.
setup ok
install_restart_launchctl
mkdir -p "$(dirname "$SUBROUTER_UPGRADE_INHIBIT_FILE")"
printf 'pinned by hand\n' >"$SUBROUTER_UPGRADE_INHIBIT_FILE"
bash "$DEPLOY" restart-daemon >/dev/null 2>&1
grep -q "pinned by hand" "$SUBROUTER_UPGRADE_INHIBIT_FILE" 2>/dev/null
check "restart-daemon leaves an existing autoupdate pin in place" $?
teardown

# 11. Replacing the supervisor must not disarm it either.
setup ok
install_restart_launchctl
export SUBROUTER_SUPERVISOR_BIN="$ROOT/bin/supervisor"
printf '#!/bin/sh\nexit 0\n' >"$SUBROUTER_SUPERVISOR_BIN"; chmod 0755 "$SUBROUTER_SUPERVISOR_BIN"
printf '#!/bin/sh\n# new\nexit 0\n' >"$ROOT/supervisor-candidate"; chmod 0755 "$ROOT/supervisor-candidate"
mkdir -p "$(dirname "$SUBROUTER_UPGRADE_INHIBIT_FILE")"
printf 'pinned by hand\n' >"$SUBROUTER_UPGRADE_INHIBIT_FILE"
printf 'ok\n' >"$HEALTH_FILE"
bash "$DEPLOY" install-supervisor "$ROOT/supervisor-candidate" >/dev/null 2>&1
grep -q "pinned by hand" "$SUBROUTER_UPGRADE_INHIBIT_FILE" 2>/dev/null
check "install-supervisor leaves an existing autoupdate pin in place" $?
teardown

# --- Release installs, pins and kept backups --------------------------------
# A file:// release tree stands in for GitHub: <tag>/<asset> and SHA256SUMS.

make_release() { # make_release <tag> [bad-sum|no-sum]
  local tag="$1" dir="$ROOT/releases/$1" asset="subrouter_${1#v}_darwin_amd64"
  mkdir -p "$dir"
  printf '#!/bin/sh\n# release %s\nexit 0\n' "$tag" >"$dir/$asset"
  chmod 0755 "$dir/$asset"
  case "${2:-}" in
    bad-sum) printf '%064d  %s\n' 0 "$asset" >"$dir/SHA256SUMS" ;;
    no-sum) printf '%064d  subrouter_%s_darwin_arm64\n' 0 "${tag#v}" >"$dir/SHA256SUMS" ;;
    *) (cd "$dir" && shasum -a 256 "$asset" >SHA256SUMS) ;;
  esac
  # Each release is tagged on a commit that contains the previous one.
  git -C "$ROOT/repo" checkout --quiet -B releases
  git -C "$ROOT/repo" -c user.name=t -c user.email=t@t commit --quiet --allow-empty -m "$tag"
  git -C "$ROOT/repo" tag "$tag"
}

release_env() {
  export SUBROUTER_RELEASE_DOWNLOAD_URL="file://$ROOT/releases"
  export SUBROUTER_RELEASE_ARCH=x86_64
  export SUBROUTER_BACKUP_DIR="$ROOT/state/backups"
  printf 'v1.0.0\n' >"$SUBROUTER_VERSION_FILE"
}

# 12. install-release downloads, verifies and installs a release.
setup ok
release_env
make_release v2.0.0
bash "$DEPLOY" install-release v2.0.0 >/dev/null 2>&1
rc=$?
[ "$rc" -eq 0 ] && cmp -s "$ROOT/bin/subrouter" "$ROOT/releases/v2.0.0/subrouter_2.0.0_darwin_amd64"
check "install-release installs the verified release worker" $?
[ "$(cat "$SUBROUTER_VERSION_FILE")" = "v2.0.0" ]
check "install-release records the release tag" $?
ls "$SUBROUTER_BACKUP_DIR"/*_v1.0.0 >/dev/null 2>&1
check "install-release keeps the replaced worker as a versioned backup" $?
teardown

# 13. A checksum mismatch installs nothing.
setup ok
release_env
make_release v2.0.0 bad-sum
before="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
bash "$DEPLOY" install-release v2.0.0 >/dev/null 2>&1
rc=$?
after="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
[ "$rc" -ne 0 ] && [ "$before" = "$after" ] && [ "$(cat "$SUBROUTER_VERSION_FILE")" = "v1.0.0" ] && [ ! -s "$ROOT/upgrade.calls" ]
check "install-release refuses a checksum mismatch" $?
teardown

# 14. A release without exactly one checksum line installs nothing.
setup ok
release_env
make_release v2.0.0 no-sum
before="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
bash "$DEPLOY" install-release v2.0.0 >/dev/null 2>&1
rc=$?
after="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
[ "$rc" -ne 0 ] && [ "$before" = "$after" ]
check "install-release refuses a release with no checksum for the asset" $?
bash "$DEPLOY" install-release 'v2;rm' >/dev/null 2>&1
[ $? -ne 0 ]
check "install-release refuses a malformed version" $?
teardown

# 15. Only the newest three backups are kept, loose legacy copies included.
setup ok
release_env
for n in 1 2 3 4 5; do
  : >"$ROOT/bin/subrouter.backup-2026010${n}-000000"
  touch -t "2026010${n}0000" "$ROOT/bin/subrouter.backup-2026010${n}-000000"
done
for tag in v2.0.0 v3.0.0 v4.0.0 v5.0.0; do
  make_release "$tag"
  bash "$DEPLOY" install-release "$tag" >/dev/null 2>&1
done
[ "$(find "$SUBROUTER_BACKUP_DIR" -type f | wc -l)" -eq 3 ] \
  && ls "$SUBROUTER_BACKUP_DIR"/*_v4.0.0 >/dev/null 2>&1 \
  && ! ls "$SUBROUTER_BACKUP_DIR"/*_v1.0.0 >/dev/null 2>&1
check "only the newest three versioned backups are kept" $?
[ "$(find "$ROOT/bin" -name 'subrouter.backup-*' | wc -l)" -eq 3 ] && [ ! -e "$ROOT/bin/subrouter.backup-20260101-000000" ]
check "older loose subrouter.backup-* copies are pruned" $?

# 16. rollback --to puts a kept release back and names it in the marker.
bash "$DEPLOY" rollback --to v3.0.0 >/dev/null 2>&1
rc=$?
[ "$rc" -eq 0 ] && cmp -s "$ROOT/bin/subrouter" "$ROOT/releases/v3.0.0/subrouter_3.0.0_darwin_amd64" \
  && [ "$(cat "$SUBROUTER_VERSION_FILE")" = "v3.0.0" ]
check "rollback --to restores a kept release" $?
ls "$SUBROUTER_BACKUP_DIR"/*_v5.0.0 >/dev/null 2>&1
check "rollback keeps the worker it replaced, so it can be rolled forward" $?
bash "$DEPLOY" rollback --to v1.0.0 >/dev/null 2>&1
[ $? -ne 0 ] && cmp -s "$ROOT/bin/subrouter" "$ROOT/releases/v3.0.0/subrouter_3.0.0_darwin_amd64"
check "rollback --to a pruned version is refused" $?
list_out="$(bash "$DEPLOY" list 2>&1)"
printf '%s\n' "$list_out" | grep -q '^installed v3.0.0' \
  && printf '%s\n' "$list_out" | grep -q '^pinned    no' \
  && printf '%s\n' "$list_out" | grep -q ' v5.0.0 '
check "list shows the installed version, the pin state and kept backups" $?
teardown

# 17. pin <version> installs that release and pins autoupdate at it.
setup ok
release_env
make_release v2.0.0
bash "$DEPLOY" pin v2.0.0 >/dev/null 2>&1
rc=$?
[ "$rc" -eq 0 ] && [ "$(cat "$SUBROUTER_VERSION_FILE")" = "v2.0.0" ] \
  && grep -q '^pinned at v2.0.0' "$SUBROUTER_UPGRADE_INHIBIT_FILE" 2>/dev/null
check "pin <version> installs the release and leaves the pin in place" $?
list_out="$(bash "$DEPLOY" list 2>&1)"
printf '%s\n' "$list_out" | grep -q '^pinned    yes: pinned at v2.0.0'
check "list reports the pin" $?
bash "$DEPLOY" unpin >/dev/null 2>&1
[ ! -e "$SUBROUTER_UPGRADE_INHIBIT_FILE" ]
check "unpin removes the pin" $?
teardown

# 18. pin without a version pins what is installed and installs nothing.
setup ok
release_env
bash "$DEPLOY" pin >/dev/null 2>&1
grep -q '^pinned at v1.0.0' "$SUBROUTER_UPGRADE_INHIBIT_FILE" 2>/dev/null && [ ! -s "$ROOT/upgrade.calls" ]
check "pin without a version pins the installed release" $?
teardown

# 19. A failed pinned install keeps autoupdate pinned at the running release.
setup fail
release_env
make_release v2.0.0
bash "$DEPLOY" pin v2.0.0 >/dev/null 2>&1
rc=$?
[ "$rc" -ne 0 ] && grep -q '^pinned at v1.0.0' "$SUBROUTER_UPGRADE_INHIBIT_FILE" 2>/dev/null \
  && [ "$(cat "$SUBROUTER_VERSION_FILE")" = "v1.0.0" ]
check "a failed pin install pins the release that is still running" $?
teardown

# 20. The deploy lock is shared with the guard and autoupdate: an install
# waits for the holder and then refuses, naming it, without touching anything.
setup ok
mkdir -p "$SUBROUTER_DEPLOY_LOCK_DIR"
printf 'subrouter-guard.sh pid 1\n' >"$SUBROUTER_DEPLOY_LOCK_DIR/owner"
before="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
out="$(SUBROUTER_DEPLOY_LOCK_WAIT_SECS=1 bash "$DEPLOY" install "$ROOT/candidate" --revision "$REV_CHILD" 2>&1)"
rc=$?
after="$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
[ "$rc" -ne 0 ] && [ "$before" = "$after" ] && printf '%s\n' "$out" | grep -q "subrouter-guard.sh pid 1 holds"
check "install waits for, then names, the holder of the shared deploy lock" $?
[ -d "$SUBROUTER_DEPLOY_LOCK_DIR" ] && [ -f "$SUBROUTER_DEPLOY_LOCK_DIR/owner" ]
check "a refused install leaves the other holder's lock alone" $?
teardown

# 21. reconfigure installs a valid worker config through a hot upgrade.
setup ok
printf '{"args":["--old"]}\n' >"$SUBROUTER_WORKER_CONFIG"; chmod 0640 "$SUBROUTER_WORKER_CONFIG"
printf '{"args":["--bedrock"],"env":{"A":"b"}}\n' >"$ROOT/new-config.json"
bash "$DEPLOY" reconfigure "$ROOT/new-config.json" >/dev/null 2>&1
rc=$?
[ "$rc" -eq 0 ] && cmp -s "$SUBROUTER_WORKER_CONFIG" "$ROOT/new-config.json" && [ "$(wc -l <"$ROOT/upgrade.calls")" -eq 1 ]
check "reconfigure installs the config and upgrades once" $?
if stat --version >/dev/null 2>&1; then live_mode="$(stat -c '%a' "$SUBROUTER_WORKER_CONFIG")"; else live_mode="$(stat -f '%Lp' "$SUBROUTER_WORKER_CONFIG")"; fi
[ "$live_mode" = "640" ]
check "reconfigure keeps the live file mode" $?
teardown

# 22. A config whose worker never becomes ready is reverted.
setup fail
printf '{"args":["--old"]}\n' >"$SUBROUTER_WORKER_CONFIG"
printf '{"args":["--bad"]}\n' >"$ROOT/new-config.json"
bash "$DEPLOY" reconfigure "$ROOT/new-config.json" >/dev/null 2>&1
rc=$?
[ "$rc" -ne 0 ] && grep -q -- "--old" "$SUBROUTER_WORKER_CONFIG" && [ "$(wc -l <"$ROOT/upgrade.calls")" -ge 2 ]
check "a failed reconfigure restores the old config and upgrades back" $?
teardown

# 23. An invalid file never reaches the supervisor.
setup ok
printf '{"args":["--addr","127.0.0.1:1"]}\n' >"$ROOT/new-config.json"
bash "$DEPLOY" reconfigure "$ROOT/new-config.json" >/dev/null 2>&1
rc=$?
[ "$rc" -ne 0 ] && [ ! -e "$SUBROUTER_WORKER_CONFIG" ] && [ ! -s "$ROOT/upgrade.calls" ]
check "reconfigure refuses a config that sets a supervisor-owned flag" $?
teardown

# 24. A plist without --worker-config would silently ignore the file.
setup ok
python3 -c 'import plistlib,sys; plistlib.dump({"ProgramArguments":["sup","supervise","--","--flag"]}, open(sys.argv[1],"wb"))' "$SUBROUTER_PLIST"
printf '{"args":[]}\n' >"$ROOT/new-config.json"
bash "$DEPLOY" reconfigure "$ROOT/new-config.json" >/dev/null 2>&1
rc=$?
[ "$rc" -ne 0 ] && [ ! -s "$ROOT/upgrade.calls" ]
check "reconfigure refuses a plist that does not wire --worker-config" $?
teardown

# Lineage: a candidate must contain the live worker's recorded commit.
setup ok
bash "$DEPLOY" install "$ROOT/candidate" >/dev/null 2>&1
rc=$?
[ "$rc" -ne 0 ] && ! cmp -s "$ROOT/bin/subrouter" "$ROOT/candidate" && [ ! -s "$ROOT/upgrade.calls" ]
check "install without --revision is refused" $?
teardown

setup ok
bash "$DEPLOY" record-revision "$REV_LIVE" >/dev/null 2>&1
bash "$DEPLOY" install "$ROOT/candidate" --revision "$REV_STALE" >"$ROOT/out" 2>&1
rc=$?
[ "$rc" -ne 0 ] && ! cmp -s "$ROOT/bin/subrouter" "$ROOT/candidate" && [ ! -s "$ROOT/upgrade.calls" ] && grep -q "does not contain the live worker" "$ROOT/out"
check "a candidate built from a branch without the live commit is refused" $?
teardown

setup ok
bash "$DEPLOY" record-revision "$REV_LIVE" >/dev/null 2>&1
bash "$DEPLOY" install "$ROOT/candidate" --revision "$REV_CHILD" >/dev/null 2>&1
rc=$?
[ "$rc" -eq 0 ] && cmp -s "$ROOT/bin/subrouter" "$ROOT/candidate" && [[ "$(bash "$DEPLOY" status 2>/dev/null)" == *"revision  $REV_CHILD"* ]]
check "a candidate that contains the live commit is installed and its commit recorded" $?
teardown

setup ok
bash "$DEPLOY" install "$ROOT/candidate" --revision 0123456789abcdef0123456789abcdef01234567 >/dev/null 2>&1
rc=$?
[ "$rc" -ne 0 ] && ! cmp -s "$ROOT/bin/subrouter" "$ROOT/candidate"
check "a revision that was never pushed is refused" $?
teardown

setup ok
bash "$DEPLOY" record-revision "$REV_LIVE" >/dev/null 2>&1
bash "$DEPLOY" install "$ROOT/candidate" --allow-unrelated "emergency test" >/dev/null 2>&1
rc=$?
[ "$rc" -eq 0 ] && cmp -s "$ROOT/bin/subrouter" "$ROOT/candidate"
check "--allow-unrelated installs without the lineage check" $?
teardown

setup ok
bash "$DEPLOY" record-revision "$REV_LIVE" >/dev/null 2>&1
bash "$DEPLOY" install "$ROOT/candidate" --revision "$REV_CHILD" >/dev/null 2>&1
bash "$DEPLOY" rollback >/dev/null 2>&1
[[ "$(bash "$DEPLOY" status 2>/dev/null)" == *"revision  $REV_LIVE"* ]]
check "rollback reports the restored worker's commit" $?
teardown

if [ "$failures" -ne 0 ]; then printf '%d check(s) failed\n' "$failures"; exit 1; fi
printf 'all checks passed\n'
