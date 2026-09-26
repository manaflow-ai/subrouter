#!/usr/bin/env bash
# Behavior tests for the trusted CLA rerun helper.
# These tests execute the helper with a mock GitHub API. They do not inspect
# workflow source text, and the mock never receives a write token.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$ROOT_DIR/.github/scripts/rerun-failed-cla.sh"
command -v jq >/dev/null
test -f "$SCRIPT"

readonly SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
WORKFLOW_SHA="$(git -C "$ROOT_DIR" rev-parse HEAD)"
readonly WORKFLOW_SHA
readonly GENERATION=v2.2-action-212a0f2dd659b24b48a30ba35966e06dc41736af
export SHA WORKFLOW_SHA GENERATION

fake_gh() {
  local endpoint=""
  local arg
  local method=GET
  for arg in "$@"; do
    [[ "$arg" == repos/* ]] && endpoint="$arg"
    [[ "$arg" == POST ]] && method=POST
    # The production helper must retain API response bodies for its bounded
    # parser. Fail the mock if a caller suppresses the body with --silent.
    if [[ "$arg" == --silent ]]; then
      echo "mock rejected --silent: response bodies are required" >&2
      return 97
    fi
  done
  [[ -n "$endpoint" ]] || { echo "mock endpoint missing" >&2; return 1; }
  if [[ "$method" == POST ]]; then
    printf '%s\n' "$endpoint" >>"$FAKE_POST_FILE"
    printf '{}\n'
    return 0
  fi

  local head_repo='contributor/subrouter'
  local head_repo_id=200
  local marker="CLA generation $GENERATION"
  local check_count=1
  local run_count=1
  case "$FAKE_MODE" in
    null-head) head_repo=''; head_repo_id='' ;;
    null-unrelated) ;;
    collision) ;;
    duplicate-check) check_count=2 ;;
    duplicate-success) ;;
    duplicate-job) ;;
    stale-marker) marker='CLA generation v2.2-action-0000000000000000000000000000000000000000' ;;
    no-run|no-run-unsigned) run_count=0 ;;
    deleted-comment|deleted-comment-empty)
      run_count=0
      if [[ "$endpoint" == repos/manaflow-ai/subrouter/issues/comments/900 ]]; then
        printf 'HTTP/2 404\r\ncontent-type: application/json\r\n\r\n{"message":"Not Found","status":404}\n'
        return 1
      fi
      ;;
    deleted-comment-unsigned)
      if [[ "$endpoint" == repos/manaflow-ai/subrouter/issues/comments/900 ]]; then
        printf 'HTTP/2 404\r\ncontent-type: application/json\r\n\r\n'
        return 1
      fi
      ;;
    link-truncated)
      if [[ "$endpoint" == repos/manaflow-ai/subrouter/pulls\?* ]]; then
        printf 'HTTP/2 200\r\nLink: <https://api.github.com/repos/manaflow-ai/subrouter/pulls?page=next>; rel="next"\r\n\r\n'
        jq -nc --arg sha "$SHA" '[{number:294,state:"open",base:{ref:"main",repo:{id:100,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:$sha,repo:null}}]'
        return 0
      fi
      ;;
    oversized)
      if [[ "$endpoint" == repos/manaflow-ai/subrouter/issues/294 ]]; then
        printf '{"number":294,"state":"open","pull_request":{"url":"https://api.github.com/repos/manaflow-ai/subrouter/pulls/294"},"padding":"'
        printf '%*s' 1100000 '' | tr ' ' A
        printf '"}\n'
        return 0
      fi
      ;;
    active-run)
      if [[ "$endpoint" == repos/manaflow-ai/subrouter/actions/workflows/6002/runs\?* ]]; then
        jq -nc --arg sha "$SHA" '{workflow_runs:[{id:7999,workflow_id:6002,path:".github/workflows/cla.yml",event:"pull_request_target",status:"in_progress",conclusion:null,head_sha:$sha,head_branch:"feature",created_at:"2026-08-31T07:30:00Z",html_url:"https://github.com/manaflow-ai/subrouter/actions/runs/7999",pull_requests:[{number:294,base:{ref:"main",repo:{id:100,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:$sha}}]},{id:8001,workflow_id:6002,path:".github/workflows/cla.yml",event:"pull_request_target",status:"completed",conclusion:"failure",head_sha:$sha,head_branch:"feature",created_at:"2026-08-31T07:00:00Z",html_url:"https://github.com/manaflow-ai/subrouter/actions/runs/8001",pull_requests:[{number:294,base:{ref:"main",repo:{id:100,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:$sha}}]}]}'
        return 0
      fi
      ;;
    many-runs)
      if [[ "$endpoint" == repos/manaflow-ai/subrouter/actions/workflows/6002/runs\?* ]]; then
        page="${endpoint##*page=}"
        page="${page%%[^0-9]*}"
        [[ "$page" =~ ^[1-9][0-9]*$ ]] || return 1
        offset=$(( (page - 1) * 100 ))
        if (( page <= 10 )); then
          if (( page < 10 )); then
            printf 'HTTP/2 200\r\nLink: <https://api.github.com/repos/manaflow-ai/subrouter/actions/workflows/6002/runs?page=%d>; rel="next"\r\n\r\n' "$((page + 1))"
          else
            printf 'HTTP/2 200\r\n\r\n'
          fi
          jq -nc --arg sha "$SHA" --argjson offset "$offset" '{workflow_runs:[range(0;100) as $n | {id:(8001 + $offset + $n),workflow_id:6002,path:".github/workflows/cla.yml",event:"pull_request_target",status:"completed",conclusion:"failure",head_sha:$sha,head_branch:"feature",created_at:"2026-08-31T07:00:00Z",html_url:("https://github.com/manaflow-ai/subrouter/actions/runs/" + ((8001 + $offset + $n)|tostring)),pull_requests:[{number:294,base:{ref:"main",repo:{id:100,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:$sha}}]}]}'
          return 0
        fi
      fi
      ;;
  esac

  case "$endpoint" in
    repos/manaflow-ai/subrouter/issues/294)
      jq -nc '{number:294,state:"open",pull_request:{url:"https://api.github.com/repos/manaflow-ai/subrouter/pulls/294"}}'
      ;;
    repos/manaflow-ai/subrouter/issues/comments/900)
      jq -nc --arg body "$COMMENT_BODY" --arg created "$COMMENT_CREATED_AT" --arg login "$COMMENT_AUTHOR_LOGIN" --argjson id "$COMMENT_AUTHOR_ID" \
        '{id:900,issue_url:"https://api.github.com/repos/manaflow-ai/subrouter/issues/294",body:$body,user:{id:$id,login:$login,type:"User"},created_at:$created,updated_at:$created}'
      ;;
    repos/manaflow-ai/subrouter/pulls/294)
      if [[ "$FAKE_MODE" == null-head ]]; then
        jq -nc --arg sha "$SHA" '{number:294,state:"open",merged_at:null,user:{id:300,login:"contributor"},base:{ref:"main",repo:{id:100,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:$sha,repo:null}}'
      else
        jq -nc --arg sha "$SHA" --arg repo "$head_repo" --argjson repo_id "$head_repo_id" \
          '{number:294,state:"open",merged_at:null,user:{id:300,login:"contributor"},base:{ref:"main",repo:{id:100,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:$sha,repo:{id:$repo_id,full_name:$repo}}}'
      fi
      ;;
    repos/manaflow-ai/subrouter/pulls\?state=open*)
      if [[ "$FAKE_MODE" == collision ]]; then
        jq -nc --arg sha "$SHA" '[{number:294,state:"open",base:{ref:"main",repo:{id:100,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:$sha,repo:null}},{number:295,state:"open",base:{ref:"main",repo:{id:100,full_name:"manaflow-ai/subrouter"}},head:{ref:"other",sha:$sha,repo:null}}]'
      elif [[ "$FAKE_MODE" == null-unrelated ]]; then
        jq -nc --arg sha "$SHA" '[{number:294,state:"open",base:{ref:"main",repo:{id:100,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:$sha,repo:{id:200,full_name:"contributor/subrouter"}}},{number:295,state:"open",base:{ref:"main",repo:{id:100,full_name:"manaflow-ai/subrouter"}},head:{ref:"deleted",sha:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",repo:null}}]'
      else
        jq -nc --arg sha "$SHA" '[{number:294,state:"open",base:{ref:"main",repo:{id:100,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:$sha,repo:null}}]'
      fi
      ;;
    repos/manaflow-ai/subrouter/actions/workflows\?*)
      jq -nc '[{id:6001,path:".github/workflows/other.yml",state:"active"},{id:6002,path:".github/workflows/cla.yml",state:"active"}]' | jq '{workflows:.}'
      ;;
    repos/manaflow-ai/subrouter/actions/workflows/6002/runs\?*)
      if [[ "$run_count" == 0 ]]; then
        jq -nc '{workflow_runs:[]}'
      else
        jq -nc --arg sha "$SHA" --arg marker "$marker" \
          '{workflow_runs:[{id:8001,workflow_id:6002,path:".github/workflows/cla.yml",event:"pull_request_target",status:"completed",conclusion:"failure",head_sha:$sha,head_branch:"feature",created_at:"2026-08-31T07:00:00Z",html_url:"https://github.com/manaflow-ai/subrouter/actions/runs/8001",pull_requests:[{number:294,base:{ref:"main",repo:{id:100,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:$sha}}]}]}'
      fi
      ;;
    repos/manaflow-ai/subrouter/actions/runs/8001)
      jq -nc --arg sha "$SHA" '{id:8001,workflow_id:6002,path:".github/workflows/cla.yml",event:"pull_request_target",status:"completed",conclusion:"failure",head_sha:$sha,head_branch:"feature",created_at:"2026-08-31T07:00:00Z",html_url:"https://github.com/manaflow-ai/subrouter/actions/runs/8001",pull_requests:[{number:294,base:{ref:"main",repo:{id:100,full_name:"manaflow-ai/subrouter"}},head:{ref:"feature",sha:$sha}}]}'
      ;;
    repos/manaflow-ai/subrouter/actions/runs/8001/jobs\?*)
      if [[ "$FAKE_MODE" == duplicate-job ]]; then
        jq -nc --arg sha "$SHA" --arg marker "$marker" \
          '{jobs:[{id:9001,run_id:8001,name:"CLA Assistant v3",status:"completed",conclusion:"failure",head_sha:$sha,html_url:"https://github.com/manaflow-ai/subrouter/actions/runs/8001/job/9001",steps:[{name:$marker,status:"completed",conclusion:"success"}]},{id:9011,run_id:8001,name:"CLA Assistant v3",status:"completed",conclusion:"failure",head_sha:$sha,html_url:"https://github.com/manaflow-ai/subrouter/actions/runs/8001/job/9011",steps:[{name:$marker,status:"completed",conclusion:"success"}]},{id:9002,run_id:8001,name:"CLA ledger writer",status:"completed",conclusion:"success",head_sha:$sha,steps:[]},{id:9003,run_id:8001,name:"Validate CLA trigger",status:"completed",conclusion:"success",head_sha:$sha,steps:[]}]}'
      else
        jq -nc --arg sha "$SHA" --arg marker "$marker" \
          '{jobs:[{id:9001,run_id:8001,name:"CLA Assistant v3",status:"completed",conclusion:"failure",head_sha:$sha,html_url:"https://github.com/manaflow-ai/subrouter/actions/runs/8001/job/9001",steps:[{name:$marker,status:"completed",conclusion:"success"}]},{id:9002,run_id:8001,name:"CLA ledger writer",status:"completed",conclusion:"success",head_sha:$sha,steps:[]},{id:9003,run_id:8001,name:"Validate CLA trigger",status:"completed",conclusion:"success",head_sha:$sha,steps:[]}]}'
      fi
      ;;
    repos/manaflow-ai/subrouter/contents/signatures/version2/cla.json\?ref=cla-signatures)
      if [[ "$FAKE_MODE" == partial-sign ]]; then
        ledger_content="$(jq -nc --arg created "$COMMENT_CREATED_AT" '{signedContributors:[{name:"contributor",id:300,comment_id:900,created_at:$created,repoId:100,pullRequestNo:294}]}' | base64 | tr -d '\n')"
        jq -nc --arg content "$ledger_content" '{type:"file",encoding:"base64",content:$content}'
      else
        echo "mock has no fixture for $endpoint" >&2
        return 1
      fi
      ;;
    repos/manaflow-ai/subrouter/actions/jobs/9001)
      jq -nc --arg sha "$SHA" --arg marker "$marker" \
        '{id:9001,run_id:8001,name:"CLA Assistant v3",status:"completed",conclusion:"failure",head_sha:$sha,html_url:"https://github.com/manaflow-ai/subrouter/actions/runs/8001/job/9001",steps:[{name:$marker,status:"completed",conclusion:"success"}]}'
      ;;
    repos/manaflow-ai/subrouter/commits/*/check-runs\?*)
      if [[ "$FAKE_MODE" == duplicate-success ]]; then
        jq -nc --arg sha "$SHA" '{check_runs:[{id:7001,name:"CLA Assistant v3",head_sha:$sha,status:"completed",conclusion:"failure",app:{id:15368,slug:"github-actions"},details_url:"https://github.com/manaflow-ai/subrouter/actions/runs/8001/job/9001"},{id:7002,name:"CLA Assistant v3",head_sha:$sha,status:"completed",conclusion:"success",app:{id:15368,slug:"github-actions"},details_url:"https://github.com/manaflow-ai/subrouter/actions/runs/8001/job/9011"}]}'
      else
        jq -nc --arg sha "$SHA" --argjson count "$check_count" '
          {check_runs: [range(0;$count) | {id:(7001 + .),name:"CLA Assistant v3",head_sha:$sha,status:"completed",conclusion:"failure",app:{id:15368,slug:"github-actions"},details_url:"https://github.com/manaflow-ai/subrouter/actions/runs/8001/job/9001"}]}
        '
      fi
      ;;
    repos/manaflow-ai/subrouter/check-runs/7001)
      jq -nc --arg sha "$SHA" '{id:7001,name:"CLA Assistant v3",head_sha:$sha,status:"completed",conclusion:"failure",app:{id:15368,slug:"github-actions"},details_url:"https://github.com/manaflow-ai/subrouter/actions/runs/8001/job/9001"}'
      ;;
    *)
      echo "mock has no fixture for $endpoint" >&2
      return 1
      ;;
  esac
}

# Export one stable command shim. The helper invokes `gh` from a bounded child
# shell, so exporting the shim and its fixture function keeps every API call
# on the same deterministic mock path.
gh() { fake_gh "$@"; }
export -f fake_gh gh

run_case() {
  local mode="$1"
  local expected="$2"
  local work status
  work="$(mktemp -d)"
  export FAKE_MODE="$mode"
  export FAKE_POST_FILE="$work/posts"
  : >"$FAKE_POST_FILE"
  export GH_REPO=manaflow-ai/subrouter EVENT_NAME=issue_comment ISSUE_NUMBER=294 PR_NUMBER=294
  export COMMENT_ID=900 COMMENT_BODY=recheck COMMENT_CREATED_AT=2026-08-31T08:00:00Z
  export COMMENT_AUTHOR_ID=300 COMMENT_AUTHOR_LOGIN=contributor COMMENT_AUTHOR_TYPE=User
  export COMMENT_AUTHOR_ASSOCIATION=NONE WORKFLOW_PATH=.github/workflows/cla.yml WORKFLOW_SHA
  export CLA_GENERATION="$GENERATION" TARGET_EVENT=pull_request_target TARGET_BASE_REF=main
  export SIGNATURE_RECORDED=false
  export WRITER_POLICY_RESULT=true
  export WRITER_RESULT=success
  if [[ "$mode" == partial-sign ]]; then
    export COMMENT_BODY='I have read the CLA Document v2.2 and I hereby sign the CLA'
    export SIGNATURE_RECORDED=true
    export WRITER_POLICY_RESULT=false
  fi
  if [[ "$mode" == no-run-unsigned ]]; then
    export WRITER_POLICY_RESULT=false
  fi
  if [[ "$mode" == deleted-comment-unsigned ]]; then
    export WRITER_POLICY_RESULT=false
  fi
  set +e
  (cd "$ROOT_DIR" && bash "$SCRIPT") >"$work/output" 2>&1
  status="$?"
  set -e
  if [[ "$expected" == pass ]]; then
    [[ "$status" == 0 ]] || { cat "$work/output" >&2; echo "case $mode failed" >&2; return 1; }
    if [[ "$mode" == no-run || "$mode" == no-run-unsigned || "$mode" == deleted-comment || "$mode" == deleted-comment-empty ]]; then
      [[ ! -s "$FAKE_POST_FILE" ]] || { echo "case $mode unexpectedly posted" >&2; return 1; }
    else
      grep -Fq '/actions/runs/8001/rerun' "$FAKE_POST_FILE" || { cat "$work/output" >&2; echo "case $mode did not post full rerun" >&2; return 1; }
    fi
  else
    [[ "$status" != 0 ]] || { cat "$work/output" >&2; echo "case $mode unexpectedly passed" >&2; return 1; }
    [[ ! -s "$FAKE_POST_FILE" ]] || { cat "$work/output" >&2; echo "case $mode posted after rejection" >&2; return 1; }
  fi
  rm -rf "$work"
}

run_case valid pass
run_case null-head fail
run_case null-unrelated pass
run_case no-run pass
run_case no-run-unsigned fail
run_case deleted-comment pass
run_case deleted-comment-empty pass
run_case deleted-comment-unsigned pass
run_case partial-sign pass
run_case active-run pass
run_case many-runs fail
run_case collision fail
run_case duplicate-check fail
run_case duplicate-success fail
run_case duplicate-job fail
run_case stale-marker fail
run_case link-truncated fail
run_case oversized fail
echo "CLA rerun behavior tests passed"
