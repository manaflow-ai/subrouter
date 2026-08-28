#!/usr/bin/env bash
# subrouter-verify (macOS): durable self-verification of the Claude rate-limit
# reroute. macOS port of ../gcp/subrouter-verify.sh.
#
# Two substitutions vs the systemd version:
#   - `journalctl -u ... --cursor-file`  -> byte-offset cursors over the daemon
#     log files (/var/log/subrouter.log + .err.log), advanced each run.
#   - `systemctl is-active`              -> LaunchDaemon state + health endpoint.
# All other checks are identical: the failed-reroute invariant, contract drift
# (Claude 429s / unknown unified-status), health/version, and an hourly canary
# that pins one tiny request to a cooked account and asserts it is not rejected.
#
# Output: ALERT/INFO/OK lines to this job's log and to
# /var/lib/subrouter-verify/alerts.log; a summary to status.json.
set -uo pipefail

LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter-team}"
HEALTH="${SUBROUTER_HEALTH_URL:-http://127.0.0.1:31415/_subrouter/health}"
USAGE="${SUBROUTER_USAGE_URL:-http://127.0.0.1:31415/_subrouter/usage-status}"
PROXY="${SUBROUTER_PROXY_URL:-http://127.0.0.1:31415/v1/messages}"
LOGS=("/var/log/subrouter.err.log" "/var/log/subrouter.log")
STATE="${SUBROUTER_VERIFY_STATE:-/var/lib/subrouter-verify}"
CANARY_STAMP="$STATE/canary.last"
ALERTS="$STATE/alerts.log"
STATUS="$STATE/status.json"
mkdir -p "$STATE"

now_epoch=$(date -u +%s)
now_iso=$(date -u +%Y-%m-%dT%H:%M:%SZ)
alerts=0

emit() { # level msg...
  local level="$1"; shift
  local line="$now_iso [$level] $*"
  echo "SUBROUTER-VERIFY $line"
  echo "$line" >> "$ALERTS"
  [ "$level" = "ALERT" ] && alerts=$((alerts + 1))
  return 0
}

# Read new bytes appended to a log file since the last run, advancing a
# per-file byte cursor. Seeds to the current size on first run so we never
# alert on historical log lines. Handles truncation/rotation (pos > size).
read_new_log() {
  local f="$1"
  local cf
  cf="$STATE/pos.$(echo "$f" | tr '/.' '__')"
  [ -f "$f" ] || return 0
  local size pos
  size=$(stat -f %z "$f" 2>/dev/null || echo 0)
  if [ ! -f "$cf" ]; then echo "$size" >"$cf"; return 0; fi
  pos=$(cat "$cf" 2>/dev/null || echo 0)
  case "$pos" in ''|*[!0-9]*) pos=0 ;; esac
  [ "$pos" -gt "$size" ] && pos=0
  if [ "$size" -gt "$pos" ]; then
    tail -c "+$((pos + 1))" "$f" 2>/dev/null
  fi
  echo "$size" >"$cf"
}

# --- healthy Claude account count from usage-status (for the invariant) ---
counts=$(curl -fsS "$USAGE" 2>/dev/null | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print("0 0"); sys.exit()
rows = d if isinstance(d, list) else d.get("accounts", [])
cl = [x for x in rows if x.get("provider") == "claude"]
def healthy(x):
    ws = x.get("windows") or []
    mx = max([w.get("UsedPercent", 0) for w in ws], default=0)
    return bool(x.get("auth_valid")) and not x.get("error") and mx < 95
print(sum(1 for x in cl if healthy(x)), len(cl))
' 2>/dev/null || echo "0 0")
healthy_claude=$(awk '{print $1}' <<<"$counts"); : "${healthy_claude:=0}"
total_claude=$(awk '{print $2}' <<<"$counts"); : "${total_claude:=0}"

# --- 1. health + version (detect a down service or silent rollback) ---
state_line=$(launchctl print "system/${LABEL}" 2>/dev/null | awk -F'= ' '/[^a-z]state = /{print $2; exit}')
health_ok=1
curl -fsS "$HEALTH" >/dev/null 2>&1 || health_ok=0
if [ "${state_line:-}" != "running" ] && [ "$health_ok" -eq 0 ]; then
  # launchctl phrasing varies by macOS; the health probe is the arbiter.
  emit ALERT "subrouter LaunchDaemon ${LABEL} not running (state=${state_line:-unknown})"
fi
if [ "$health_ok" -eq 0 ]; then
  emit ALERT "subrouter health endpoint not responding"
fi
ver=$(cat /etc/subrouter-version 2>/dev/null || echo unknown)

# --- 1a. self-heal a down service (recovery, not only detection) ---
# 2026-08-27: a manual update ran `launchctl bootout` on the LaunchDaemon and
# never bootstrapped it back. KeepAlive cannot revive a service that has been
# removed from the launchd domain, so the fleet's one proxy stayed down while
# this watchdog wrote ALERT lines to a log nobody was reading. On a box with
# no alert delivery, detection without recovery is not enough: when the
# service is down, put it back, then report what happened.
#
# Intentional maintenance: `sudo touch $STATE/maintenance` suppresses recovery
# (never the alerts) while the sentinel is younger than 90 minutes. The age
# cap exists so a forgotten sentinel cannot recreate the outage it allows.
PLIST="${SUBROUTER_PLIST:-/Library/LaunchDaemons/${LABEL}.plist}"
MAINTENANCE="$STATE/maintenance"
DOWN_MARKER="$STATE/health.down"
if [ "$health_ok" -eq 0 ]; then
  if [ -e "$MAINTENANCE" ] && [ -n "$(find "$MAINTENANCE" -mmin -90 2>/dev/null)" ]; then
    emit INFO "service down; maintenance sentinel is fresh, skipping recovery ($MAINTENANCE)"
  else
    if [ -e "$MAINTENANCE" ]; then
      emit ALERT "maintenance sentinel is stale (>90m), recovering anyway; remove $MAINTENANCE"
    fi
    if ! launchctl print "system/${LABEL}" >/dev/null 2>&1; then
      # Booted out of the launchd domain entirely (the 2026-08-27 case).
      # Bootstrapping cannot hurt anything: there is no service to disturb.
      if [ -f "$PLIST" ]; then
        emit ALERT "recovery: ${LABEL} missing from launchd domain, bootstrapping ${PLIST}"
        launchctl bootstrap system "$PLIST" >/dev/null 2>&1
      else
        emit ALERT "recovery impossible: ${PLIST} does not exist"
      fi
    elif [ "${state_line:-}" != "running" ]; then
      # Loaded but not running: plain kickstart starts it without killing
      # anything, so it is safe on the first failed probe.
      emit ALERT "recovery: kickstarting stopped ${LABEL}"
      launchctl kickstart "system/${LABEL}" >/dev/null 2>&1
    elif [ -e "$DOWN_MARKER" ]; then
      # Running but not answering for two consecutive cycles: a wedged
      # process. kickstart -k kills the survivor, so it waits for the second
      # cycle to rule out a transient probe failure.
      emit ALERT "recovery: ${LABEL} running but unhealthy two cycles, kickstart -k"
      launchctl kickstart -k "system/${LABEL}" >/dev/null 2>&1
    else
      emit INFO "service running but health probe failed; will kickstart -k if it persists next cycle"
    fi
    sleep 5
    if curl -fsS "$HEALTH" >/dev/null 2>&1; then
      emit INFO "recovery succeeded: health endpoint answering again"
      health_ok=1
    else
      emit ALERT "recovery attempted or deferred; health endpoint still not answering"
    fi
  fi
fi
if [ "$health_ok" -eq 0 ]; then
  : >"$DOWN_MARKER"
else
  rm -f "$DOWN_MARKER"
fi

# --- 1b. account import capability (a server nobody can add accounts to) ---
# An unset admin/account-import credential makes the server reject every
# `sr add` while everything else stays green, so this is invisible until
# someone needs to onboard an account. A worker that predates the health field
# reports nothing and is skipped rather than alerted on.
import_state=$(curl -fsS "$HEALTH" 2>/dev/null \
  | python3 -c 'import json,sys
try:
    print((json.load(sys.stdin) or {}).get("account_import",""))
except Exception:
    print("")' 2>/dev/null || true)
if [ "$import_state" = "disabled" ]; then
  emit ALERT "account import is disabled: this server rejects every sr add (no admin or account-import credential configured)"
fi


# --- read new log lines since last run (byte cursors advance themselves) ---
lines=""
for f in "${LOGS[@]}"; do
  lines+=$'\n'"$(read_new_log "$f")"
done

drift_429=0
overload_5xx=0
bedrock_5xx=0
bedrock_midstream=0
bedrock_fallthrough=0
panic_seen=0
while IFS= read -r l; do
  [ -z "$l" ] && continue
  # 2. Failed-reroute invariant: a rate-limit reached the client after failover
  #    gave up. Real bug only if healthy accounts existed and not all were tried.
  case "$l" in
    *"claude rate-limit returned to client after failover exhausted"*)
      reason=$(sed -n 's/.*reason=\([^ ]*\).*/\1/p' <<<"$l")
      tried=$(sed -n 's/.*tried_accounts=\([0-9]*\).*/\1/p' <<<"$l"); : "${tried:=0}"
      if [ "$healthy_claude" -gt 0 ] && [ "$tried" -lt "$total_claude" ]; then
        emit ALERT "failed reroute while healthy accounts existed: reason=$reason tried=$tried total_claude=$total_claude healthy=$healthy_claude :: $l"
      else
        emit INFO "failover exhausted, pool genuinely constrained: reason=$reason tried=$tried healthy=$healthy_claude"
      fi
      ;;
  esac
  # 3. Contract drift: Claude HTTP 429 reappearing (we handle it, but note it).
  case "$l" in
    *"claude account unusable upstream response"*"status=429"*)
      drift_429=$((drift_429 + 1)) ;;
  esac
  # 3b. Anthropic overload (529/5xx): not a subrouter bug, but track it.
  case "$l" in
    *"claude upstream server error"*)
      overload_5xx=$((overload_5xx + 1)) ;;
  esac
  # 3c. Bedrock fable-primary failures. Pre-stream non-2xx falls through to the
  #     pool (count it); mid-stream exceptions/truncations reach the client as a
  #     visible error event, so any occurrence is worth an ALERT line.
  case "$l" in
    *"claude-fable bedrock error response status=5"*)
      bedrock_5xx=$((bedrock_5xx + 1)) ;;
  esac
  case "$l" in
    *"fable bedrock-primary non-2xx"*|*"fable bedrock-primary request failed"*|*"claude-fable bedrock stream failed before first event"*)
      bedrock_fallthrough=$((bedrock_fallthrough + 1)) ;;
  esac
  case "$l" in
    *"claude-fable bedrock mid-stream exception"*|*"claude-fable bedrock stream truncated"*)
      bedrock_midstream=$((bedrock_midstream + 1))
      emit ALERT "bedrock mid-stream failure reached a client: $l"
      ;;
  esac
  # 3d. Daemon panics: always an ALERT.
  case "$l" in
    *"panic:"*|*"fatal error:"*)
      panic_seen=$((panic_seen + 1))
      emit ALERT "subrouter panic/fatal in daemon log: $l"
      ;;
  esac
  # 4. Contract drift: an anthropic-ratelimit-unified-status value we do not know.
  case "$l" in
    *"claude account unusable upstream response"*" anthropic-ratelimit-unified-status="*)
      st=$(sed -n 's/.* anthropic-ratelimit-unified-status=\([a-z_]*\).*/\1/p' <<<"$l")
      case "$st" in
        allowed|allowed_warning|rejected|"") : ;;
        *) emit ALERT "unknown anthropic-ratelimit-unified-status=$st (possible contract drift): $l" ;;
      esac
      ;;
  esac
done <<<"$lines"
[ "$drift_429" -gt 0 ] && emit INFO "claude HTTP 429s observed this window (handled): $drift_429"
if [ "$overload_5xx" -gt 0 ]; then
  if [ "$overload_5xx" -ge 20 ]; then
    emit ALERT "anthropic overload: $overload_5xx upstream 5xx/529 this window (clients likely force-failing)"
  else
    emit INFO "anthropic overload: $overload_5xx upstream 5xx/529 this window"
  fi
fi
if [ "$bedrock_5xx" -gt 0 ]; then
  if [ "$bedrock_5xx" -ge 10 ]; then
    emit ALERT "bedrock fable 5xx: $bedrock_5xx pre-stream server errors this window"
  else
    emit INFO "bedrock fable 5xx: $bedrock_5xx pre-stream server errors this window (fell through to pool)"
  fi
fi
[ "$bedrock_fallthrough" -gt 0 ] && emit INFO "bedrock fable fall-throughs to pool this window: $bedrock_fallthrough"

# --- 5. Canary (hourly, only when a cooked Claude account exists) ---
last_canary=$(cat "$CANARY_STAMP" 2>/dev/null || echo 0)
case "$last_canary" in ''|*[!0-9]*) last_canary=0 ;; esac
if [ $((now_epoch - last_canary)) -ge 3600 ]; then
  cooked=$(curl -fsS "$USAGE" 2>/dev/null | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit()
rows = d if isinstance(d, list) else d.get("accounts", [])
for x in rows:
    if x.get("provider") != "claude" or x.get("error"):
        continue
    ws = x.get("windows") or []
    if any(w.get("Name") == "7d" and w.get("UsedPercent", 0) >= 100 for w in ws):
        print(x.get("id")); break
' 2>/dev/null)
  if [ -n "$cooked" ]; then
    echo "$now_epoch" >"$CANARY_STAMP"
    st=$(curl -sS -D - -o /dev/null --max-time 30 -X POST "$PROXY" \
      -H 'X-Subrouter-Agent: claude' -H "X-Subrouter-Account-ID: $cooked" \
      -H "X-Subrouter-Session: verify-canary-$now_epoch" \
      -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
      -d '{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}' 2>/dev/null \
      | grep -i '^anthropic-ratelimit-unified-status:' | head -1 | tr -d '\r' | awk '{print tolower($2)}')
    case "$st" in
      allowed|allowed_warning)
        emit OK "canary: pinned cooked $cooked -> client got $st (not blocked)" ;;
      rejected)
        emit ALERT "canary: pinned cooked $cooked -> client got REJECTED (reroute BROKEN)" ;;
      *)
        emit INFO "canary: pinned cooked $cooked -> unified-status=${st:-none} (inconclusive)" ;;
    esac
  fi
fi

printf '{"ts":"%s","version":"%s","healthy_claude":%s,"total_claude":%s,"overload_5xx":%s,"claude_429s":%s,"bedrock_5xx":%s,"bedrock_midstream":%s,"bedrock_fallthrough":%s,"alerts_this_run":%s}\n' \
  "$now_iso" "$ver" "$healthy_claude" "$total_claude" "$overload_5xx" "$drift_429" "$bedrock_5xx" "$bedrock_midstream" "$bedrock_fallthrough" "$alerts" >"$STATUS"
[ "$alerts" -eq 0 ] && emit OK "checks passed (version=$ver healthy_claude=$healthy_claude/$total_claude)"
exit 0
