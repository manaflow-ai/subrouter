#!/usr/bin/env bash
# Exercises the canary rollout path (RFC #444 step C) of subrouter-deploy.sh,
# subrouter-guard.sh and subrouter-autoupdate.sh against a fake supervisor
# control socket that implements /_subrouter/canary the way the real one
# answers it, plus file-backed health. Nothing here touches a real service.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY="$HERE/../subrouter-deploy.sh"
GUARD="$HERE/../subrouter-guard.sh"
AUTOUPDATE="$HERE/../subrouter-autoupdate.sh"
failures=0
FAKE_PID=""

check() {
  if [ "$2" -eq 0 ]; then printf 'ok   %s\n' "$1"
  else printf 'FAIL %s\n' "$1"; failures=$((failures + 1)); fi
}

# The fake keeps its state in sup.json so a test can play the stepper: set the
# rollout to promoted or aborted, or forget it the way a restart does. mode
# "nocanary" answers 404 like a supervisor from before #452; "refuse" answers
# canary/start with 502 like a candidate that never becomes ready.
start_fake_supervisor() { # start_fake_supervisor <mode>
  python3 - "$ROOT/control.sock" "$1" "$ROOT/sup.json" "$ROOT/calls" "$SUBROUTER_RELEASE_STATE" "$ROOT/health" <<'PY' &
import http.server, json, os, socket, socketserver, sys, urllib.parse

path, mode, state_path, calls, release_state, health = sys.argv[1:7]

def load():
    try:
        with open(state_path) as stream:
            return json.load(stream)
    except (OSError, ValueError):
        return {"state": "idle", "next": 2}

def save(doc):
    with open(state_path, "w") as stream:
        json.dump(doc, stream)

def release(doc, state, weight, reason):
    document = {}
    try:
        with open(release_state) as stream:
            document = json.load(stream)
    except (OSError, ValueError):
        pass
    document.update({"version": doc.get("version", "vcandidate"), "previous_version": "vincumbent",
                     "state": state, "weight": weight, "reason": reason, "since": "2026-09-27T00:00:00Z"})
    document.pop("bake_until", None)
    with open(release_state, "w") as stream:
        json.dump(document, stream)

def status(doc):
    # Like the real supervisor, versions come from each generation's traffic:
    # the incumbent runs the candidate once something replaced it from
    # --worker-bin (a plain upgrade during the canary, a crash recovery).
    body = {"state": doc["state"], "weight": doc.get("weight", 0),
            "incumbent": {"id": "gen-1", "connections": 3, "version": doc.get("incumbent_version", "vincumbent")}}
    if doc.get("no_versions"):
        # A traffic read failed: the supervisor reports no versions.
        body["incumbent"].pop("version")
    if doc["state"] == "canary":
        body.update({"steps": [5, 25, 100], "step": 1, "release": f"vcandidate canary {doc['weight']}% (1m)",
                     "candidate": {"id": doc["candidate"], "connections": 1},
                     "gate": {"action": "hold", "reason": "12 requests, under the 50-request floor"}})
    if doc.get("last"):
        body["last"] = dict(doc["last"])
        if doc.get("no_versions"):
            body["last"].pop("version", None)
        if doc["state"] != "canary":
            body["release"] = f"vcandidate {doc['last']['state']}"
    return body

class Handler(http.server.BaseHTTPRequestHandler):
    def reply(self, code, body):
        data = (json.dumps(body) if isinstance(body, dict) else body).encode()
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        with open(calls, "a") as stream:
            stream.write("GET " + self.path + "\n")
        if mode == "nocanary" or not self.path.startswith("/_subrouter/canary"):
            return self.reply(404, "404 page not found\n")
        self.reply(200, status(load()))

    def do_POST(self):
        with open(calls, "a") as stream:
            stream.write("POST " + self.path + "\n")
        url = urllib.parse.urlparse(self.path)
        query = urllib.parse.parse_qs(url.query)
        doc = load()
        if url.path == "/_subrouter/upgrade":
            if doc["state"] == "canary":
                doc["last"] = {"state": "aborted", "candidate": doc["candidate"], "version": "vcandidate", "weight": doc["weight"], "reason": "superseded by a plain upgrade"}
                doc["state"] = "idle"
                doc["incumbent_version"] = "vcandidate"
            elif doc.get("incumbent_version") == "vcandidate":
                # A plain upgrade from the restored binary: last-good serves.
                doc["incumbent_version"] = "vincumbent"
            save(doc)
            return self.reply(200, {"active": {"id": "gen-9"}})
        if mode == "nocanary":
            return self.reply(404, "404 page not found\n")
        if url.path == "/_subrouter/canary/start":
            if mode == "refuse":
                return self.reply(502, "candidate gen-2 was not ready within 30s\n")
            if doc["state"] == "canary":
                return self.reply(409, "canary is already running\n")
            doc.update({"state": "canary", "candidate": f"gen-{doc.get('next', 2)}", "weight": 5, "next": doc.get("next", 2) + 1})
            save(doc)
            release(doc, "canary", 5, "step 1 of 3 (5% -> 25% -> 100%)")
            return self.reply(200, status(doc))
        if url.path in ("/_subrouter/canary/promote", "/_subrouter/canary/abort"):
            if doc["state"] != "canary":
                return self.reply(409, "no canary is running\n")
            outcome = "promoted" if url.path.endswith("promote") else "aborted"
            reason = "promoted by operator" if outcome == "promoted" else (query.get("reason") or ["aborted by operator"])[0]
            weight = 100 if outcome == "promoted" else doc["weight"]
            doc["last"] = {"state": outcome, "candidate": doc["candidate"], "version": "vcandidate", "weight": weight, "reason": reason}
            doc["state"] = "idle"
            save(doc)
            release(doc, outcome, weight, reason)
            if outcome == "aborted" and os.path.exists(health + ".on-abort"):
                # The incumbent answers health again once new connections
                # stop reaching a hung candidate.
                os.replace(health + ".on-abort", health)
            return self.reply(200, status(doc))
        self.reply(404, "404 page not found\n")

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

# sup_end <promoted|aborted|forget> [reason]: what the stepper (or a restart)
# does to the rollout while nobody is looking.
sup_end() {
  python3 - "$ROOT/sup.json" "$SUBROUTER_RELEASE_STATE" "$1" "${2:-}" <<'PY'
import json, sys
path, release_state, outcome, reason = sys.argv[1:5]
doc = json.load(open(path))
if outcome == "forget":
    doc = {"state": "idle", "next": doc.get("next", 2)}
else:
    weight = 100 if outcome == "promoted" else doc["weight"]
    doc["last"] = {"state": outcome, "candidate": doc["candidate"], "version": "vcandidate", "weight": weight, "reason": reason}
    doc["state"] = "idle"
    state = json.load(open(release_state))
    state.update({"state": outcome, "weight": weight, "reason": reason})
    json.dump(state, open(release_state, "w"))
json.dump(doc, open(path, "w"))
PY
}

setup() { # setup <fake-mode>
  ROOT="$(mktemp -d)"
  mkdir -p "$ROOT/bin" "$ROOT/state" "$ROOT/etc" "$ROOT/fakebin" "$ROOT/revisions"
  git init --bare -q "$ROOT/repo.git"
  git init -q "$ROOT/repo-work"
  git -C "$ROOT/repo-work" config user.email test@example.invalid
  git -C "$ROOT/repo-work" config user.name test
  printf base >"$ROOT/repo-work/base"
  git -C "$ROOT/repo-work" add base
  git -C "$ROOT/repo-work" commit -qm base
  BASE_REV="$(git -C "$ROOT/repo-work" rev-parse HEAD)"
  git -C "$ROOT/repo-work" push -q "$ROOT/repo.git" HEAD:main
  printf candidate >"$ROOT/repo-work/candidate"
  git -C "$ROOT/repo-work" add candidate
  git -C "$ROOT/repo-work" commit -qm candidate
  TEST_REVISION="$(git -C "$ROOT/repo-work" rev-parse HEAD)"
  git -C "$ROOT/repo-work" push -q "$ROOT/repo.git" HEAD:main
  export BASE_REV TEST_REVISION
  cat >"$ROOT/fakebin/go" <<'FAKEGO'
#!/bin/sh
file="$3"
if grep -q incumbent "$file" 2>/dev/null; then revision="$BASE_REV"; else revision="$TEST_REVISION"; fi
printf 'path\texample.test/subrouter\nbuild\tvcs.revision=%s\nbuild\tvcs.modified=false\n' "$revision"
FAKEGO
  chmod 0755 "$ROOT/fakebin/go"
  printf '#!/bin/sh\n# incumbent\nexit 0\n' >"$ROOT/bin/subrouter"; chmod 0755 "$ROOT/bin/subrouter"
  cp -p "$ROOT/bin/subrouter" "$ROOT/incumbent"
  printf '#!/bin/sh\n# candidate\nexit 0\n' >"$ROOT/candidate"; chmod 0755 "$ROOT/candidate"
  printf '{"ok": true}\n' >"$ROOT/health"
  printf 'v9.9.8\n' >"$ROOT/etc/subrouter-version"
  : >"$ROOT/calls"
  cat >"$ROOT/bin/launchctl" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$LAUNCHCTL_CALLS"
exit 0
FAKE
  chmod 0755 "$ROOT/bin/launchctl"
  export LAUNCHCTL_CALLS="$ROOT/launchctl.calls"
  : >"$LAUNCHCTL_CALLS"
  export SUBROUTER_BIN="$ROOT/bin/subrouter"
  export SUBROUTER_DEPLOY_STATE="$ROOT/state"
  export SUBROUTER_VERIFY_STATE="$ROOT/state"
  export SUBROUTER_LAST_GOOD="$ROOT/state/subrouter.last-good"
  export SUBROUTER_VERSION_FILE="$ROOT/etc/subrouter-version"
  export SUBROUTER_HEALTH_URL="file://$ROOT/health"
  export SUBROUTER_CONTROL_SOCKET="$ROOT/control.sock"
  export SUBROUTER_PLIST="$ROOT/team.plist"
  export SUBROUTER_UPGRADE_INHIBIT_FILE="$ROOT/transaction/upgrade-inhibited"
  export SUBROUTER_DEPLOY_LOCK_DIR="$ROOT/state/deploy.lock"
  export SUBROUTER_GUARD_LOCK_DIR="$ROOT/state/guard.lock"
  export SUBROUTER_DEPLOY_HEALTH_TIMEOUT_SECS=3
  export SUBROUTER_GUARD_HEALTH_WAIT_SECS=1
  export SUBROUTER_LAUNCHCTL="$ROOT/bin/launchctl"
  export SUBROUTER_RELEASE_STATE="$ROOT/state/release-state.json"
  export SUBROUTER_TRAFFIC_URL="file://$ROOT/traffic.json"
  export SUBROUTER_MUTATION_LOCK_FILE="$ROOT/mutation.lock"
  export SUBROUTER_DEPLOY_REPO_URL="$ROOT/repo.git"
  export SUBROUTER_DEPLOY_REPO_CACHE="$ROOT/repo-cache.git"
  export SUBROUTER_DEPLOY_REVISIONS_DIR="$ROOT/revisions"
  export PATH="$ROOT/fakebin:$PATH"
  printf '%s\n' "$BASE_REV" >"$ROOT/revisions/$(shasum -a 256 "$ROOT/bin/subrouter" | awk '{print $1}')"
  unset SUBROUTER_DEPLOY_CANARY SUBROUTER_BAKE_SECONDS
  : >"$ROOT/team.plist"
  start_fake_supervisor "$1"
}

teardown() {
  [ -z "$FAKE_PID" ] || { kill "$FAKE_PID" 2>/dev/null; wait "$FAKE_PID" 2>/dev/null; FAKE_PID=""; }
  rm -rf "$ROOT"
}

rollout_field() {
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2],""))' "$ROOT/state/canary-rollout.json" "$1" 2>/dev/null
}
release_field() {
  python3 -c 'import json,sys; v=json.load(open(sys.argv[1])).get(sys.argv[2],""); print(v if isinstance(v,str) else json.dumps(v))' \
    "$SUBROUTER_RELEASE_STATE" "$1" 2>/dev/null
}
guard_tick() { bash "$GUARD" >>"$ROOT/guard.log" 2>&1; }

# 1. Supported: install starts a canary with the default steps and returns.
setup ok
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --revision "$TEST_REVISION" >"$ROOT/install.out" 2>&1
rc=$?
[ "$rc" -eq 0 ] && grep -q '^POST /_subrouter/canary/start?steps=default$' "$ROOT/calls" \
  && ! grep -q '/_subrouter/upgrade' "$ROOT/calls"
check "install goes out as a canary with steps=default when the supervisor supports it" $?
cmp -s "$SUBROUTER_BIN" "$ROOT/candidate" && cmp -s "$SUBROUTER_LAST_GOOD" "$ROOT/incumbent" \
  && [ "$(cat "$SUBROUTER_VERSION_FILE")" = "v9.9.9" ]
check "the candidate is at the worker path, last-good is the incumbent, the marker names the candidate" $?
[ "$(rollout_field label)" = "v9.9.9" ] && [ "$(rollout_field previous_label)" = "v9.9.8" ] \
  && [ "$(rollout_field candidate_id)" = "gen-2" ] && [ -z "$(rollout_field resolved)" ]
check "the rollout is recorded as pending with the candidate generation" $?
[ "$(release_field state)" != "baking" ] && [ ! -e "$SUBROUTER_UPGRADE_INHIBIT_FILE" ]
check "a canary install starts no bake and leaves no pin behind" $?
bash "$DEPLOY" status >"$ROOT/status.out" 2>&1
grep -q '^canary    vcandidate canary 5% (1m); step 1 of 3 (5% -> 25% -> 100%)' "$ROOT/status.out" \
  && grep -q '^rollout   v9.9.9 from v9.9.8, .*: in progress' "$ROOT/status.out"
check "status shows the canary and the pending rollout" $?

# 2. While it rolls out the guard never records the candidate as last-good.
guard_tick
cmp -s "$SUBROUTER_LAST_GOOD" "$ROOT/incumbent" && grep -q 'canary: v9.9.9 is rolling out' "$ROOT/guard.log"
check "a guard tick during the canary leaves last-good on the incumbent" $?
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --revision "$TEST_REVISION" >/dev/null 2>&1   # same binary: no-op
printf '#!/bin/sh\n# third\nexit 0\n' >"$ROOT/third"; chmod 0755 "$ROOT/third"
bash "$DEPLOY" install "$ROOT/third" --label v9.9.10 --revision "$TEST_REVISION" >"$ROOT/second.out" 2>&1
[ $? -ne 0 ] && cmp -s "$SUBROUTER_BIN" "$ROOT/candidate"
check "a second install is refused while the canary rolls out" $?

# 3. The stepper aborts: the guard puts last-good back without a restart,
# pins autoupdate with the reason, and resets the version marker.
: >"$ROOT/calls"
sup_end aborted "proxy 5xx 3.2% vs 0.3% (8 of 250 requests)"
guard_tick
cmp -s "$SUBROUTER_BIN" "$ROOT/incumbent"
check "an abort puts last-good back at the worker path" $?
grep -q '^pinned at v9.9.8 by the canary gate: aborted v9.9.9 at 5% .*(proxy 5xx 3.2% vs 0.3% (8 of 250 requests))' "$SUBROUTER_UPGRADE_INHIBIT_FILE" 2>/dev/null
check "an abort pins autoupdate with the reason" $?
[ "$(cat "$SUBROUTER_VERSION_FILE")" = "v9.9.8" ]
check "an abort sets the version marker back to the incumbent" $?
[ "$(grep -c '^POST' "$ROOT/calls")" -eq 1 ] && grep -q '^POST /_subrouter/upgrade$' "$ROOT/calls" && [ ! -s "$LAUNCHCTL_CALLS" ]
check "an abort restarts nothing: one hot upgrade to last-good, no launchctl" $?
[ "$(rollout_field resolved)" = "aborted" ] && [ "$(release_field state)" = "aborted" ]
check "the rollout is recorded as aborted" $?
guard_tick
cmp -s "$SUBROUTER_LAST_GOOD" "$ROOT/incumbent"
check "the next guard tick is back to the plain last-good path" $?
teardown

# 4. The stepper promotes: the guard records the candidate as last-good.
setup ok
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --revision "$TEST_REVISION" >/dev/null 2>&1
sup_end promoted "step 3 of 3 passed: 400 requests, proxy 5xx 0 vs 1"
guard_tick
cmp -s "$SUBROUTER_LAST_GOOD" "$ROOT/candidate" && cmp -s "$SUBROUTER_BIN" "$ROOT/candidate" \
  && [ "$(rollout_field resolved)" = "promoted" ] && [ ! -e "$SUBROUTER_UPGRADE_INHIBIT_FILE" ] \
  && [ "$(cat "$SUBROUTER_VERSION_FILE")" = "v9.9.9" ]
check "a promote records the candidate as last-good" $?
teardown

# 5. deploy abort: the deploy notices first and handles it the same way.
setup ok
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --revision "$TEST_REVISION" >/dev/null 2>&1
bash "$DEPLOY" abort "streams look wrong" >"$ROOT/abort.out" 2>&1
rc=$?
[ "$rc" -eq 0 ] && grep -q '^POST /_subrouter/canary/abort?reason=streams%20look%20wrong$' "$ROOT/calls" \
  && cmp -s "$SUBROUTER_BIN" "$ROOT/incumbent" && grep -q 'streams look wrong' "$SUBROUTER_UPGRADE_INHIBIT_FILE" 2>/dev/null \
  && [ "$(cat "$SUBROUTER_VERSION_FILE")" = "v9.9.8" ]
check "subrouter-deploy.sh abort restores last-good, pins and resets the marker" $?
teardown

# 6. deploy promote during a canary promotes it through the supervisor.
setup ok
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --revision "$TEST_REVISION" >/dev/null 2>&1
bash "$DEPLOY" promote >/dev/null 2>&1
rc=$?
[ "$rc" -eq 0 ] && grep -q '^POST /_subrouter/canary/promote$' "$ROOT/calls" && cmp -s "$SUBROUTER_LAST_GOOD" "$ROOT/candidate"
check "subrouter-deploy.sh promote promotes a running canary and records last-good" $?
teardown

# 7. A supervisor that restarted mid-rollout started the candidate binary.
# The guard restores last-good and hot-swaps to it.
setup ok
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --revision "$TEST_REVISION" >/dev/null 2>&1
sup_end forget
: >"$ROOT/calls"
guard_tick
cmp -s "$SUBROUTER_BIN" "$ROOT/incumbent" && grep -q '^POST /_subrouter/upgrade$' "$ROOT/calls" \
  && grep -q 'restarted mid-rollout' "$SUBROUTER_UPGRADE_INHIBIT_FILE" 2>/dev/null \
  && [ "$(release_field state)" = "aborted" ]
check "a rollout the supervisor forgot is treated as aborted and switched back to last-good" $?
teardown

# 8. The supervisor refuses the candidate: nothing changes.
setup refuse
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --revision "$TEST_REVISION" >"$ROOT/install.out" 2>&1
rc=$?
[ "$rc" -ne 0 ] && cmp -s "$SUBROUTER_BIN" "$ROOT/incumbent" && [ ! -e "$ROOT/state/canary-rollout.json" ] \
  && [ "$(cat "$SUBROUTER_VERSION_FILE")" = "v9.9.8" ] && grep -q 'was not ready within 30s' "$ROOT/install.out"
check "a canary the supervisor refuses leaves the incumbent binary and marker in place" $?
teardown

# 9. Not supported (404): install falls back to today's plain upgrade and bake.
setup nocanary
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --revision "$TEST_REVISION" >/dev/null 2>&1
rc=$?
[ "$rc" -eq 0 ] && grep -q '^POST /_subrouter/upgrade$' "$ROOT/calls" && ! grep -q 'canary/start' "$ROOT/calls" \
  && [ "$(release_field state)" = "baking" ] && [ ! -e "$ROOT/state/canary-rollout.json" ]
check "install falls back to a plain upgrade and a bake when the supervisor has no canary" $?
teardown

# 10. --plain and SUBROUTER_DEPLOY_CANARY=0 force the plain upgrade.
setup ok
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --plain --revision "$TEST_REVISION" >/dev/null 2>&1
grep -q '^POST /_subrouter/upgrade$' "$ROOT/calls" && ! grep -q 'canary/start' "$ROOT/calls"
check "install --plain uses the plain upgrade" $?
cp -p "$ROOT/incumbent" "$SUBROUTER_BIN"; rm -f "$SUBROUTER_RELEASE_STATE"; : >"$ROOT/calls"
SUBROUTER_DEPLOY_CANARY=0 bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --revision "$TEST_REVISION" >/dev/null 2>&1
grep -q '^POST /_subrouter/upgrade$' "$ROOT/calls" && ! grep -q 'canary/start' "$ROOT/calls"
check "SUBROUTER_DEPLOY_CANARY=0 uses the plain upgrade" $?
teardown

# 11. Autoupdate sends a release out as a canary too, then defers while it runs.
setup ok
mkdir -p "$ROOT/releases/v10.0.0"
arch=amd64; case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; esac
asset="subrouter_10.0.0_darwin_${arch}"
printf '#!/bin/sh\n# release v10.0.0\nexit 0\n' >"$ROOT/releases/v10.0.0/$asset"
(cd "$ROOT/releases/v10.0.0" && shasum -a 256 "$asset" >SHA256SUMS)
printf '{"tag_name":"v10.0.0"}\n' >"$ROOT/latest.json"
export SUBROUTER_RELEASE_API_URL="file://$ROOT/latest.json" SUBROUTER_RELEASE_DOWNLOAD_URL="file://$ROOT/releases"
bash "$AUTOUPDATE" >"$ROOT/autoupdate.out" 2>&1
rc=$?
[ "$rc" -eq 0 ] && grep -q '^POST /_subrouter/canary/start?steps=default$' "$ROOT/calls" \
  && ! grep -q '/_subrouter/upgrade' "$ROOT/calls" && cmp -s "$SUBROUTER_BIN" "$ROOT/releases/v10.0.0/$asset" \
  && cmp -s "$SUBROUTER_LAST_GOOD" "$ROOT/incumbent" && [ "$(cat "$SUBROUTER_VERSION_FILE")" = "v10.0.0" ]
check "autoupdate installs a release as a canary" $?
sup_end aborted "all 5xx 9.0% vs 1.0%"
# The next release is out before the guard ticks; the updater notices the
# abort first.
mkdir -p "$ROOT/releases/v10.0.1"
printf '#!/bin/sh\n# release v10.0.1\nexit 0\n' >"$ROOT/releases/v10.0.1/subrouter_10.0.1_darwin_${arch}"
(cd "$ROOT/releases/v10.0.1" && shasum -a 256 "subrouter_10.0.1_darwin_${arch}" >SHA256SUMS)
printf '{"tag_name":"v10.0.1"}\n' >"$ROOT/latest.json"
bash "$AUTOUPDATE" >"$ROOT/autoupdate2.out" 2>&1
cmp -s "$SUBROUTER_BIN" "$ROOT/incumbent" && grep -q 'worker update deferred: pinned at v9.9.8 by the canary gate' "$ROOT/autoupdate2.out" \
  && [ "$(grep -c 'canary/start' "$ROOT/calls")" -eq 1 ]
check "autoupdate handles an abort it notices first and then honours the pin" $?
unset SUBROUTER_RELEASE_API_URL SUBROUTER_RELEASE_DOWNLOAD_URL
teardown

# 12. Health down during a canary: probes carry no session key, so they may
# be reaching a hung candidate. The guard aborts the canary before any
# strike or restart, and the incumbent answers again.
setup ok
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --revision "$TEST_REVISION" >/dev/null 2>&1
mv "$ROOT/health" "$ROOT/health.on-abort"
: >"$ROOT/calls"
guard_tick
! grep -q 'canary/abort' "$ROOT/calls" && [ -z "$(rollout_field resolved)" ] && [ "$(cat "$ROOT/state/guard.strikes")" = "1" ] \
  && [ ! -e "$SUBROUTER_UPGRADE_INHIBIT_FILE" ]
check "one failed probe during a canary is a strike, not an abort or a pin" $?
guard_tick
grep -q '^POST /_subrouter/canary/abort?reason=health+down$' "$ROOT/calls" && [ ! -s "$LAUNCHCTL_CALLS" ] \
  && [ ! -e "$ROOT/state/guard.strikes" ] && [ -e "$ROOT/health" ]
check "health down during a canary aborts it instead of restarting the service" $?
cmp -s "$SUBROUTER_BIN" "$ROOT/incumbent" && grep -q 'health down' "$SUBROUTER_UPGRADE_INHIBIT_FILE" 2>/dev/null \
  && [ "$(rollout_field resolved)" = "aborted" ]
check "the health-down abort puts last-good back and pins" $?
teardown

# 13. A plain upgrade superseded the canary (an old script, a reconfigure):
# the serving generation now runs the candidate. The reconcile restores
# last-good and switches the generation to it.
setup ok
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --revision "$TEST_REVISION" >/dev/null 2>&1
curl -fsS --unix-socket "$ROOT/control.sock" -X POST http://localhost/_subrouter/upgrade >/dev/null
: >"$ROOT/calls"
guard_tick
cmp -s "$SUBROUTER_BIN" "$ROOT/incumbent" && grep -q '^POST /_subrouter/upgrade$' "$ROOT/calls" \
  && grep -q 'switched the serving generation to last-good' "$ROOT/guard.log"
check "an abort while the candidate serves switches the generation back to last-good" $?
teardown

# 14. reconfigure is a plain upgrade, so it is refused during a canary.
setup ok
python3 -c 'import plistlib,sys; plistlib.dump({"ProgramArguments":["sup","supervise","--worker-config",sys.argv[2],"--"]}, open(sys.argv[1],"wb"))' \
  "$SUBROUTER_PLIST" "$ROOT/state/worker-config.json"
export SUBROUTER_WORKER_CONFIG="$ROOT/state/worker-config.json"
printf '{"args":[]}\n' >"$SUBROUTER_WORKER_CONFIG"
printf '{"args":[],"env":{"A":"b"}}\n' >"$ROOT/new-config.json"
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --revision "$TEST_REVISION" >/dev/null 2>&1
: >"$ROOT/calls"
bash "$DEPLOY" reconfigure "$ROOT/new-config.json" >"$ROOT/reconfigure.out" 2>&1
rc=$?
[ "$rc" -ne 0 ] && ! grep -q '/_subrouter/upgrade' "$ROOT/calls" && grep -q 'still pending' "$ROOT/reconfigure.out" \
  && ! grep -q '"A"' "$SUBROUTER_WORKER_CONFIG"
check "reconfigure is refused while a canary is pending" $?
unset SUBROUTER_WORKER_CONFIG
teardown

# 15. A pending rollout and a supervisor that does not answer: install stops.
setup ok
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --revision "$TEST_REVISION" >/dev/null 2>&1
kill "$FAKE_PID" 2>/dev/null; wait "$FAKE_PID" 2>/dev/null; FAKE_PID=""
rm -f "$ROOT/control.sock"
printf '#!/bin/sh\n# third\nexit 0\n' >"$ROOT/third"; chmod 0755 "$ROOT/third"
bash "$DEPLOY" install "$ROOT/third" --label v9.9.10 --revision "$TEST_REVISION" >"$ROOT/install.out" 2>&1
rc=$?
[ "$rc" -ne 0 ] && cmp -s "$SUBROUTER_BIN" "$ROOT/candidate" && [ -s "$ROOT/install.out" ]
check "install refuses when a rollout is pending and the supervisor does not answer" $?
teardown

# 16. handoff-supervisor stops waiting on a handoff that died without a result.
setup ok
python3 -c 'import plistlib,sys; plistlib.dump({"ProgramArguments":["sup","supervise","--addr",":31415","--"]}, open(sys.argv[1],"wb"))' "$SUBROUTER_PLIST"
printf '#!/bin/sh\necho "handoff: starting"\nexit 3\n' >"$ROOT/dead-handoff.sh"; chmod 0755 "$ROOT/dead-handoff.sh"
start=$SECONDS
SUBROUTER_HANDOFF_SCRIPT="$ROOT/dead-handoff.sh" SUBROUTER_HANDOFF_LOG="$ROOT/handoff.log" SUBROUTER_MAINTENANCE_FILE="$ROOT/state/maintenance" \
  bash "$DEPLOY" handoff-supervisor "$ROOT/candidate" >"$ROOT/handoff.out" 2>&1
rc=$?
[ "$rc" -ne 0 ] && [ $((SECONDS - start)) -lt 20 ] && grep -q 'exited without a result line' "$ROOT/handoff.out" && [ ! -d "$SUBROUTER_DEPLOY_LOCK_DIR" ]
check "handoff-supervisor gives up on a handoff that exited without a result, and releases the lock" $?
printf '#!/bin/sh\nsleep 30\n' >"$ROOT/slow-handoff.sh"; chmod 0755 "$ROOT/slow-handoff.sh"
SUBROUTER_HANDOFF_WAIT_SECS=2 SUBROUTER_HANDOFF_SCRIPT="$ROOT/slow-handoff.sh" SUBROUTER_HANDOFF_LOG="$ROOT/handoff.log" SUBROUTER_MAINTENANCE_FILE="$ROOT/state/maintenance" \
  bash "$DEPLOY" handoff-supervisor "$ROOT/candidate" >"$ROOT/handoff.out" 2>&1
rc=$?
[ "$rc" -ne 0 ] && grep -q 'has not finished after 2s' "$ROOT/handoff.out"
check "handoff-supervisor stops waiting at its deadline" $?
pkill -f "$ROOT/slow-handoff.sh" 2>/dev/null
teardown

# 17. The handoff script prints a result line on any exit, even a signal.
ROOT="$(mktemp -d)"
python3 -c 'import plistlib,sys; plistlib.dump({"ProgramArguments":["sup","supervise","--addr",":31415","--control-socket","/tmp/x.sock","--"]}, open(sys.argv[1],"wb"))' "$ROOT/team.plist"
printf '#!/bin/sh\nexit 0\n' >"$ROOT/candidate"; chmod 0755 "$ROOT/candidate"
printf '#!/bin/sh\nsleep 2\nexit 0\n' >"$ROOT/launchctl"; chmod 0755 "$ROOT/launchctl"
SUBROUTER_PLIST="$ROOT/team.plist" SUBROUTER_LAUNCHCTL="$ROOT/launchctl" SUBROUTER_MAINTENANCE_FILE="$ROOT/maintenance" \
  bash "$HERE/../subrouter-supervisor-handoff.sh" "$ROOT/candidate" "$ROOT/team.plist" >"$ROOT/out" 2>&1 &
handoff=$!
sleep 1
kill -TERM "$handoff"
wait "$handoff" 2>/dev/null
grep -q '^HANDOFF FAILED: the handoff script exited (status 143)' "$ROOT/out"
check "an interrupted handoff still prints HANDOFF FAILED" $?
rm -rf "$ROOT"

# 18. The supervisor reports no versions (its traffic reads failed) while a
# crash recovery left the candidate serving. The abort still switches the
# generation to last-good instead of guessing from empty versions.
setup ok
bash "$DEPLOY" install "$ROOT/candidate" --label v9.9.9 --revision "$TEST_REVISION" >/dev/null 2>&1
sup_end aborted "incumbent worker exited during the rollout: signal: killed"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); d["no_versions"]=True; json.dump(d, open(sys.argv[1],"w"))' "$ROOT/sup.json"
: >"$ROOT/calls"
guard_tick
cmp -s "$SUBROUTER_BIN" "$ROOT/incumbent" && grep -q '^POST /_subrouter/upgrade$' "$ROOT/calls" && [ "$(rollout_field resolved)" = "aborted" ]
check "an abort with no versions reported still switches the generation to last-good" $?
teardown

if [ "$failures" -ne 0 ]; then printf '%d check(s) failed\n' "$failures"; exit 1; fi
printf 'all checks passed\n'
