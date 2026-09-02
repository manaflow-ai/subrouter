#!/usr/bin/env bash
# Behavior tests for the Cloudflare deployment source guard.
# The cases execute the guard with a mocked branch API. They do not inspect
# workflow text and never expose a deployment credential.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$ROOT_DIR/.github/scripts/authorize-cloudflare-deploy.sh"
test -f "$SCRIPT"

readonly SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
readonly OTHER_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
export SHA OTHER_SHA

fake_gh() {
  local endpoint=""
  local silent=false
  local arg
  for arg in "$@"; do
    [[ "$arg" == repos/* ]] && endpoint="$arg"
    [[ "$arg" == --silent ]] && silent=true
  done
  [[ "$endpoint" == repos/manaflow-ai/subrouter/branches/main ]] || {
    echo "unexpected GitHub endpoint: $endpoint" >&2
    return 1
  }
  case "${FAKE_MODE:-}" in
    api-ok)
      [[ "$silent" == true ]] || jq -nc --arg sha "$SHA" '{name:"main",protected:true,commit:{sha:$sha}}'
      ;;
    api-unprotected)
      [[ "$silent" == true ]] || jq -nc --arg sha "$SHA" '{name:"main",protected:false,commit:{sha:$sha}}'
      ;;
    api-mismatch)
      [[ "$silent" == true ]] || jq -nc --arg sha "$OTHER_SHA" '{name:"main",protected:true,commit:{sha:$sha}}'
      ;;
    api-oversized)
      [[ "$silent" == true ]] && return 0
      printf '{"name":"main","protected":true,"commit":{"sha":"%s"},"padding":"' "$SHA"
      printf '%*s' 300000 '' | tr ' ' X
      printf '"}\n'
      ;;
    api-malformed)
      [[ "$silent" == true ]] && return 0
      printf '{"name":"main","protected":true,"commit":\n'
      ;;
    api-error)
      return 1
      ;;
    *)
      echo "unexpected mock mode: ${FAKE_MODE:-}" >&2
      return 1
      ;;
  esac
}

gh() { fake_gh "$@"; }
export -f fake_gh gh

run_case() {
  local mode="$1"
  local expected="$2"
  local work status
  work="$(mktemp -d)"
  export FAKE_MODE="$mode"
  export GITHUB_OUTPUT="$work/output"
  : >"$GITHUB_OUTPUT"
  export GH_REPO=manaflow-ai/subrouter EVENT_NAME=workflow_dispatch EVENT_REPOSITORY=manaflow-ai/subrouter
  export EVENT_REF=refs/heads/main EVENT_REF_PROTECTED=true
  export EVENT_SHA="$SHA" WORKFLOW_SHA="$SHA"
  export WORKFLOW_REF='manaflow-ai/subrouter/.github/workflows/cloudflare-do.yml@refs/heads/main'

  case "$mode" in
    pull-request)
      export EVENT_NAME=pull_request EVENT_REF=refs/pull/12/merge EVENT_REF_PROTECTED=false
      export WORKFLOW_SHA="$OTHER_SHA"
      ;;
    dispatch-feature)
      export EVENT_REF=refs/heads/feature
      ;;
    dispatch-unprotected)
      export EVENT_REF_PROTECTED=false
      ;;
    dispatch-workflow-branch)
      export WORKFLOW_REF='manaflow-ai/subrouter/.github/workflows/cloudflare-do.yml@refs/heads/feature'
      ;;
    dispatch-sha-mismatch)
      export WORKFLOW_SHA="$OTHER_SHA"
      ;;
    wrong-repo)
      export EVENT_REPOSITORY=someone-else/subrouter
      ;;
  esac

  set +e
  (cd "$ROOT_DIR" && bash "$SCRIPT") >"$work/log" 2>&1
  status="$?"
  set -e
  if [[ "$expected" == pass ]]; then
    [[ "$status" == 0 ]] || { cat "$work/log" >&2; echo "case $mode failed" >&2; return 1; }
    grep -Fq 'source_sha=' "$GITHUB_OUTPUT" || { cat "$work/log" >&2; echo "case $mode did not emit source" >&2; return 1; }
    if [[ "$mode" == pull-request ]]; then
      grep -Fq 'deploy_authorized=false' "$GITHUB_OUTPUT" || {
        cat "$work/log" >&2
        echo "pull request unexpectedly authorized deployment" >&2
        return 1
      }
    else
      grep -Fq 'deploy_authorized=true' "$GITHUB_OUTPUT" || {
        cat "$work/log" >&2
        echo "protected dispatch did not authorize deployment" >&2
        return 1
      }
    fi
  else
    [[ "$status" != 0 ]] || { cat "$work/log" >&2; echo "case $mode unexpectedly passed" >&2; return 1; }
    [[ ! -s "$GITHUB_OUTPUT" ]] || { cat "$work/log" >&2; echo "case $mode emitted authorization" >&2; return 1; }
  fi
  rm -rf "$work"
}

run_case pull-request pass
run_case api-ok pass
run_case dispatch-feature fail
run_case dispatch-unprotected fail
run_case dispatch-workflow-branch fail
run_case dispatch-sha-mismatch fail
run_case api-unprotected fail
run_case api-mismatch fail
run_case api-oversized fail
run_case api-malformed fail
run_case api-error fail
run_case wrong-repo fail
echo "Cloudflare dispatch guard behavior tests passed"
