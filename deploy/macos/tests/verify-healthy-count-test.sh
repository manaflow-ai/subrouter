#!/usr/bin/env bash
# Exercises the healthy-account count in subrouter-verify.sh against canned
# usage-status bodies. Nothing here touches the real service.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERIFY="$HERE/../subrouter-verify.sh"
failures=0
SANDBOX="$(mktemp -d)"
trap 'rm -rf "$SANDBOX"' EXIT
# Point every endpoint and the state directory somewhere inert, so a script
# that ignores --count-healthy fails these checks without reaching a real
# daemon or writing outside the sandbox.
export SUBROUTER_VERIFY_STATE="$SANDBOX/state"
export SUBROUTER_HEALTH_URL="file://$SANDBOX/none"
export SUBROUTER_USAGE_URL="file://$SANDBOX/none"
export SUBROUTER_PROXY_URL="file://$SANDBOX/none"

expect() { # expect <description> <want> <usage-status json>
  local got
  got="$(printf '%s' "$3" | bash "$VERIFY" --count-healthy 2>/dev/null)"
  if [ "$got" = "$2" ]; then
    printf 'ok   %s\n' "$1"
  else
    printf 'FAIL %s: want %q, got %q\n' "$1" "$2" "$got"
    failures=$((failures + 1))
  fi
}

oauth() { # oauth <5h used> <7d used> [extra window]
  printf '{"provider":"claude","auth_mode":"oauth","auth_checked":true,"auth_valid":true,"windows":[{"Name":"5h","UsedPercent":%s},{"Name":"7d","UsedPercent":%s}%s]}' "$1" "$2" "${3:+,$3}"
}

expect "oauth under every window counts" "1 1" "[$(oauth 40 60)]"
expect "oauth over its 5h window does not count" "0 1" "[$(oauth 96 60)]"
expect "oauth over its 7d window does not count" "0 1" "[$(oauth 10 100)]"
expect "oauth with invalid auth does not count" "0 1" \
  '[{"provider":"claude","auth_mode":"oauth","auth_checked":true,"auth_valid":false,"windows":[]}]'
expect "a per-model feature window alone does not cook the account" "1 1" \
  "[$(oauth 40 60 '{"Name":"7d_opus","Feature":"opus","UsedPercent":100}')]"

# API keys carry no usage windows and are never auth-checked; usage-status
# reports auth_valid=false for them. They are routable unless a spent-credit
# hold is live.
key='{"provider":"claude","auth_mode":"apikey","auth_checked":false,"auth_valid":false,"credit":{"grant_usd":200,"spent_usd":0,"pace":"%s"}}'
expect "api key with credit counts" "1 1" "[$(printf "$key" "on pace")]"
expect "api key past its re-probe time counts" "1 1" "[$(printf "$key" "spent, re-probing")]"
expect "api key held out with a spent balance does not count" "0 1" "[$(printf "$key" "spent")]"
expect "an account with an error does not count" "0 1" \
  '[{"provider":"claude","auth_mode":"apikey","error":"revoked"}]'
expect "codex accounts are not counted" "1 1" \
  "[$(oauth 10 10),{\"provider\":\"codex\",\"auth_valid\":true,\"windows\":[]}]"
expect "the accounts wrapper form is accepted" "1 2" "{\"accounts\":[$(oauth 10 10),$(oauth 100 10)]}"
expect "invalid JSON reports nothing healthy" "0 0" "not json"

[ "$failures" -eq 0 ] || { printf '%d failure(s)\n' "$failures"; exit 1; }
printf 'all verify healthy-count checks passed\n'
