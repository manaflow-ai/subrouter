#!/usr/bin/env bash
# Runs subrouter-verify.sh end to end against a fake launchd, file:// probes,
# and a sandboxed log, and checks how the failed-reroute invariant reads the
# pool. Nothing here touches the real service.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERIFY="$HERE/../subrouter-verify.sh"
failures=0

check() { # check <description> <condition-result>
  if [ "$2" -eq 0 ]; then
    printf 'ok   %s\n' "$1"
  else
    printf 'FAIL %s\n' "$1"
    failures=$((failures + 1))
  fi
}

# run_verify <usage-status body, or "" for an unreachable endpoint>
run_verify() {
  ROOT="$(mktemp -d)"
  mkdir -p "$ROOT/state" "$ROOT/bin"
  cat >"$ROOT/bin/launchctl" <<'FAKE'
#!/usr/bin/env bash
[ "${1:-}" = "print" ] && { echo "	state = running"; exit 0; }
exit 0
FAKE
  # BSD stat -f %z <file> prints the size; GNU stat reads -f differently.
  cat >"$ROOT/bin/stat" <<'FAKE'
#!/usr/bin/env bash
wc -c <"${@: -1}" | tr -d ' '
FAKE
  chmod 0755 "$ROOT/bin/launchctl" "$ROOT/bin/stat"
  printf '{"ok":true}' >"$ROOT/health"
  [ -n "$1" ] && printf '%s' "$1" >"$ROOT/usage"
  : >"$ROOT/daemon.log"
  date -u +%s >"$ROOT/state/canary.last"
  # Seed the log cursor at 0 so the next run reads the line appended below.
  echo 0 >"$ROOT/state/pos.$(echo "$ROOT/daemon.log" | tr '/.' '__')"
  printf '%s\n' 'WARN claude rate-limit returned to client after failover exhausted reason=rate_limited tried_accounts=0' >>"$ROOT/daemon.log"
  PATH="$ROOT/bin:$PATH" \
    SUBROUTER_VERIFY_STATE="$ROOT/state" \
    SUBROUTER_VERIFY_LOGS="$ROOT/daemon.log" \
    SUBROUTER_HEALTH_URL="file://$ROOT/health" \
    SUBROUTER_USAGE_URL="file://$ROOT/usage" \
    SUBROUTER_PROXY_URL="file://$ROOT/none" \
    SUBROUTER_GUARD_HEARTBEAT="$ROOT/none" \
    bash "$VERIFY" 2>/dev/null
  rm -rf "$ROOT"
}

constrained='[{"provider":"claude","auth_mode":"oauth","auth_valid":true,"windows":[{"Name":"5h","UsedPercent":100}]}]'

out="$(run_verify "$constrained")"
printf '%s\n' "$out" | grep -q 'failover exhausted, pool genuinely constrained'
check "a cooked pool reads as genuinely constrained" $?

out="$(run_verify "")"
printf '%s\n' "$out" | grep -q 'pool genuinely constrained'
check "an unreachable usage-status never reads as a constrained pool" $((1 - $?))
printf '%s\n' "$out" | grep -q '\[ALERT\] failed reroute while the pool state is unknown'
check "an unreachable usage-status alerts on the failed reroute" $?
printf '%s\n' "$out" | grep -q '\[ALERT\] usage-status unavailable'
check "an unreachable usage-status alerts by itself" $?

[ "$failures" -eq 0 ] || { printf '%d failure(s)\n' "$failures"; exit 1; }
printf 'all verify reroute-invariant checks passed\n'
