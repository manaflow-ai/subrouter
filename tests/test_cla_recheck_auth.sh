#!/usr/bin/env bash
# Behavior tests for the read-only recheck authorizer.
# The fixtures model an external-fork Pull Request and an already-green CLA
# check. They exercise API responses through a fake `gh` command, not workflow
# source text.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$ROOT_DIR/.github/scripts/cla-recheck-auth.sh"
readonly PR_NUMBER=301
readonly COMMENT_ID=900
readonly COMMENT_AUTHOR_ID=25807
readonly COMMENT_AUTHOR_LOGIN=danielraffel
readonly COMMENT_BODY=recheck
readonly COMMENT_CREATED_AT=2026-09-02T03:42:16Z

command -v jq >/dev/null
test -x "$SCRIPT"

api_calls=()
pull_attempts=0

fake_gh() {
  local endpoint="${!#}"
  local method=GET
  local arg
  for arg in "$@"; do
    [[ "$arg" == POST ]] && method=POST
  done
  [[ "$method" == GET ]] || {
    echo "unexpected write method: $method" >&2
    return 97
  }
  api_calls+=("$endpoint")

  case "$FAKE_MODE:$endpoint" in
    external-fork:repos/manaflow-ai/subrouter/issues/301)
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
      jq -nc '{number:301,state:"open",pull_request:{url:"https://api.github.com/repos/manaflow-ai/subrouter/pulls/301"}}'
      ;;
    external-fork:repos/manaflow-ai/subrouter/pulls/301)
      local attempt
      attempt="$(<"${PULL_ATTEMPTS_FILE}")"
      attempt=$((attempt + 1))
      printf '%s\n' "$attempt" >"${PULL_ATTEMPTS_FILE}"
      pull_attempts="$attempt"
      if (( attempt < 3 )); then
        printf 'HTTP/2 503\r\ncontent-type: application/json\r\n\r\n{"message":"temporary upstream failure"}\n'
        return 1
      fi
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
      jq -nc '{number:301,state:"open",merged_at:null,user:{id:25807,login:"danielraffel"},base:{ref:"main",repo:{id:1228491972,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",repo:{id:200,full_name:"danielraffel/subrouter"}}}'
      ;;
    external-fork:repos/manaflow-ai/subrouter/issues/comments/900)
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
      jq -nc --arg created "$COMMENT_CREATED_AT" '{id:900,body:"recheck",user:{id:25807,login:"danielraffel",type:"User"},issue_url:"https://api.github.com/repos/manaflow-ai/subrouter/issues/301",created_at:$created,updated_at:$created}'
      ;;
    external-fork:repos/manaflow-ai/subrouter/collaborators/danielraffel/permission)
      printf 'HTTP/2 404\r\ncontent-type: application/json\r\n\r\n{"message":"Not Found","status":404}\n'
      return 1
      ;;
    closed-404:repos/manaflow-ai/subrouter/issues/301)
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
      jq -nc '{number:301,state:"open",pull_request:{url:"https://api.github.com/repos/manaflow-ai/subrouter/pulls/301"}}'
      ;;
    closed-404:repos/manaflow-ai/subrouter/pulls/301)
      printf 'HTTP/2 404\r\ncontent-type: application/json\r\n\r\n{"message":"Not Found","status":404}\n'
      return 1
      ;;
    current-null:repos/manaflow-ai/subrouter/issues/301)
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
      jq -nc '{number:301,state:"open",pull_request:{url:"https://api.github.com/repos/manaflow-ai/subrouter/pulls/301"}}'
      ;;
    current-null:repos/manaflow-ai/subrouter/pulls/301)
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
      jq -nc '{number:301,state:"open",merged_at:null,user:{id:25807,login:"danielraffel"},base:{ref:"main",repo:{id:1228491972,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",repo:null}}'
      ;;
    missing-state:repos/manaflow-ai/subrouter/issues/301)
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
      jq -nc '{number:301,pull_request:{url:"https://api.github.com/repos/manaflow-ai/subrouter/pulls/301"}}'
      ;;
    missing-merged:repos/manaflow-ai/subrouter/issues/301)
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
      jq -nc '{number:301,state:"open",pull_request:{url:"https://api.github.com/repos/manaflow-ai/subrouter/pulls/301"}}'
      ;;
    missing-merged:repos/manaflow-ai/subrouter/pulls/301)
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
      jq -nc '{number:301,state:"open",user:{id:25807,login:"danielraffel"},base:{ref:"main",repo:{id:1228491972,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",repo:{id:200,full_name:"danielraffel/subrouter"}}}'
      ;;
    transport-error:repos/manaflow-ai/subrouter/issues/301)
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
      jq -nc '{number:301,state:"open",pull_request:{url:"https://api.github.com/repos/manaflow-ai/subrouter/pulls/301"}}'
      ;;
    transport-error:repos/manaflow-ai/subrouter/pulls/301)
      printf 'HTTP/2 503\r\ncontent-type: application/json\r\n\r\n{"message":"service unavailable"}\n'
      return 1
      ;;
    authorized:repos/manaflow-ai/subrouter/issues/301)
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
      jq -nc '{number:301,state:"open",pull_request:{url:"https://api.github.com/repos/manaflow-ai/subrouter/pulls/301"}}'
      ;;
    authorized:repos/manaflow-ai/subrouter/pulls/301)
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
      jq -nc '{number:301,state:"open",merged_at:null,user:{id:25807,login:"danielraffel"},base:{ref:"main",repo:{id:1228491972,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",repo:{id:200,full_name:"danielraffel/subrouter"}}}'
      ;;
    authorized:repos/manaflow-ai/subrouter/issues/comments/900)
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
      jq -nc --arg created "$COMMENT_CREATED_AT" '{id:900,body:"recheck",user:{id:25807,login:"danielraffel",type:"User"},issue_url:"https://api.github.com/repos/manaflow-ai/subrouter/issues/301",created_at:$created,updated_at:$created}'
      ;;
    authorized:repos/manaflow-ai/subrouter/collaborators/danielraffel/permission)
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n'
      jq -nc '{user:{id:25807,login:"danielraffel"},permission:"maintain",role_name:"maintain"}'
      ;;
    malformed:*)
      printf 'HTTP/2 200\r\ncontent-type: application/json\r\n\r\n{"unexpected":true}\n'
      ;;
    *)
      echo "fixture endpoint missing for $FAKE_MODE:$endpoint" >&2
      return 98
      ;;
  esac
}

gh() { fake_gh "$@"; }
export -f fake_gh gh

run_case() {
  local mode="$1" expected_status="$2" expected_decision="$3" expected_action="$4"
  local work status
  work="$(mktemp -d)"
  export FAKE_MODE="$mode" pull_attempts=0 PULL_ATTEMPTS_FILE="$work/pull-attempts"
  printf '0\n' >"$PULL_ATTEMPTS_FILE"
  export GH_REPO=manaflow-ai/subrouter PR_NUMBER COMMENT_ID COMMENT_BODY
  export COMMENT_USER_ID="$COMMENT_AUTHOR_ID" COMMENT_USER_LOGIN="$COMMENT_AUTHOR_LOGIN" \
    COMMENT_USER_TYPE=User COMMENT_CREATED_AT
  export GITHUB_OUTPUT="$work/output.env"
  export CLA_RECHECK_RETRY_DELAY_SECONDS=0
  : >"$GITHUB_OUTPUT"
  set +e
  (cd "$ROOT_DIR" && bash "$SCRIPT") >"$work/log" 2>&1
  status="$?"
  set -e
  [[ "$status" == "$expected_status" ]] || { cat "$work/log" >&2; echo "case $mode status $status" >&2; return 1; }
  grep -Fxq "authorized=false" "$GITHUB_OUTPUT" || grep -Fxq "authorized=true" "$GITHUB_OUTPUT"
  grep -Fxq "decision=$expected_decision" "$GITHUB_OUTPUT" || { cat "$work/output.env" >&2; return 1; }
  grep -Fxq "check_action=$expected_action" "$GITHUB_OUTPUT" || { cat "$work/output.env" >&2; return 1; }
  if [[ "$mode" == external-fork ]]; then
    [[ "$(<"$PULL_ATTEMPTS_FILE")" == 3 ]] || { echo "external-fork did not retry pull API" >&2; return 1; }
  fi
  rm -rf "$work"
}

# Daniel's external-fork PR already has a durable CLA signature and a green
# required check. A temporary Pulls API failure must be retried, then a valid
# non-collaborator response is an unauthorized no-op that preserves green.
run_case external-fork 0 unauthorized preserve

# Persistent API failure is a retry state, not an unauthorized denial and not a
# new failure check. The caller can retry after GitHub recovers.
run_case transport-error 1 retry preserve

# A validated Pulls 404 means the PR disappeared between the issue and Pulls
# reads. Preserve the existing check as an ordinary no-op.
run_case closed-404 0 unauthorized preserve

# A current PR with a missing head repository is malformed identity data. It
# must fail closed instead of being mistaken for an unrelated deleted fork.
run_case current-null 1 error fail

# Missing state is malformed, not a closed Pull Request denial.
run_case missing-state 1 error fail

# Missing merged_at must not be interpreted as an explicit unmerged state.
run_case missing-merged 1 error fail

# A valid maintainer response admits the refresh path.
run_case authorized 0 authorized refresh

# Malformed API data is an explicit error and must never be treated as a
# successful or unauthorized authorization.
run_case malformed 1 error fail

echo "CLA recheck authorization behavior tests passed"
