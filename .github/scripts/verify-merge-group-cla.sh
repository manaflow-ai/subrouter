#!/usr/bin/env bash
# Verify, for one merge queue entry, that the queued pull request's live head
# carries a passing native `CLA Assistant v3` check from cla.yml.
#
# cla.yml runs on pull_request_target, so it never reports on a merge group
# commit, and the merge queue waits for the required check on that commit.
# This helper is the merge-group producer of the same context. It never
# decides CLA status itself: it only re-reads the verdict that the native
# pull_request_target job published on the pull request head, and fails
# closed on anything missing, pending past the wait budget, ambiguous, or
# produced by anything other than cla.yml.
#
# The workflow checks this file out at the merge group's base SHA (a commit
# already on main), so a queued pull request cannot swap in its own verifier.
set -euo pipefail

readonly EXPECTED_REPOSITORY='manaflow-ai/subrouter'
readonly EXPECTED_WORKFLOW_PATH='.github/workflows/cla.yml'
readonly EXPECTED_ASSISTANT_JOB='CLA Assistant v3'
readonly EXPECTED_APP_ID=15368
readonly EXPECTED_APP_SLUG='github-actions'
readonly EXPECTED_GENERATION='v2.2-action-212a0f2dd659b24b48a30ba35966e06dc41736af'
readonly EXPECTED_EVENT='pull_request_target'
readonly EXPECTED_BASE_REF='main'
readonly MAX_PAGE_BYTES=1048576
readonly MAX_TOTAL_BYTES=20000000
readonly MAX_PAGES=10
readonly MAX_NATIVE_CHECKS=50
# An `edited` or `ready_for_review` event on a queued pull request starts a
# fresh native run on the same head. Wait for it rather than failing the
# entry, but never longer than the queue's own check timeout.
readonly MAX_WAIT_ATTEMPTS="${CLA_WAIT_ATTEMPTS:-15}"
readonly WAIT_SECONDS="${CLA_WAIT_SECONDS:-20}"

fail() {
  echo "::error title=CLA merge queue policy::${1}" >&2
  exit 1
}

is_safe_id() {
  local value="${1:-}"
  [[ "$value" =~ ^[1-9][0-9]*$ ]] || return 1
  (( ${#value} <= 16 )) || return 1
  if (( ${#value} == 16 )); then
    (( value <= 9007199254740991 )) || return 1
  fi
}

is_sha() {
  [[ "${1:-}" =~ ^[0-9a-f]{40}$ ]]
}

# Bounded API reader, same contract as rerun-failed-cla.sh: the complete
# response lands in a size-capped file before jq sees it, and pagination is
# detected from the Link header rather than inferred from page length alone.
api_total_bytes=0
api_file="$(mktemp)"
trap 'rm -f -- "$api_file" "${pages_file:-}"' EXIT
api_body=''
api_has_next=false
api_get() {
  local endpoint="$1" raw_bytes http_status first_line has_headers=false producer_status
  : >"$api_file"
  set +e
  (
    ulimit -f 2049
    gh api --method GET \
      --header 'Accept: application/vnd.github+json' \
      --header 'X-GitHub-Api-Version: 2022-11-28' \
      --include "$endpoint" >"$api_file" 2>/dev/null
  )
  producer_status="$?"
  set -e
  raw_bytes="$(LC_ALL=C wc -c <"$api_file" | tr -d '[:space:]')"
  [[ "$raw_bytes" =~ ^[0-9]+$ ]] || return 1
  (( raw_bytes <= MAX_PAGE_BYTES )) || return 2
  api_total_bytes=$((api_total_bytes + raw_bytes))
  (( api_total_bytes <= MAX_TOTAL_BYTES )) || return 2
  IFS= read -r first_line <"$api_file" || true
  if [[ "$first_line" =~ ^HTTP/[0-9.]+[[:space:]]+[0-9]{3}([[:space:]]|$) ]]; then
    has_headers=true
    http_status="$(awk '/^HTTP\// { gsub("\r", "", $2); code=$2 } END { print code }' "$api_file")"
    api_body="$(awk 'BEGIN { body=0 } { sub(/\r$/, "") } body { print; next } /^$/ { body=1 }' "$api_file")"
  else
    http_status=200
    api_body="$(<"$api_file")"
  fi
  [[ "$http_status" =~ ^[0-9]{3}$ ]] || return 1
  api_has_next=false
  if [[ "$has_headers" == true ]] && awk '
    BEGIN { in_headers=1; found=0 }
    in_headers {
      line=$0; sub(/\r$/, "", line)
      if (line == "") { in_headers=0; next }
      lower=tolower(line)
      if (lower ~ /^link:[[:space:]]/ && lower ~ /rel="?next"?/) found=1
    }
    END { exit(found ? 0 : 1) }
  ' "$api_file"; then
    api_has_next=true
  fi
  (( producer_status == 0 )) || return 1
  [[ "$http_status" =~ ^2[0-9][0-9]$ ]]
}

require_inputs() {
  [[ "${GH_REPO:-}" == "$EXPECTED_REPOSITORY" ]] || fail "The helper is not running in the canonical repository."
  [[ "${EVENT_NAME:-}" == merge_group ]] || fail "The helper received an unexpected event."
  [[ "${MERGE_GROUP_BASE_REF:-}" == "refs/heads/${EXPECTED_BASE_REF}" ]] || fail "The merge group does not target ${EXPECTED_BASE_REF}."
  is_sha "${MERGE_GROUP_BASE_SHA:-}" || fail "The merge group base SHA is invalid."
  is_sha "${MERGE_GROUP_HEAD_SHA:-}" || fail "The merge group head SHA is invalid."
  [[ "${CLA_GENERATION:-}" == "$EXPECTED_GENERATION" ]] || fail "The CLA generation is not the reviewed action release."
  # GitHub names every queue entry ref after the pull request it adds.
  local ref_pattern="^refs/heads/gh-readonly-queue/${EXPECTED_BASE_REF}/pr-([1-9][0-9]{0,15})-[0-9a-f]{40}\$"
  [[ "${MERGE_GROUP_HEAD_REF:-}" =~ $ref_pattern ]] || fail "The merge group ref does not name exactly one queued pull request."
  PR_NUMBER="${BASH_REMATCH[1]}"
  is_safe_id "$PR_NUMBER" || fail "The queued pull request number is unsafe."
  [[ "$WAIT_SECONDS" =~ ^[0-9]+$ && "$MAX_WAIT_ATTEMPTS" =~ ^[1-9][0-9]*$ ]] || fail "The wait budget is invalid."
  local checked_out_sha
  checked_out_sha="$(git rev-parse HEAD 2>/dev/null)" || fail "The trusted checkout cannot be verified."
  [[ "$checked_out_sha" == "${TRUSTED_SHA:-}" ]] || fail "The verifier is not checked out at the trusted base revision."
}

read_pr() {
  api_get "repos/${GH_REPO}/pulls/${PR_NUMBER}" || fail "Could not read the queued pull request."
  jq -e --arg repo "${GH_REPO}" --arg pr "${PR_NUMBER}" --arg base "$EXPECTED_BASE_REF" '
    def safe_id: type == "number" and floor == . and . > 0 and . <= 9007199254740991;
    type == "object" and
    (.number | safe_id and (tostring == $pr)) and .state == "open" and .merged_at == null and
    (.base | type == "object") and .base.ref == $base and
    (.base.repo | type == "object") and .base.repo.full_name == $repo and (.base.repo.id | safe_id) and
    (.head | type == "object") and (.head.sha | type == "string" and test("^[0-9a-f]{40}$"))
  ' <<<"$api_body" >/dev/null 2>&1 || fail "The queued pull request is not an open pull request into ${EXPECTED_BASE_REF}."
  HEAD_SHA="$(jq -er '.head.sha | strings' <<<"$api_body")"
}

list_head_checks() {
  pages_file="$(mktemp)"
  local page count
  for ((page = 1; page <= MAX_PAGES; page++)); do
    api_get "repos/${GH_REPO}/commits/${HEAD_SHA}/check-runs?filter=all&per_page=100&page=${page}" ||
      fail "Could not enumerate checks for the queued pull request head."
    jq -e '
      def safe_id: type == "number" and floor == . and . > 0 and . <= 9007199254740991;
      type == "object" and (.check_runs | type == "array" and length <= 100) and all(.check_runs[];
        type == "object" and (.id | safe_id) and
        (.name | type == "string" and length > 0 and test("^[^\\r\\n]+$")) and
        (.head_sha | type == "string") and
        (.status | type == "string" and length > 0) and
        (.conclusion == null or (.conclusion | type == "string")) and
        (.app | type == "object" and (.id | safe_id) and (.slug | type == "string")) and
        (.details_url == null or (.details_url | type == "string")))
    ' <<<"$api_body" >/dev/null 2>&1 || fail "The check-run response is malformed."
    count="$(jq -er '.check_runs | length' <<<"$api_body")"
    printf '%s\n' "$api_body" >>"$pages_file"
    if (( count < 100 )) && [[ "$api_has_next" != true ]]; then break; fi
    (( page < MAX_PAGES )) || fail "The check-run result window is truncated."
  done
  CHECKS_JSON="$(jq -s --arg name "$EXPECTED_ASSISTANT_JOB" '
    [.[].check_runs[] | select((.name | ascii_downcase) == ($name | ascii_downcase))] | sort_by(.id)
  ' "$pages_file")" || fail "Could not combine check-run pages."
  rm -f -- "$pages_file"
}

# Every same-name check on the head must be a native cla.yml job. A second
# producer (another workflow, another app, a case-folded name) would make the
# required context ambiguous, so it fails the entry even when it is green.
validate_native_check() {
  local check_json="$1" check_id url run_id job_id url_pattern
  check_id="$(jq -er '.id' <<<"$check_json")"
  jq -e --arg name "$EXPECTED_ASSISTANT_JOB" --arg sha "$HEAD_SHA" \
    --argjson app_id "$EXPECTED_APP_ID" --arg slug "$EXPECTED_APP_SLUG" '
    .name == $name and .head_sha == $sha and .app.id == $app_id and .app.slug == $slug
  ' <<<"$check_json" >/dev/null 2>&1 || fail "Check ${check_id} reuses the CLA context but is not the native GitHub Actions check."
  url="$(jq -r '.details_url // ""' <<<"$check_json")"
  url_pattern="^https://github\\.com/${GH_REPO}/actions/runs/([1-9][0-9]{0,15})/job/([1-9][0-9]{0,15})\$"
  [[ "$url" =~ $url_pattern ]] || fail "Check ${check_id} does not link to a GitHub Actions job."
  run_id="${BASH_REMATCH[1]}"
  job_id="${BASH_REMATCH[2]}"

  api_get "repos/${GH_REPO}/actions/runs/${run_id}" || fail "Could not read workflow run ${run_id}."
  jq -e --arg id "$run_id" --arg path "$EXPECTED_WORKFLOW_PATH" --arg event "$EXPECTED_EVENT" \
    --arg sha "$HEAD_SHA" --arg base "$EXPECTED_BASE_REF" --argjson pr "$PR_NUMBER" '
    type == "object" and (.id | tostring) == $id and .path == $path and .event == $event and
    .head_sha == $sha and
    (.pull_requests == null or (.pull_requests | type == "array")) and
    (if (.pull_requests // []) | length == 0 then true
     else any(.pull_requests[]; .number == $pr and .base.ref == $base) end)
  ' <<<"$api_body" >/dev/null 2>&1 || fail "Check ${check_id} was not produced by ${EXPECTED_WORKFLOW_PATH} on ${EXPECTED_EVENT} for this pull request head."

  api_get "repos/${GH_REPO}/actions/jobs/${job_id}" || fail "Could not read workflow job ${job_id}."
  JOB_JSON="$api_body"
  jq -e --arg id "$job_id" --arg run_id "$run_id" --arg name "$EXPECTED_ASSISTANT_JOB" --arg sha "$HEAD_SHA" '
    type == "object" and (.id | tostring) == $id and (.run_id | tostring) == $run_id and
    .name == $name and .head_sha == $sha and (.steps | type == "array")
  ' <<<"$JOB_JSON" >/dev/null 2>&1 || fail "Check ${check_id} does not match its native CLA job."
}

require_inputs
read_pr
echo "Merge group ${MERGE_GROUP_HEAD_SHA} queues pull request #${PR_NUMBER} at head ${HEAD_SHA}."

for ((attempt = 1; ; attempt++)); do
  list_head_checks
  total="$(jq -r 'length' <<<"$CHECKS_JSON")"
  [[ "$total" =~ ^[0-9]+$ ]] || fail "Could not count CLA checks."
  (( total > 0 )) || fail "The queued pull request head has no ${EXPECTED_ASSISTANT_JOB} check."
  (( total <= MAX_NATIVE_CHECKS )) || fail "The queued pull request head has more than ${MAX_NATIVE_CHECKS} CLA checks."
  latest="$(jq -c '.[-1]' <<<"$CHECKS_JSON")"
  if [[ "$(jq -r '.status' <<<"$latest")" == completed ]]; then break; fi
  (( attempt < MAX_WAIT_ATTEMPTS )) || fail "The newest native CLA check is still running after the wait budget."
  echo "Newest native CLA check is $(jq -r '.status' <<<"$latest"); waiting ${WAIT_SECONDS}s."
  sleep "$WAIT_SECONDS"
done

while IFS= read -r check; do
  validate_native_check "$check"
done < <(jq -c '.[]' <<<"$CHECKS_JSON")

# The newest native check is the one GitHub evaluates for the required
# context. It must be a completed success from the current policy generation:
# without the strict up-to-date rule, a head validated before a generation
# bump would otherwise reach main without re-running the new policy.
validate_native_check "$latest"
jq -e '.status == "completed" and .conclusion == "success"' <<<"$latest" >/dev/null 2>&1 ||
  fail "The newest native CLA check on the queued pull request head did not pass."
jq -e '.status == "completed" and .conclusion == "success"' <<<"$JOB_JSON" >/dev/null 2>&1 ||
  fail "The newest native CLA job did not pass."
jq -e --arg marker "CLA generation ${EXPECTED_GENERATION}" '
  [.steps[] | select(.name == $marker and .status == "completed" and .conclusion == "success")] | length == 1
' <<<"$JOB_JSON" >/dev/null 2>&1 ||
  fail "The newest native CLA check ran an older policy generation; push or reopen the pull request to re-run it."

# The head must not have moved while this verifier ran.
queued_head="$HEAD_SHA"
read_pr
[[ "$HEAD_SHA" == "$queued_head" ]] || fail "The queued pull request head changed during verification."

echo "CLA Assistant v3 passed for queued pull request #${PR_NUMBER} at head ${HEAD_SHA}."
