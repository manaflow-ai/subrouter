#!/usr/bin/env bash
# Behavior tests for the trusted merged-Pull-Request lock helper.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$ROOT_DIR/.github/scripts/lock-merged-pr.sh"
test -x "$SCRIPT"
command -v jq >/dev/null

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
printf 'false\n' >"$work/locked"
cat >"$work/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
endpoint=''
method=GET
for arg in "$@"; do
  [[ "$arg" == repos/* ]] && endpoint="$arg"
  [[ "$arg" == PUT ]] && method=PUT
done
case "$method:$endpoint" in
  GET:repos/manaflow-ai/subrouter/pulls/294)
    printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
    printf '%s\n' '{"number":294,"state":"closed","merged_at":"2026-09-02T00:00:00Z","base":{"ref":"main","repo":{"full_name":"manaflow-ai/subrouter","id":100}},"head":{"ref":"feature","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":null},"user":{"login":"contributor","id":300}}'
    ;;
  GET:repos/manaflow-ai/subrouter/issues/294)
    printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n{"locked":%s}\n' "$(<"$FAKE_LOCK_STATE")"
    ;;
  PUT:repos/manaflow-ai/subrouter/issues/294/lock)
    printf 'true\n' >"$FAKE_LOCK_STATE"
    printf 'HTTP/2 204\r\n\r\n'
    ;;
  *)
    echo "unexpected API endpoint: $method:$endpoint" >&2
    exit 1
    ;;
esac
GH
chmod +x "$work/gh"

export PATH="$work:$PATH"
export GH_TOKEN=test-token GH_REPO=manaflow-ai/subrouter PR_NUMBER=294
export CANONICAL_REPOSITORY_ID=100 EVENT_NAME=pull_request_target EVENT_ACTION=closed
export EVENT_REPOSITORY=manaflow-ai/subrouter EVENT_REPOSITORY_ID=100 EVENT_PR_NUMBER=294
export EVENT_PR_STATE=closed EVENT_PR_MERGED=true EVENT_BASE_REF=main
export EVENT_BASE_REPOSITORY=manaflow-ai/subrouter EVENT_BASE_REPOSITORY_ID=100
export EVENT_OPENER_LOGIN=contributor EVENT_OPENER_ID=300
export FAKE_LOCK_STATE="$work/locked"

output="$work/output"
bash "$SCRIPT" >"$output"
grep -Fq 'Locked merged Pull Request 294.' "$output"

# A second delivery is idempotent and still revalidates the live identity.
bash "$SCRIPT" >"$output"
grep -Fq 'Pull Request 294 is already locked.' "$output"

# A close event targeting another branch must fail before any API call.
export EVENT_BASE_REF=release
if bash "$SCRIPT" >"$output" 2>&1; then
  echo 'unexpected success for a non-main close event' >&2
  exit 1
fi
grep -Fq 'event base is not canonical main' "$output"

echo 'CLA lock behavior tests passed'
