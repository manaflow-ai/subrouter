#!/usr/bin/env bash
# Behavior tests for the merge queue CLA verifier.
# They execute the helper against a mock GitHub API. The merge queue relies on
# it to fail closed, so most cases here are rejections.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$ROOT_DIR/.github/scripts/verify-merge-group-cla.sh"
command -v jq >/dev/null
test -f "$SCRIPT"

readonly SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
readonly OTHER_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
readonly GROUP_SHA=cccccccccccccccccccccccccccccccccccccccc
readonly GENERATION=v2.2-action-212a0f2dd659b24b48a30ba35966e06dc41736af
TRUSTED_SHA="$(git -C "$ROOT_DIR" rev-parse HEAD)"
export SHA OTHER_SHA GENERATION

fake_gh() {
  local endpoint="" arg
  for arg in "$@"; do
    [[ "$arg" == repos/* ]] && endpoint="$arg"
  done
  [[ -n "$endpoint" ]] || { echo "mock endpoint missing" >&2; return 1; }
  local repo=repos/manaflow-ai/subrouter
  local calls
  calls="$(( $(cat "$FAKE_STATE/$(printf '%s' "$endpoint" | tr '/?&=' '____')" 2>/dev/null || echo 0) + 1 ))"
  printf '%s' "$calls" >"$FAKE_STATE/$(printf '%s' "$endpoint" | tr '/?&=' '____')"

  case "$endpoint" in
    "$repo/pulls/294")
      local state=open head="$SHA"
      [[ "$FAKE_MODE" == closed-pr ]] && state=closed
      [[ "$FAKE_MODE" == head-moved && "$calls" -gt 1 ]] && head="$OTHER_SHA"
      jq -nc --arg state "$state" --arg sha "$head" \
        '{number:294,state:$state,merged_at:null,base:{ref:"main",repo:{id:100,full_name:"manaflow-ai/subrouter"}},head:{sha:$sha}}'
      ;;
    "$repo/commits/$SHA/check-runs?filter=all&per_page=100&page=1")
      local native='{id:7001,name:"CLA Assistant v3",head_sha:$sha,status:"completed",conclusion:"success",app:{id:15368,slug:"github-actions"},details_url:"https://github.com/manaflow-ai/subrouter/actions/runs/8001/job/9001"}'
      local older='{id:7000,name:"CLA Assistant v3",head_sha:$sha,status:"completed",conclusion:"success",app:{id:15368,slug:"github-actions"},details_url:"https://github.com/manaflow-ai/subrouter/actions/runs/8000/job/9000"}'
      local unrelated='{id:6000,name:"Build, vet, test",head_sha:$sha,status:"completed",conclusion:"success",app:{id:15368,slug:"github-actions"},details_url:"https://github.com/manaflow-ai/subrouter/actions/runs/5000/job/5001"}'
      local filter="[$unrelated, $native]"
      case "$FAKE_MODE" in
        no-check) filter="[$unrelated]" ;;
        failed-latest) filter="[$older, ($native | .conclusion = \"failure\")]" ;;
        rerun-green) filter="[($older | .conclusion = \"failure\"), $native]" ;;
        pending-then-green)
          if (( calls == 1 )); then filter="[$older, ($native | .status = \"in_progress\" | .conclusion = null)]"
          else filter="[$older, $native]"; fi ;;
        pending-forever) filter="[$older, ($native | .status = \"in_progress\" | .conclusion = null)]" ;;
        foreign-app) filter="[$native, ($native | .id = 7002 | .app = {id:999,slug:\"impostor\"})]" ;;
        casefold) filter="[$native, ($native | .id = 7002 | .name = \"cla assistant v3\")]" ;;
        other-workflow) filter="[$native, ($native | .id = 7002 | .details_url = \"https://github.com/manaflow-ai/subrouter/actions/runs/8002/job/9002\")]" ;;
        external-url) filter="[($native | .details_url = \"https://example.com/manaflow-ai/subrouter/actions/runs/8001/job/9001\")]" ;;
      esac
      jq -nc --arg sha "$SHA" "{total_count:0, check_runs: $filter}"
      ;;
    "$repo/actions/runs/8000"|"$repo/actions/runs/8001")
      local id="${endpoint##*/}" path=.github/workflows/cla.yml event=pull_request_target
      [[ "$FAKE_MODE" == wrong-event ]] && event=pull_request
      jq -nc --argjson id "$id" --arg sha "$SHA" --arg path "$path" --arg event "$event" \
        '{id:$id,path:$path,event:$event,head_sha:$sha,pull_requests:[{number:294,base:{ref:"main"}}]}'
      ;;
    "$repo/actions/runs/8002")
      jq -nc --arg sha "$SHA" '{id:8002,path:".github/workflows/ci.yml",event:"pull_request",head_sha:$sha,pull_requests:[{number:294,base:{ref:"main"}}]}'
      ;;
    "$repo/actions/jobs/9000"|"$repo/actions/jobs/9001"|"$repo/actions/jobs/9002")
      local id="${endpoint##*/}" run_id=$(( ${endpoint##*/} - 1000 ))
      local marker="CLA generation $GENERATION" conclusion=success status=completed
      [[ "$FAKE_MODE" == stale-marker ]] && marker='CLA generation v2.1-action-0000000000000000000000000000000000000000'
      [[ "$FAKE_MODE" == failed-latest && "$id" == 9001 ]] && conclusion=failure
      jq -nc --argjson id "$id" --argjson run_id "$run_id" --arg sha "$SHA" --arg marker "$marker" \
        --arg status "$status" --arg conclusion "$conclusion" \
        '{id:$id,run_id:$run_id,name:"CLA Assistant v3",head_sha:$sha,status:$status,conclusion:$conclusion,steps:[{name:$marker,status:"completed",conclusion:"success"}]}'
      ;;
    *)
      echo "mock has no fixture for $endpoint" >&2
      return 1
      ;;
  esac
}

gh() { fake_gh "$@"; }
export -f fake_gh gh

run_case() {
  local mode="$1" expected="$2" work status
  work="$(mktemp -d)"
  mkdir "$work/state"
  export FAKE_MODE="$mode" FAKE_STATE="$work/state"
  export GH_REPO=manaflow-ai/subrouter EVENT_NAME=merge_group
  export MERGE_GROUP_HEAD_REF="refs/heads/gh-readonly-queue/main/pr-294-$OTHER_SHA"
  export MERGE_GROUP_HEAD_SHA="$GROUP_SHA" MERGE_GROUP_BASE_REF=refs/heads/main MERGE_GROUP_BASE_SHA="$OTHER_SHA"
  export TRUSTED_SHA CLA_GENERATION="$GENERATION" CLA_WAIT_SECONDS=0 CLA_WAIT_ATTEMPTS=3
  case "$mode" in
    bad-ref) export MERGE_GROUP_HEAD_REF="refs/heads/gh-readonly-queue/main/pr-294" ;;
    other-base) export MERGE_GROUP_BASE_REF=refs/heads/release ;;
    untrusted-checkout) export TRUSTED_SHA="$OTHER_SHA" ;;
    old-generation-env) export CLA_GENERATION=v2.1 ;;
  esac
  set +e
  (cd "$ROOT_DIR" && bash "$SCRIPT") >"$work/output" 2>&1
  status="$?"
  set -e
  if [[ "$expected" == pass ]]; then
    [[ "$status" == 0 ]] || { cat "$work/output" >&2; echo "case $mode failed" >&2; return 1; }
  else
    [[ "$status" != 0 ]] || { cat "$work/output" >&2; echo "case $mode unexpectedly passed" >&2; return 1; }
    grep -q '::error title=CLA merge queue policy::' "$work/output" ||
      { cat "$work/output" >&2; echo "case $mode failed without a policy error" >&2; return 1; }
  fi
  echo "ok   $mode ($expected)"
  rm -rf "$work"
}

run_case valid pass
run_case rerun-green pass
run_case pending-then-green pass
run_case no-check fail
run_case failed-latest fail
run_case pending-forever fail
run_case stale-marker fail
run_case foreign-app fail
run_case casefold fail
run_case other-workflow fail
run_case external-url fail
run_case wrong-event fail
run_case closed-pr fail
run_case head-moved fail
run_case bad-ref fail
run_case other-base fail
run_case untrusted-checkout fail
run_case old-generation-env fail
echo "CLA merge queue verifier tests passed"
