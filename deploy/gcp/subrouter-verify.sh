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

UNIT="subrouter.service"
HEALTH="http://127.0.0.1:31415/_subrouter/health"
USAGE="http://127.0.0.1:31415/_subrouter/usage-status"
PROXY="http://127.0.0.1:31415/v1/messages"
STATE=/var/lib/subrouter-verify
CURSOR="$STATE/cursor"
CANARY_STAMP="$STATE/canary.last"
ALERTS="$STATE/alerts.log"
STATUS="$STATE/status.json"
mkdir -p "$STATE"

now_epoch=$(date -u +%s)
now_iso=$(date -u +%Y-%m-%dT%H:%M:%SZ)
alerts=0

# Alerts that only reach a journal are alerts nobody sees. The notify worker
# requires a "sender" field and rejects the payload without it. When notify
# credentials are present, every ALERT is also delivered to Slack; delivery
# failures never fail the check, since a broken notifier must not mask the
# problem it was reporting.
NOTIFY_ENV="${SUBROUTER_NOTIFY_ENV:-/etc/subrouter/notify.env}"
[ -r "$NOTIFY_ENV" ] && . "$NOTIFY_ENV"

page() { # message...
  [ -n "${CMUX_NOTIFY_URL:-}" ] || return 0
  [ -n "${CMUX_NOTIFY_TOKEN:-}" ] || return 0
  local host; host="$(hostname -s 2>/dev/null || echo subrouter)"
  local text="[subrouter/${host}] $*"
  curl -fsS --max-time 10 -X POST "$CMUX_NOTIFY_URL" \
    -H "authorization: Bearer $CMUX_NOTIFY_TOKEN" \
    -H 'content-type: application/json' \
    --data "$(python3 -c 'import json,sys; print(json.dumps({"sender": "subrouter", "text": sys.argv[1]}))' "$text")" \
    >/dev/null 2>&1 || true
}

emit() { # level msg...
  local level="$1"; shift
  local line="$now_iso [$level] $*"
  echo "SUBROUTER-VERIFY $line"
  echo "$line" >> "$ALERTS"
  if [ "$level" = "ALERT" ]; then
    alerts=$((alerts + 1))
    page "$*"
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
if ! systemctl is-active --quiet "$UNIT"; then
  emit ALERT "subrouter service is not active"
fi
if ! curl -fsS "$HEALTH" >/dev/null 2>&1; then
  emit ALERT "subrouter health endpoint not responding"
fi
ver=$(cat /etc/subrouter-version 2>/dev/null || echo unknown)

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

printf '{"ts":"%s","version":"%s","healthy_claude":%s,"total_claude":%s,"overload_5xx":%s,"claude_429s":%s,"alerts_this_run":%s}\n' \
  "$now_iso" "$ver" "$healthy_claude" "$total_claude" "$overload_5xx" "$drift_429" "$alerts" >"$STATUS"
[ "$alerts" -eq 0 ] && emit OK "checks passed (version=$ver healthy_claude=$healthy_claude/$total_claude)"
exit 0
