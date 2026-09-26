#!/usr/bin/env bash
# subrouter-verify: durable self-verification of the Claude rate-limit reroute.
#
# Runs from a systemd timer on subrouter-team. All checks are passive (read the
# subrouter journal + loopback status endpoints) except an hourly canary that
# pins one tiny request to a cooked account and asserts the client is rerouted.
#
# Output: ALERT/INFO/OK lines go to this unit's journal (journalctl -u
# subrouter-verify) and append to /var/lib/subrouter-verify/alerts.log; a
# machine-readable summary is written to /var/lib/subrouter-verify/status.json.
set -uo pipefail

UNIT="${SUBROUTER_VERIFY_UNIT:-subrouter.service}"
HEALTH="${SUBROUTER_VERIFY_HEALTH_URL:-http://127.0.0.1:31415/_subrouter/health}"
USAGE="${SUBROUTER_VERIFY_USAGE_URL:-http://127.0.0.1:31415/_subrouter/usage-status}"
PROXY="${SUBROUTER_VERIFY_PROXY_URL:-http://127.0.0.1:31415/v1/messages}"
STATE="${SUBROUTER_VERIFY_STATE:-/var/lib/subrouter-verify}"
VERSION_FILE="${SUBROUTER_VERIFY_VERSION_FILE:-/etc/subrouter-version}"
CURSOR="$STATE/cursor"
CANARY_STAMP="$STATE/canary.last"
ALERTS="$STATE/alerts.log"
STATUS="$STATE/status.json"
if ! mkdir -p "$STATE"; then
  echo "SUBROUTER-VERIFY could not create state directory: $STATE" >&2
  exit 1
fi

now_epoch=$(date -u +%s)
now_iso=$(date -u +%Y-%m-%dT%H:%M:%SZ)
alerts=0

# This check emits; it does not deliver. A Cloud Monitoring log-based alert
# policy matches "[ALERT]" in these lines and owns notification, rate limiting,
# grouping and auto-close. Keeping delivery out of here means no webhook
# credentials on the box and one place to change where alerts go.
emit() { # level msg...
  local level="$1"; shift
  local line="$now_iso [$level] $*"
  echo "SUBROUTER-VERIFY $line"
  echo "$line" >> "$ALERTS"
  if [ "$level" = "ALERT" ]; then
    alerts=$((alerts + 1))
  fi
  return 0
}

# --- is the service answering at all? ---
if ! curl -fsS --max-time 5 "$HEALTH" >/dev/null 2>&1; then
  emit ALERT "health endpoint is not answering at $HEALTH; service is down"
else
  active_state="$(systemctl is-active "$UNIT" 2>/dev/null || true)"
  [ "$active_state" = "active" ] || emit ALERT "$UNIT is $active_state"
fi

# --- usable account counts from usage-status (for routing invariants) ---
usage_body=""
if ! usage_body=$(curl -fsS --max-time 10 "$USAGE" 2>/dev/null); then
  emit ALERT "usage-status endpoint is not answering at $USAGE"
  counts="0 0 0 0"
elif ! counts=$(printf '%s' "$usage_body" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
rows = d if isinstance(d, list) else d.get("accounts", [])
if not isinstance(rows, list):
    sys.exit(1)
def healthy(x):
    if x.get("error"):
        return False
    if x.get("auth_mode") == "apikey":
        return True
    ws = x.get("windows") or []
    base = [w for w in ws if not w.get("Feature")]
    mx = max([w.get("UsedPercent", 0) for w in base], default=0)
    return bool(x.get("auth_valid")) and mx < 95
def provider_counts(provider):
    matches = [x for x in rows if x.get("provider") == provider]
    return sum(1 for x in matches if healthy(x)), len(matches)
hc, tc = provider_counts("claude")
ho, to = provider_counts("codex")
print(hc, tc, ho, to)
' 2>/dev/null); then
  emit ALERT "usage-status returned invalid JSON"
  counts="0 0 0 0"
fi
read -r healthy_claude total_claude healthy_codex total_codex <<<"$counts"
: "${healthy_claude:=0}" "${total_claude:=0}" "${healthy_codex:=0}" "${total_codex:=0}"
if [ "$total_claude" -gt 0 ] && [ "$healthy_claude" -eq 0 ]; then
  emit ALERT "no usable Claude accounts: $healthy_claude/$total_claude"
fi
if [ "$total_codex" -gt 0 ] && [ "$healthy_codex" -eq 0 ]; then
  emit ALERT "no usable Codex accounts: $healthy_codex/$total_codex"
fi

# --- 1. health + version (detect a down service or silent rollback) ---
if ! systemctl is-active --quiet "$UNIT"; then
  emit ALERT "subrouter service is not active"
fi
health_ok=1
curl -fsS "$HEALTH" >/dev/null 2>&1 || health_ok=0
if [ "$health_ok" -eq 0 ]; then
  emit ALERT "subrouter health endpoint not responding"
fi
ver=$(cat "$VERSION_FILE" 2>/dev/null || echo unknown)

# --- 1a. self-heal a down service (recovery, not only detection) ---
# systemd's Restart= revives a crashed process, but a unit left stopped (a
# `systemctl stop` with no matching start, the systemd analogue of the
# 2026-08-27 macOS bootout outage) stays stopped forever while this watchdog
# only alerts. When the service is down and not intentionally held down,
# start it, then report what happened.
#
# Intentional maintenance: `touch $STATE/maintenance` suppresses recovery
# (never the alerts) while the sentinel is younger than 90 minutes. The age
# cap exists so a forgotten sentinel cannot recreate the outage it allows.
MAINTENANCE="$STATE/maintenance"
if [ "$health_ok" -eq 0 ] && ! systemctl is-active --quiet "$UNIT"; then
  if [ -e "$MAINTENANCE" ] && [ -n "$(find "$MAINTENANCE" -mmin -90 2>/dev/null)" ]; then
    emit INFO "service down; maintenance sentinel is fresh, skipping recovery ($MAINTENANCE)"
  else
    if [ -e "$MAINTENANCE" ]; then
      emit ALERT "maintenance sentinel is stale (>90m), recovering anyway; remove $MAINTENANCE"
    fi
    emit ALERT "recovery: starting $UNIT"
    systemctl start "$UNIT" >/dev/null 2>&1
    sleep 5
    if curl -fsS "$HEALTH" >/dev/null 2>&1; then
      emit INFO "recovery succeeded: health endpoint answering again"
    else
      emit ALERT "recovery attempted but health endpoint still not answering"
    fi
  fi
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


# --- read new journal entries since last run (cursor-file advances itself) ---
if [ ! -f "$CURSOR" ]; then
  # Seed to "now" on first run so we do not alert on historical entries.
  journalctl -u "$UNIT" --cursor-file="$CURSOR" -n0 -o cat >/dev/null 2>&1 || true
fi
lines=$(journalctl -u "$UNIT" --cursor-file="$CURSOR" -o cat 2>/dev/null || true)

drift_429=0
overload_5xx=0
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
  # 3b. Anthropic overload (529/5xx): not a subrouter bug, but track it so we
  #     can see capacity outages that make the client (Claude Code) force-fail.
  case "$l" in
    *"claude upstream server error"*)
      overload_5xx=$((overload_5xx + 1)) ;;
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
# Anthropic overload: INFO normally, ALERT on a sustained burst so a real
# capacity outage (clients force-failing) pings the on-call.
if [ "$overload_5xx" -gt 0 ]; then
  if [ "$overload_5xx" -ge 20 ]; then
    emit ALERT "anthropic overload: $overload_5xx upstream 5xx/529 this window (clients likely force-failing)"
  else
    emit INFO "anthropic overload: $overload_5xx upstream 5xx/529 this window"
  fi
fi

# --- 5. Canary (hourly, only when a cooked Claude account exists) ---
last_canary=$(cat "$CANARY_STAMP" 2>/dev/null || echo 0)
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
    # The user is hard-blocked only on "rejected"; "allowed"/"allowed_warning"
    # both mean requests still work, so either is a pass.
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

printf '{"ts":"%s","version":"%s","healthy_claude":%s,"total_claude":%s,"healthy_codex":%s,"total_codex":%s,"overload_5xx":%s,"claude_429s":%s,"alerts_this_run":%s}\n' \
  "$now_iso" "$ver" "$healthy_claude" "$total_claude" "$healthy_codex" "$total_codex" "$overload_5xx" "$drift_429" "$alerts" >"$STATUS"
[ "$alerts" -eq 0 ] && emit OK "checks passed (version=$ver healthy_claude=$healthy_claude/$total_claude healthy_codex=$healthy_codex/$total_codex)"
exit 0
