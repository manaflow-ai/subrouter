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
