#!/usr/bin/env bash
# release-bake-lib.sh: the post-upgrade bake gate shared by
# subrouter-deploy.sh, subrouter-autoupdate.sh and subrouter-guard.sh.
#
# A release that becomes ready and answers /_subrouter/health passes every
# check the swap makes, even when it breaks routing or streaming. So a new
# worker is not "good" when it starts: it bakes. The installer snapshots the
# outgoing generation's /_subrouter/traffic counters as a baseline and writes
# release-state.json with state "baking". Every guard tick compares the new
# generation's outcome ratios with that baseline and either keeps waiting,
# promotes it (last-good advances only then), or rolls it back and pins the
# previous release.
#
# Sourced, not executed. Callers set STATE (the subrouter-verify directory) and
# one of HEALTH_URL / HEALTH before sourcing.
# shellcheck shell=bash

RELEASE_STATE_FILE="${SUBROUTER_RELEASE_STATE:-${STATE:-/var/lib/subrouter-verify}/release-state.json}"
# 0 disables the gate: installs are promoted immediately, as before.
BAKE_SECONDS="${SUBROUTER_BAKE_SECONDS:-1200}"
# Below this many requests the ratios are noise, so no ratio can trip.
BAKE_MIN_REQUESTS="${SUBROUTER_BAKE_MIN_REQUESTS:-50}"
# A ratio needs at least this many failures of its kind before it can trip.
BAKE_MIN_ERRORS="${SUBROUTER_BAKE_MIN_ERRORS:-5}"
# A ratio trips only when it is above BOTH factor x baseline and
# baseline + margin: the factor ignores small drift on a noisy baseline, the
# margin ignores relative jumps from a near-zero baseline.
BAKE_RATIO_FACTOR="${SUBROUTER_BAKE_RATIO_FACTOR:-2}"
BAKE_PROXY_5XX_MARGIN="${SUBROUTER_BAKE_PROXY_5XX_MARGIN:-0.02}"
BAKE_STREAM_DROP_MARGIN="${SUBROUTER_BAKE_STREAM_DROP_MARGIN:-0.02}"
BAKE_5XX_MARGIN="${SUBROUTER_BAKE_5XX_MARGIN:-0.05}"
# Worker process restarts seen during the bake (a changed started_at) that
# roll it back.
BAKE_MAX_RESTARTS="${SUBROUTER_BAKE_MAX_RESTARTS:-2}"

bake_enabled() { [ "${BAKE_SECONDS:-0}" -gt 0 ] 2>/dev/null; }

bake_traffic_url() {
  if [ -n "${SUBROUTER_TRAFFIC_URL:-}" ]; then
    printf '%s\n' "$SUBROUTER_TRAFFIC_URL"
    return
  fi
  local health="${HEALTH_URL:-${HEALTH:-http://127.0.0.1:31415/_subrouter/health}}"
  case "$health" in
    */_subrouter/health) printf '%s\n' "${health%/_subrouter/health}/_subrouter/traffic" ;;
    *) printf '\n' ;;
  esac
}

# bake_fetch_traffic prints the serving generation's /_subrouter/traffic JSON,
# or nothing when the worker predates the endpoint or does not answer.
bake_fetch_traffic() {
  local url
  url="$(bake_traffic_url)"
  [ -n "$url" ] || return 0
  curl -fsS --max-time 4 "$url" 2>/dev/null || true
}

# bake_state_field <field> prints one top-level field of the state file.
bake_state_field() {
  [ -f "$RELEASE_STATE_FILE" ] || return 0
  python3 - "$RELEASE_STATE_FILE" "$1" <<'PY' 2>/dev/null || true
import json, sys
try:
    with open(sys.argv[1]) as stream:
        value = json.load(stream).get(sys.argv[2], "")
except Exception:
    value = ""
print(value if isinstance(value, str) else json.dumps(value))
PY
}

bake_is_baking() { [ "$(bake_state_field state)" = "baking" ]; }

# bake_begin <version> <previous-version> <baseline-traffic-json> [<new-generation-traffic-json>]
# Writes state "baking" (or "promoted" when the gate is disabled). The
# baseline is the outgoing generation's counters since its own start.
bake_begin() {
  RELEASE_STATE_FILE="$RELEASE_STATE_FILE" BAKE_SECONDS="$BAKE_SECONDS" \
  BAKE_VERSION="$1" BAKE_PREVIOUS="$2" BAKE_BASELINE="$3" BAKE_GENERATION="${4:-}" \
    python3 - <<'PY'
import datetime, json, os, tempfile

def counts(raw):
    try:
        doc = json.loads(raw) if raw else None
    except ValueError:
        doc = None
    if not isinstance(doc, dict) or "requests" not in doc:
        return None
    if "responses_5xx" in doc:
        # Already a baseline: a worker replaced mid-bake keeps its bake's.
        return doc
    responses = doc.get("responses") or {}
    streams = doc.get("stream_drops") or {}
    return {
        "started_at": doc.get("started_at", ""),
        "uptime_seconds": int(doc.get("uptime_seconds") or 0),
        "requests": int(doc.get("requests") or 0),
        "responses_5xx": int(responses.get("5xx") or 0),
        "proxy_5xx": int(doc.get("proxy_5xx") or 0),
        "stream_proxy_drops": int(streams.get("proxy") or 0),
    }

path = os.environ["RELEASE_STATE_FILE"]
seconds = int(os.environ["BAKE_SECONDS"] or 0)
now = datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0)
iso = lambda t: t.strftime("%Y-%m-%dT%H:%M:%SZ")
state = {
    "version": os.environ["BAKE_VERSION"],
    "previous_version": os.environ["BAKE_PREVIOUS"],
    "state": "baking" if seconds > 0 else "promoted",
    "reason": "" if seconds > 0 else "bake gate disabled (SUBROUTER_BAKE_SECONDS=0)",
    "since": iso(now),
    "bake_until": iso(now + datetime.timedelta(seconds=seconds)) if seconds > 0 else "",
    "baseline": counts(os.environ["BAKE_BASELINE"]),
}
generation = counts(os.environ["BAKE_GENERATION"])
if generation:
    state["observed"] = {"generation_started_at": generation["started_at"], "restarts": 0}
os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path) or ".", prefix=".release-state.")
with os.fdopen(fd, "w") as stream:
    json.dump(state, stream, indent=2)
    stream.write("\n")
os.chmod(tmp, 0o644)
os.replace(tmp, path)
PY
}

# bake_mark <state> <reason>: records the end of a bake.
bake_mark() {
  [ -f "$RELEASE_STATE_FILE" ] || return 0
  RELEASE_STATE_FILE="$RELEASE_STATE_FILE" BAKE_STATE="$1" BAKE_REASON="$2" python3 - <<'PY'
import datetime, json, os, tempfile
path = os.environ["RELEASE_STATE_FILE"]
with open(path) as stream:
    state = json.load(stream)
state["state"] = os.environ["BAKE_STATE"]
state["reason"] = os.environ["BAKE_REASON"]
state["since"] = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path) or ".", prefix=".release-state.")
with os.fdopen(fd, "w") as stream:
    json.dump(state, stream, indent=2)
    stream.write("\n")
os.chmod(tmp, 0o644)
os.replace(tmp, path)
PY
}

# bake_evaluate <current-traffic-json> prints "<action>\t<reason>" where
# action is continue, promote, or rollback, and records what it observed
# (generation restarts, counters carried across them) in the state file.
bake_evaluate() {
  RELEASE_STATE_FILE="$RELEASE_STATE_FILE" BAKE_TRAFFIC="$1" \
  BAKE_MIN_REQUESTS="$BAKE_MIN_REQUESTS" BAKE_MIN_ERRORS="$BAKE_MIN_ERRORS" \
  BAKE_RATIO_FACTOR="$BAKE_RATIO_FACTOR" BAKE_PROXY_5XX_MARGIN="$BAKE_PROXY_5XX_MARGIN" \
  BAKE_STREAM_DROP_MARGIN="$BAKE_STREAM_DROP_MARGIN" BAKE_5XX_MARGIN="$BAKE_5XX_MARGIN" \
  BAKE_MAX_RESTARTS="$BAKE_MAX_RESTARTS" python3 - <<'PY'
import datetime, json, os, tempfile

env = os.environ
path = env["RELEASE_STATE_FILE"]
with open(path) as stream:
    state = json.load(stream)
if state.get("state") != "baking":
    print("idle\t")
    raise SystemExit(0)

KEYS = ("requests", "responses_5xx", "proxy_5xx", "stream_proxy_drops")

def counts(raw):
    try:
        doc = json.loads(raw) if raw else None
    except ValueError:
        doc = None
    if not isinstance(doc, dict) or "requests" not in doc:
        return None
    responses = doc.get("responses") or {}
    streams = doc.get("stream_drops") or {}
    return {
        "started_at": doc.get("started_at", ""),
        "requests": int(doc.get("requests") or 0),
        "responses_5xx": int(responses.get("5xx") or 0),
        "proxy_5xx": int(doc.get("proxy_5xx") or 0),
        "stream_proxy_drops": int(streams.get("proxy") or 0),
    }

now = datetime.datetime.now(datetime.timezone.utc)
observed = state.setdefault("observed", {})
observed.setdefault("restarts", 0)
carried = observed.setdefault("carried", {key: 0 for key in KEYS})
last = observed.get("last") or {key: 0 for key in KEYS}
current = counts(env["BAKE_TRAFFIC"])
if current is not None:
    started = current["started_at"]
    known = observed.get("generation_started_at")
    lower = any(current[key] < last.get(key, 0) for key in KEYS)
    if not known:
        observed["generation_started_at"] = started
    elif started != known or lower:
        # A new worker process: its counters restarted from zero, so keep
        # what the previous one served in the bake totals.
        observed["restarts"] += 1
        observed["generation_started_at"] = started
        for key in KEYS:
            carried[key] = carried.get(key, 0) + last.get(key, 0)
    last = {key: current[key] for key in KEYS}
    observed["last"] = last
else:
    observed["traffic_unavailable"] = True
totals = {key: carried.get(key, 0) + last.get(key, 0) for key in KEYS}
observed["totals"] = totals
observed["checked_at"] = now.strftime("%Y-%m-%dT%H:%M:%SZ")

def save():
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path) or ".", prefix=".release-state.")
    with os.fdopen(fd, "w") as stream:
        json.dump(state, stream, indent=2)
        stream.write("\n")
    os.chmod(tmp, 0o644)
    os.replace(tmp, path)

def pct(value):
    return f"{value * 100:.1f}%"

def decide():
    if observed["restarts"] >= int(env["BAKE_MAX_RESTARTS"]):
        return "rollback", f"worker restarted {observed['restarts']} times during the bake"
    baseline = state.get("baseline") or {}
    base_requests = int(baseline.get("requests") or 0)
    requests = totals["requests"]
    min_requests = int(env["BAKE_MIN_REQUESTS"])
    min_errors = int(env["BAKE_MIN_ERRORS"])
    factor = float(env["BAKE_RATIO_FACTOR"])
    checks = (
        ("proxy_5xx", "proxy 5xx", float(env["BAKE_PROXY_5XX_MARGIN"])),
        ("stream_proxy_drops", "proxy stream drops", float(env["BAKE_STREAM_DROP_MARGIN"])),
        ("responses_5xx", "all 5xx", float(env["BAKE_5XX_MARGIN"])),
    )
    if requests >= min_requests:
        for key, label, margin in checks:
            failures = totals[key]
            if failures < min_errors:
                continue
            ratio = failures / requests
            base = (int(baseline.get(key) or 0) / base_requests) if base_requests else 0.0
            if ratio > base * factor and ratio > base + margin:
                return "rollback", (
                    f"{label} {pct(ratio)} vs {pct(base)} baseline "
                    f"({failures} of {requests} requests)"
                )
    until = state.get("bake_until") or ""
    try:
        deadline = datetime.datetime.strptime(until, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc)
    except ValueError:
        deadline = now
    if now < deadline:
        left = int((deadline - now).total_seconds())
        return "continue", f"{requests} requests, proxy 5xx {totals['proxy_5xx']}, {left}s left"
    if observed.get("traffic_unavailable") and current is None:
        return "promote", "bake window passed; the worker reported no traffic counters, so only health was checked"
    if requests < min_requests:
        return "promote", (
            f"bake window passed with {requests} requests, under the {min_requests}-request floor; "
            "only health and restarts were checked"
        )
    return "promote", (
        f"baked with {requests} requests: proxy 5xx {totals['proxy_5xx']}, "
        f"all 5xx {totals['responses_5xx']}, proxy stream drops {totals['stream_proxy_drops']}"
    )

action, reason = decide()
save()
print(f"{action}\t{reason}")
PY
}

# bake_control_socket prints the supervisor control socket, from
# SUBROUTER_CONTROL_SOCKET or the LaunchDaemon's --control-socket argument.
bake_control_socket() {
  if [ -n "${SUBROUTER_CONTROL_SOCKET:-}" ]; then
    printf '%s\n' "$SUBROUTER_CONTROL_SOCKET"
    return
  fi
  [ -f "${PLIST:-}" ] || return 0
  PLIST="$PLIST" python3 - <<'PY' 2>/dev/null || true
import os, plistlib
with open(os.environ["PLIST"], "rb") as stream:
    arguments = plistlib.load(stream).get("ProgramArguments") or []
for index, argument in enumerate(arguments):
    if argument == "--control-socket" and index + 1 < len(arguments):
        print(arguments[index + 1])
        break
    if argument.startswith("--control-socket="):
        print(argument.split("=", 1)[1])
        break
PY
}

# bake_summary prints the state for `subrouter-deploy.sh status`.
bake_summary() {
  if [ ! -f "$RELEASE_STATE_FILE" ]; then
    printf 'release   no bake recorded (%s)\n' "$RELEASE_STATE_FILE"
    return
  fi
  python3 - "$RELEASE_STATE_FILE" <<'PY'
import datetime, json, sys
try:
    with open(sys.argv[1]) as stream:
        state = json.load(stream)
except Exception as error:
    print(f"release   unreadable {sys.argv[1]}: {error}")
    raise SystemExit(0)
version = state.get("version") or "unknown"
previous = state.get("previous_version") or "unknown"
kind = state.get("state") or "unknown"
line = f"release   {version} {kind} since {state.get('since', '?')} (previous {previous})"
if kind == "baking":
    try:
        until = datetime.datetime.strptime(state["bake_until"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc)
        left = int((until - datetime.datetime.now(datetime.timezone.utc)).total_seconds())
        line += f"; {max(left, 0) // 60}m{max(left, 0) % 60:02d}s left"
    except Exception:
        pass
print(line)
if state.get("reason"):
    print(f"          {state['reason']}")
totals = (state.get("observed") or {}).get("totals")
if totals:
    print("          bake so far: " + ", ".join(f"{key}={value}" for key, value in totals.items()))
baseline = state.get("baseline")
if baseline:
    print("          baseline:    " + ", ".join(f"{key}={baseline.get(key)}" for key in ("requests", "responses_5xx", "proxy_5xx", "stream_proxy_drops", "uptime_seconds")))
PY
}

# ------------------------------------------------------------------ canary
# Canary rollouts (RFC #444 step C). When the running supervisor answers
# GET /_subrouter/canary, an install starts the new worker as a candidate
# beside the incumbent with canary/start?steps=default instead of a plain
# /_subrouter/upgrade, and returns. The supervisor's stepper then walks it
# 5% -> 25% -> 100% against the incumbent over the same window and promotes or
# aborts it. The canary replaces the bake: a canary install never writes
# state "baking", so the guard's bake logic never sees it.
#
# The supervisor only moves traffic. The host bookkeeping is ours, in
# canary-rollout.json: which binary is the candidate, what was serving before,
# and whether the outcome has been handled. Whoever holds the deploy lock and
# notices the end first (the guard on its tick, a deploy, autoupdate) runs
# canary_reconcile:
#   promoted: the candidate becomes last-good, as a passed bake does.
#   aborted:  the candidate binary is still at --worker-bin, so a later
#             upgrade or a crash recovery would start it. Put last-good back
#             at the worker path without a restart (the incumbent is already
#             serving), pin autoupdate with the reason, and set
#             /etc/subrouter-version back to the release that is serving.
# Callers set BIN, LAST_GOOD, VERSION_FILE, UPGRADE_INHIBIT_FILE and PLIST (or
# SUBROUTER_CONTROL_SOCKET), and may define canary_log <INFO|ALERT> <text>.

# auto uses the canary whenever the supervisor supports it; 0 never does.
CANARY_MODE="${SUBROUTER_DEPLOY_CANARY:-auto}"
CANARY_STEPS="${SUBROUTER_CANARY_STEPS:-default}"
CANARY_DWELL="${SUBROUTER_CANARY_DWELL:-}"
CANARY_ROLLOUT_FILE="${SUBROUTER_CANARY_ROLLOUT:-${STATE:-/var/lib/subrouter-verify}/canary-rollout.json}"
CANARY_OUTCOME=""
CANARY_BODY=""
CANARY_WROTE_PIN=0

if ! declare -F canary_log >/dev/null; then
  canary_log() { printf 'subrouter-canary: %s\n' "$2" >&2; }
fi

canary_steps_text() {
  if [ "$CANARY_STEPS" = "default" ]; then printf '5%% -> 25%% -> 100%%, gated'; else printf 'steps %s' "$CANARY_STEPS"; fi
}

canary_enabled() { [ "$CANARY_MODE" != "0" ] && [ "$CANARY_MODE" != "off" ]; }

canary_sha() { [ -f "$1" ] && shasum -a 256 "$1" | awk '{print $1}' || echo "missing"; }

# canary_json <json> <dotted.path> prints one value, "" when it is missing.
canary_json() {
  CANARY_DOC="$1" python3 - "$2" <<'PY' 2>/dev/null || true
import json, os, sys
try:
    value = json.loads(os.environ["CANARY_DOC"])
except ValueError:
    value = None
for part in sys.argv[1].split("."):
    value = value.get(part) if isinstance(value, dict) else None
if value is None:
    value = ""
print(value if isinstance(value, str) else json.dumps(value))
PY
}

# canary_query <socket> prints GET /_subrouter/canary, and fails when the
# supervisor does not answer or predates the endpoint (404).
canary_query() {
  local socket="$1" body
  [ -n "$socket" ] && [ -S "$socket" ] || return 1
  body="$(curl -fsS --max-time 10 --unix-socket "$socket" http://localhost/_subrouter/canary 2>/dev/null)" || return 1
  [ -n "$(canary_json "$body" state)" ] || return 1
  printf '%s\n' "$body"
}

# canary_supported <socket>: the supervisor can run a canary and the mode
# allows it.
canary_supported() { canary_enabled && canary_query "$1" >/dev/null; }

# canary_post <socket> <path?query> prints the body; fails on a non-2xx answer.
canary_post() {
  local out code
  out="$(curl -sS --max-time 150 --unix-socket "$1" -X POST -w '\n%{http_code}' "http://localhost$2" 2>&1)" || {
    printf '%s\n' "$out"
    return 1
  }
  code="${out##*$'\n'}"
  printf '%s\n' "${out%$'\n'*}"
  case "$code" in 2[0-9][0-9]) return 0 ;; *) return 1 ;; esac
}

canary_rollout_field() {
  [ -f "$CANARY_ROLLOUT_FILE" ] || return 0
  canary_json "$(cat "$CANARY_ROLLOUT_FILE" 2>/dev/null)" "$1"
}

# canary_rollout_update key=value... merges string fields into the file.
canary_rollout_update() {
  CANARY_ROLLOUT_FILE="$CANARY_ROLLOUT_FILE" python3 - "$@" <<'PY'
import json, os, sys, tempfile
path = os.environ["CANARY_ROLLOUT_FILE"]
try:
    with open(path) as stream:
        doc = json.load(stream)
except (OSError, ValueError):
    doc = {}
for pair in sys.argv[1:]:
    key, _, value = pair.partition("=")
    doc[key] = value
os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path) or ".", prefix=".canary-rollout.")
with os.fdopen(fd, "w") as stream:
    json.dump(doc, stream, indent=2)
    stream.write("\n")
os.chmod(tmp, 0o644)
os.replace(tmp, path)
PY
}

# A rollout is pending from just before canary/start until its outcome is
# handled. While it is, nothing may record the binary at BIN as last-good.
canary_rollout_pending() {
  [ -f "$CANARY_ROLLOUT_FILE" ] && [ -z "$(canary_rollout_field resolved)" ]
}

canary_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
canary_actor() { printf '%s' "${SUDO_USER:-$(id -un 2>/dev/null || echo unknown)}"; }

# canary_install_mode <socket> prints how an install should swap the worker:
#   canary            start it as a canary
#   plain             /_subrouter/upgrade (no canary support, or turned off)
#   baking            plain, because the incumbent is still baking
#   running <release> refuse: a canary is already rolling out
canary_install_mode() {
  local socket="$1" body
  canary_enabled || { echo plain; return; }
  body="$(canary_query "$socket")" || { echo plain; return; }
  if [ "$(canary_json "$body" state)" = "canary" ]; then
    echo "running $(canary_json "$body" release)"
    return
  fi
  # A worker that is still baking is not good yet, so it cannot be the
  # incumbent a canary falls back to. Replace it the way a bake expects.
  if declare -F bake_is_baking >/dev/null && bake_is_baking; then
    echo baking
    return
  fi
  echo canary
}

# canary_install <candidate> <label> <previous-label> <socket> <fallback>
# swaps <candidate> in at BIN and starts it as a canary. The caller holds the
# deploy lock and checked canary_install_mode. On failure BIN is <fallback>
# again and the incumbent never stopped serving.
canary_install() {
  local candidate="$1" label="$2" previous="$3" socket="$4" fallback="$5"
  local candidate_sha incumbent_sha out
  candidate_sha="$(canary_sha "$candidate")"
  incumbent_sha="$(canary_sha "$BIN")"
  # The incumbent is serving and keeps serving through the whole rollout, so
  # it is last-good; that is what an abort puts back.
  mkdir -p "$(dirname "$LAST_GOOD")"
  cp -p "$BIN" "${LAST_GOOD}.new" && mv -f "${LAST_GOOD}.new" "$LAST_GOOD" || return 1
  rm -f "$CANARY_ROLLOUT_FILE"
  canary_rollout_update "label=$label" "previous_label=$previous" \
    "candidate_sha256=$candidate_sha" "incumbent_sha256=$incumbent_sha" \
    "candidate_id=" "started_at=$(canary_now)" "started_by=$(canary_actor)" \
    "steps=$CANARY_STEPS" "resolved=" || return 1
  install -m 0755 "$candidate" "${BIN}.new" && mv -f "${BIN}.new" "$BIN" || { rm -f "$CANARY_ROLLOUT_FILE"; return 1; }
  local query="steps=${CANARY_STEPS}"
  [ -z "$CANARY_DWELL" ] || query="${query}&dwell=${CANARY_DWELL}"
  if ! out="$(canary_post "$socket" "/_subrouter/canary/start?${query}")"; then
    canary_log ALERT "the supervisor refused the canary: $(printf '%s' "$out" | head -n 1)"
    install -m 0755 "$fallback" "${BIN}.rollback" && mv -f "${BIN}.rollback" "$BIN"
    rm -f "$CANARY_ROLLOUT_FILE"
    return 1
  fi
  canary_rollout_update "candidate_id=$(canary_json "$out" candidate.id)"
  canary_log INFO "started ${label} as a canary: $(canary_json "$out" release) ($(canary_steps_text))"
  return 0
}

# canary_write_pin <text>
canary_write_pin() {
  mkdir -p "$(dirname "$UPGRADE_INHIBIT_FILE")" 2>/dev/null || true
  if printf '%s\n' "$1" >"${UPGRADE_INHIBIT_FILE}.new" 2>/dev/null &&
     chmod 0600 "${UPGRADE_INHIBIT_FILE}.new" && mv -f "${UPGRADE_INHIBIT_FILE}.new" "$UPGRADE_INHIBIT_FILE"; then
    CANARY_WROTE_PIN=1
    canary_log ALERT "worker autoupdate pinned by $UPGRADE_INHIBIT_FILE until a human clears it (subrouter-deploy.sh unpin)"
  else
    canary_log ALERT "could not write $UPGRADE_INHIBIT_FILE; autoupdate is NOT pinned"
  fi
}

# The supervisor writes the release state only when it knows the file. When
# it restarted mid-rollout the file still says "canary"; close it.
canary_close_release_state() { # canary_close_release_state <state> <reason>
  declare -F bake_state_field >/dev/null || return 0
  [ "$(bake_state_field state)" = "canary" ] || return 0
  bake_mark "$1" "$2" 2>/dev/null || true
}

canary_finish_promoted() { # canary_finish_promoted <reason>
  local label live_sha candidate_sha
  label="$(canary_rollout_field label)"
  live_sha="$(canary_sha "$BIN")"
  candidate_sha="$(canary_rollout_field candidate_sha256)"
  if [ "$live_sha" = "$candidate_sha" ]; then
    mkdir -p "$(dirname "$LAST_GOOD")"
    if cp -p "$BIN" "${LAST_GOOD}.new" && mv -f "${LAST_GOOD}.new" "$LAST_GOOD"; then
      canary_log INFO "promoted ${label} (${live_sha:0:12}) to last-good: ${1:-promoted}"
    else
      rm -f "${LAST_GOOD}.new"
      canary_log ALERT "could not record ${label} (${live_sha:0:12}) as last-good; retrying next check"
      return 1
    fi
  else
    canary_log ALERT "${label} was promoted, but ${BIN} is now ${live_sha:0:12}, not the candidate ${candidate_sha:0:12}; last-good left unchanged"
  fi
  canary_rollout_update "resolved=promoted" "resolved_at=$(canary_now)" "reason=${1:-}"
  canary_close_release_state promoted "${1:-promoted}"
}

canary_finish_aborted() { # canary_finish_aborted <reason> <weight> <lost 0|1>
  local reason="$1" weight="$2" lost="$3"
  local label previous candidate_sha incumbent_sha live_sha good_sha restored=0
  label="$(canary_rollout_field label)"
  previous="$(canary_rollout_field previous_label)"
  candidate_sha="$(canary_rollout_field candidate_sha256)"
  incumbent_sha="$(canary_rollout_field incumbent_sha256)"
  live_sha="$(canary_sha "$BIN")"
  good_sha="$(canary_sha "$LAST_GOOD")"
  canary_log ALERT "${label} was aborted${weight:+ at ${weight}%}: ${reason}"
  if [ "$live_sha" = "$candidate_sha" ]; then
    if [ "$good_sha" = "missing" ] || [ "$good_sha" = "$candidate_sha" ]; then
      canary_log ALERT "no last-good worker other than the candidate; ${BIN} still holds ${label}, which a restart would start. This needs a human"
    else
      cp -p "$BIN" "${BIN}.rejected-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
      if install -m 0755 "$LAST_GOOD" "${BIN}.rollback" && mv -f "${BIN}.rollback" "$BIN"; then
        restored=1
        canary_log INFO "put last-good ${good_sha:0:12} back at ${BIN}; the incumbent kept serving, nothing restarted"
      else
        rm -f "${BIN}.rollback"
        canary_log ALERT "could not write ${BIN}; it still holds the aborted candidate"
      fi
    fi
  fi
  canary_write_pin "pinned at ${previous:-last-good} by the canary gate: aborted ${label}${weight:+ at ${weight}%} at $(canary_now) (${reason}); clear with subrouter-deploy.sh unpin"
  live_sha="$(canary_sha "$BIN")"
  if [ -n "$previous" ] && [ "$live_sha" = "$incumbent_sha" ]; then
    if printf '%s\n' "$previous" >"${VERSION_FILE}.new" 2>/dev/null && mv -f "${VERSION_FILE}.new" "$VERSION_FILE"; then
      canary_log INFO "version marker reads ${previous} again"
    else
      rm -f "${VERSION_FILE}.new" 2>/dev/null || true
      canary_log ALERT "could not set $VERSION_FILE back to ${previous}"
    fi
  fi
  # The serving generation may itself run the candidate binary: after a
  # supervisor restart, after a plain upgrade superseded the canary, or after
  # the incumbent crashed and was replaced from --worker-bin. The supervisor's
  # view of versions can be empty when a traffic read fails, so do not guess:
  # whenever last-good went back on disk, start a generation from it. The
  # listener stays bound, and when the incumbent already ran last-good this
  # only replaces it with the same binary.
  if [ "$restored" -eq 1 ]; then
    local socket
    socket="$(bake_control_socket)"
    if [ -n "$socket" ] && [ -S "$socket" ] &&
       curl -fsS --max-time 120 --unix-socket "$socket" -X POST http://localhost/_subrouter/upgrade >/dev/null 2>&1; then
      canary_log INFO "switched the serving generation to last-good behind the bound listener"
    else
      canary_log ALERT "the supervisor did not take last-good through the control socket; the serving generation may still run the candidate"
    fi
  fi
  canary_rollout_update "resolved=aborted" "resolved_at=$(canary_now)" "reason=${reason}" "weight=${weight}"
  canary_close_release_state aborted "$reason"
}

# canary_reconcile handles the end of a pending rollout and sets
# CANARY_OUTCOME to none, running, unknown (no answer from the supervisor),
# promoted or aborted. The caller holds the deploy lock.
canary_reconcile() {
  CANARY_OUTCOME=none
  canary_rollout_pending || return 0
  local socket body candidate_id live live_candidate last_candidate outcome reason weight lost=0
  socket="$(bake_control_socket)"
  candidate_id="$(canary_rollout_field candidate_id)"
  if ! body="$(canary_query "$socket")"; then
    CANARY_OUTCOME=unknown
    return 0
  fi
  CANARY_BODY="$body"
  live="$(canary_json "$body" state)"
  if [ "$live" = "canary" ]; then
    live_candidate="$(canary_json "$body" candidate.id)"
    if [ -z "$candidate_id" ] || [ "$live_candidate" = "$candidate_id" ]; then
      CANARY_OUTCOME=running
      return 0
    fi
    outcome=aborted
    reason="replaced by another canary (${live_candidate})"
    weight=""
  else
    last_candidate="$(canary_json "$body" last.candidate)"
    if [ -n "$last_candidate" ] && { [ -z "$candidate_id" ] || [ "$last_candidate" = "$candidate_id" ]; }; then
      outcome="$(canary_json "$body" last.state)"
      reason="$(canary_json "$body" last.reason)"
      weight="$(canary_json "$body" last.weight)"
    else
      outcome=aborted
      lost=1
      reason="the supervisor has no record of canary ${candidate_id:-(unknown)}; it restarted mid-rollout"
      weight=""
    fi
  fi
  if [ "$outcome" = "promoted" ]; then
    canary_finish_promoted "$reason" || { CANARY_OUTCOME=running; return 0; }
  else
    outcome=aborted
    canary_finish_aborted "${reason:-aborted}" "$weight" "$lost"
  fi
  CANARY_OUTCOME="$outcome"
}

# canary_summary prints the rollout for `subrouter-deploy.sh status`.
canary_summary() {
  local socket body
  socket="$(bake_control_socket)"
  if ! body="$(canary_query "$socket")"; then
    if [ -n "$socket" ] && [ -S "$socket" ]; then
      printf 'canary    not supported by the running supervisor (installs use a plain upgrade)\n'
    else
      printf 'canary    control socket %s does not answer\n' "${socket:-unknown}"
    fi
  else
    CANARY_DOC="$body" python3 - <<'PY'
import json, os
doc = json.loads(os.environ["CANARY_DOC"])
state = doc.get("state") or "unknown"
if state == "canary":
    line = f"canary    {doc.get('release') or 'running'}"
    steps = doc.get("steps") or []
    if steps:
        line += f"; step {doc.get('step')} of {len(steps)} ({' -> '.join(f'{s}%' for s in steps)})"
    else:
        line += "; manual (no stepper)"
    print(line)
    gate = doc.get("gate") or {}
    if gate:
        print(f"          gate {gate.get('action')}: {gate.get('reason')} (checked {doc.get('gate_checked_at', '?')})")
    candidate = doc.get("candidate") or {}
    incumbent = doc.get("incumbent") or {}
    print(f"          candidate {candidate.get('id', '?')} ({candidate.get('connections', 0)} conns), incumbent {incumbent.get('id', '?')} ({incumbent.get('connections', 0)} conns)")
else:
    last = doc.get("last")
    print(f"canary    idle" + (f"; last {doc.get('release') or last.get('state')}" if last else ""))
PY
  fi
  [ -f "$CANARY_ROLLOUT_FILE" ] || return 0
  CANARY_DOC="$(cat "$CANARY_ROLLOUT_FILE" 2>/dev/null)" python3 - <<'PY' || true
import json, os
try:
    doc = json.loads(os.environ["CANARY_DOC"])
except ValueError:
    raise SystemExit(0)
outcome = doc.get("resolved") or "in progress (last-good stays on the incumbent)"
line = f"rollout   {doc.get('label') or '?'} from {doc.get('previous_label') or '?'}, started {doc.get('started_at') or '?'} by {doc.get('started_by') or '?'}: {outcome}"
if doc.get("resolved") and doc.get("reason"):
    line += f" ({doc['reason']})"
print(line)
PY
}
