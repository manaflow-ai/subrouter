#!/usr/bin/env bash
# Re-run the native CLA policy job after an authenticated CLA issue comment.
# The workflow checks this file out at an immutable workflow SHA.
set -euo pipefail

readonly SIGN_PHRASE='I have read the CLA Document v2.2 and I hereby sign the CLA'
readonly EXPECTED_REPOSITORY='manaflow-ai/subrouter'
readonly EXPECTED_WORKFLOW_PATH='.github/workflows/cla.yml'
readonly EXPECTED_ASSISTANT_JOB='CLA Assistant v3'
readonly EXPECTED_WRITER_JOB='CLA ledger writer'
readonly EXPECTED_GATE_JOB='Validate CLA trigger'
readonly EXPECTED_APP_ID=15368
readonly EXPECTED_APP_SLUG='github-actions'
readonly EXPECTED_GENERATION='v2.2-action-212a0f2dd659b24b48a30ba35966e06dc41736af'
readonly SIGNATURES_BRANCH='cla-signatures'
readonly SIGNATURES_PATH='signatures/version2/cla.json'
readonly MAX_PAGE_BYTES=1048576
readonly MAX_TOTAL_BYTES=10000000
readonly MAX_PAGES=10
readonly MAX_MATCHING_RUNS=100
readonly MAX_FALLBACK_RUNS=1000
readonly MAX_LEDGER_BYTES=1000000
readonly MAX_LEDGER_SIGNATURES=10000

fail() {
  echo "::error title=CLA rerun policy::${1}" >&2
  exit 1
}

no_op() {
  [[ "${WRITER_RESULT:-}" == success && "${WRITER_POLICY_RESULT:-}" == true ]] ||
    fail "A no-op rerun decision requires the writer's explicit all-signed policy result."
  echo "CLA rerun not needed: ${1}"
  exit 0
}

is_safe_id() {
  local value="${1:-}"
  [[ "$value" =~ ^[1-9][0-9]*$ ]] || return 1
  (( $(printf '%s' "$value" | wc -c) <= 16 )) || return 1
  if (( $(printf '%s' "$value" | wc -c) == 16 )); then
    (( value <= 9007199254740991 )) || return 1
  fi
}

is_sha() {
  [[ "${1:-}" =~ ^[0-9a-fA-F]{40}$ ]]
}

is_timestamp() {
  [[ "${1:-}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]]
}

is_safe_text() {
  local value="${1:-}"
  [[ -n "$value" && "$value" != *$'\n'* && "$value" != *$'\r'* ]]
}

# Capture the complete API response in a bounded file before jq sees it.
# ulimit -f prevents a maliciously large response from filling the runner.
api_total_bytes=0
declare -a CLA_TEMP_FILES=()
cleanup_temp_files() {
  local file
  for file in "${CLA_TEMP_FILES[@]}"; do
    rm -f -- "$file"
  done
}
api_file="$(mktemp)"
CLA_TEMP_FILES+=("$api_file")
trap cleanup_temp_files EXIT
api_http_status=''
api_body=''
api_has_next=false
api_request() {
  local method="$1"
  local endpoint="$2"
  local raw_bytes http_status response_body first_line has_headers=false
  case "$method" in
    GET|POST) ;;
    *) return 1 ;;
  esac
  : >"$api_file"
  set +e
  (
    ulimit -f 2049
    gh api --method "$method" \
      --header 'Accept: application/vnd.github+json' \
      --header 'X-GitHub-Api-Version: 2022-11-28' \
      --include "$endpoint" >"$api_file" 2>/dev/null
  )
  local producer_status="$?"
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
    response_body="$(awk '
      BEGIN { body=0 }
      { sub(/\r$/, "") }
      body { print; next }
      /^$/ { body=1 }
    ' "$api_file")"
  else
    http_status=200
    response_body="$(<"$api_file")"
  fi
  [[ "$http_status" =~ ^[0-9]{3}$ ]] || return 1
  api_http_status="$http_status"
  api_body="$response_body"
  api_has_next=false
  if [[ "$has_headers" == true ]] && awk '
    BEGIN { in_headers=1; found=0 }
    in_headers {
      line=$0
      sub(/\r$/, "", line)
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

api_get() { api_request GET "$1"; }
api_post() { api_request POST "$1"; }

is_valid_not_found() {
  [[ "$api_http_status" == 404 ]] || return 1
  # GitHub can return an empty body for a validated 404, especially when the
  # resource was deleted between the event and this read. Accept that wire
  # shape, and accept a JSON error only when its status and message are
  # internally consistent. Any other body stays an infrastructure error.
  if [[ -z "${api_body//[[:space:]]/}" ]]; then
    return 0
  fi
  jq -e 'type == "object" and ((.status == 404) or (.status == "404")) and (.message | type == "string" and length > 0)' \
    <<<"$api_body" >/dev/null 2>&1
}

require_inputs() {
  [[ "${GH_REPO:-}" == "$EXPECTED_REPOSITORY" ]] || fail "The helper is not running in the canonical repository."
  [[ "${EVENT_NAME:-}" == issue_comment ]] || fail "The helper received an unexpected event."
  # A rerun of the issue-comment workflow reuses its original event payload.
  # Refuse every attempt after the first so the rerun worker cannot schedule a
  # new rerun of itself indefinitely.
  [[ "${RUN_ATTEMPT:-}" == 1 ]] || fail "The issue-comment rerun helper only runs on the first workflow attempt."
  is_safe_id "${ISSUE_NUMBER:-}" || fail "The issue number is invalid."
  [[ "${PR_NUMBER:-}" == "${ISSUE_NUMBER:-}" ]] || fail "The issue and pull request numbers differ."
  is_safe_id "${COMMENT_ID:-}" || fail "The comment ID is invalid."
  is_safe_id "${COMMENT_AUTHOR_ID:-}" || fail "The comment author ID is invalid."
  is_timestamp "${COMMENT_CREATED_AT:-}" || fail "The comment timestamp is invalid."
  is_safe_text "${COMMENT_AUTHOR_LOGIN:-}" || fail "The comment author login is invalid."
  [[ "${COMMENT_AUTHOR_TYPE:-}" == User ]] || fail "The comment author is not a human user."
  [[ "${COMMENT_AUTHOR_LOGIN,,}" != *"[bot]" ]] || fail "Bot comments cannot trigger a CLA rerun."
  case "${COMMENT_AUTHOR_ASSOCIATION:-}" in
    OWNER|MEMBER|COLLABORATOR|CONTRIBUTOR|FIRST_TIME_CONTRIBUTOR|FIRST_TIMER|NONE) ;;
    *) fail "The comment author association is invalid." ;;
  esac
  case "${COMMENT_BODY:-}" in
    "$SIGN_PHRASE"|recheck) ;;
    *) fail "The comment is not an accepted CLA command." ;;
  esac
  case "${SIGNATURE_RECORDED:-}" in
    true|false|'') ;;
    *) fail "The writer returned an invalid signature result." ;;
  esac
  case "${WRITER_POLICY_RESULT:-}" in
    true|false) ;;
    *) fail "The writer returned an invalid CLA policy result." ;;
  esac
  case "${COMMENT_BODY:-}" in
    "$SIGN_PHRASE")
      [[ "${WRITER_RESULT:-}" == success ]] || fail "The writer job did not complete successfully."
      [[ "${WRITER_POLICY_RESULT:-}" == true ]] || fail "The writer did not confirm an all-signed policy."
      ;;
    recheck)
      [[ "${RECHECK_DECISION:-}" == authorized ]] || fail "The recheck was not authorized by the live read-only gate."
      case "${WRITER_RESULT:-}" in
        success|failure|cancelled|skipped) ;;
        *) fail "The writer result is invalid for an authorized recheck." ;;
      esac
      ;;
  esac
  [[ "${CLA_GENERATION:-}" == "$EXPECTED_GENERATION" ]] || fail "The CLA generation is not the reviewed action release."
  is_sha "${WORKFLOW_SHA:-}" || fail "The trusted workflow revision is invalid."
  [[ "${WORKFLOW_PATH:-}" == "$EXPECTED_WORKFLOW_PATH" ]] || fail "The trusted workflow path is invalid."
  [[ "${TARGET_EVENT:-}" == pull_request_target && "${TARGET_BASE_REF:-}" == main ]] ||
    fail "The target workflow contract is invalid."
  checked_out_sha="$(git rev-parse HEAD 2>/dev/null)" || fail "The trusted checkout cannot be verified."
  [[ "$checked_out_sha" == "${WORKFLOW_SHA:-}" ]] || fail "The checkout is not the immutable workflow revision."
  [[ -f .github/scripts/rerun-failed-cla.sh ]] || fail "The trusted rerun helper is missing."
}

read_issue() {
  api_get "repos/${GH_REPO}/issues/${PR_NUMBER}" || fail "Could not read the pull request issue."
  local issue_json="$api_body"
  jq -e --arg repo "${GH_REPO}" --arg pr "${PR_NUMBER}" '
    type == "object" and .state == "open" and
    (.number | type == "number" and floor == . and . > 0 and (tostring == $pr)) and
    (.pull_request | type == "object") and
    .pull_request.url == ("https://api.github.com/repos/" + $repo + "/pulls/" + $pr)
  ' <<<"$issue_json" >/dev/null 2>&1 || fail "The issue is not the exact open pull request."
}

read_comment() {
  if ! api_get "repos/${GH_REPO}/issues/comments/${COMMENT_ID}"; then
    if is_valid_not_found; then
      # Deletion is a validated race, not an infrastructure error. Defer the
      # no-op decision until after the collision and run scans. A writer that
      # did not prove all-signed must still rerun the native check, even when
      # its original comment disappeared.
      echo "The authenticated comment was deleted after the writer completed."
      return 0
    fi
    fail "Could not read the authenticated comment."
  fi
  local comment_json="$api_body"
  local issue_url="https://api.github.com/repos/${GH_REPO}/issues/${PR_NUMBER}"
  # Validate the response shape separately from the immutable snapshot
  # comparison. This keeps malformed or partial API data fail-closed while a
  # genuine edit is classified as the same bounded race as a validated 404.
  jq -e '
    type == "object" and
    (.id | type == "number" and floor == . and . > 0) and
    (.issue_url | type == "string" and length > 0) and
    (.body | type == "string") and
    (.user | type == "object") and
    (.user.id | type == "number" and floor == . and . > 0) and
    (.user.login | type == "string" and length > 0) and
    (.user.type | type == "string" and length > 0) and
    (.created_at | type == "string" and length > 0) and
    (.updated_at | type == "string" and length > 0)
  ' <<<"$comment_json" >/dev/null 2>&1 || fail "The authenticated comment response is malformed."
  if jq -e --arg issue_url "$issue_url" \
    --arg body "${COMMENT_BODY}" --arg author_id "${COMMENT_AUTHOR_ID}" \
    --arg author_login "${COMMENT_AUTHOR_LOGIN}" --arg author_type "${COMMENT_AUTHOR_TYPE}" \
    --arg created_at "${COMMENT_CREATED_AT}" --arg comment_id "${COMMENT_ID}" '
      (.id | tostring) == $comment_id and
      .issue_url == $issue_url and .body == $body and
      (.user.id | tostring) == $author_id and
      (.user.login | ascii_downcase) == ($author_login | ascii_downcase) and
      .user.type == $author_type and .created_at == $created_at and .updated_at == .created_at
    ' <<<"$comment_json" >/dev/null 2>&1; then
    return 0
  fi
  # The immutable event snapshot changed after the writer ran. Continue to
  # the exact-head collision/run checks so a false writer policy cannot leave
  # an inherited green required check untouched. A true all-signed policy may
  # safely no-op later when no failed native run exists.
  echo "The authenticated comment changed after the writer completed."
}

read_pr() {
  api_get "repos/${GH_REPO}/pulls/${PR_NUMBER}" || fail "Could not read the live pull request."
  PR_JSON="$api_body"
  jq -e --arg repo "${GH_REPO}" --arg pr "${PR_NUMBER}" --arg base "${TARGET_BASE_REF}" '
    def safe_id: type == "number" and floor == . and . > 0 and . <= 9007199254740991;
    type == "object" and
    (.number | safe_id and (tostring == $pr)) and .state == "open" and .merged_at == null and
    (.base | type == "object") and .base.ref == $base and
    (.base.repo | type == "object") and .base.repo.full_name == $repo and (.base.repo.id | safe_id) and
    (.head | type == "object") and
    (.head.ref | type == "string" and length > 0 and test("^[^\\r\\n]+$")) and
    (.head.sha | type == "string" and test("^[0-9a-fA-F]{40}$")) and
    (.head.repo | type == "object") and
    (.head.repo.full_name | type == "string" and test("^[^/[:space:]]+/[^/[:space:]]+$")) and
    (.head.repo.id | safe_id) and
    (.user | type == "object") and (.user.id | safe_id) and
    (.user.login | type == "string" and test("^[A-Za-z0-9][A-Za-z0-9-]{0,38}$"))
  ' <<<"$PR_JSON" >/dev/null 2>&1 || fail "The live pull request identity is invalid."
  HEAD_SHA="$(jq -er '.head.sha | strings' <<<"$PR_JSON")"
  HEAD_REF="$(jq -er '.head.ref | strings' <<<"$PR_JSON")"
  BASE_REPO_ID="$(jq -er '.base.repo.id | numbers | tostring' <<<"$PR_JSON")"
  PR_AUTHOR_ID="$(jq -er '.user.id | numbers | tostring' <<<"$PR_JSON")"
  HEAD_REPO="$(jq -r 'if .head.repo == null then "" else (.head.repo.full_name // "") end' <<<"$PR_JSON")"
  HEAD_REPO_ID="$(jq -r 'if .head.repo == null then "" else (.head.repo.id // "") end' <<<"$PR_JSON")"
  if [[ -n "$HEAD_REPO" || -n "$HEAD_REPO_ID" ]]; then
    [[ "$HEAD_REPO" =~ ^[^/[:space:]]+/[^/[:space:]]+$ ]] || fail "The live head repository name is invalid."
    is_safe_id "$HEAD_REPO_ID" || fail "The live head repository ID is invalid."
  fi
  if [[ "${COMMENT_BODY}" == recheck && "${COMMENT_AUTHOR_ID}" != "$PR_AUTHOR_ID" ]]; then
    case "${COMMENT_AUTHOR_ASSOCIATION}" in
      OWNER|MEMBER|COLLABORATOR) ;;
      *) fail "Only the pull request author or a trusted repository participant may request a rerun." ;;
    esac
  fi
}

validate_signature() {
  [[ "${COMMENT_BODY}" == "$SIGN_PHRASE" ]] || return 0
  api_get "repos/${GH_REPO}/contents/$SIGNATURES_PATH?ref=$SIGNATURES_BRANCH" ||
    fail "Could not read the protected signature ledger."
  local response="$api_body" content compact decoded encoded decoded_size
  jq -e 'type == "object" and .type == "file" and .encoding == "base64" and (.content | type == "string")' \
    <<<"$response" >/dev/null 2>&1 || fail "The signature ledger response is malformed."
  content="$(jq -er '.content | strings' <<<"$response")" || fail "The signature ledger content is invalid."
  compact="$(printf '%s' "$content" | tr -d '[:space:]')"
  [[ "$compact" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] || fail "The signature ledger is not valid base64."
  encoded="$(printf '%s' "$compact" | wc -c | tr -d '[:space:]')"
  if ! [[ "$encoded" =~ ^[0-9]+$ ]] || (( encoded % 4 != 0 )); then
    fail "The signature ledger encoding is invalid."
  fi
  (( encoded <= ((MAX_LEDGER_BYTES + 2) * 4 / 3 + 4) )) || fail "The signature ledger exceeds its size limit."
  if base64 --decode </dev/null >/dev/null 2>&1; then
    decoded="$(printf '%s' "$compact" | base64 --decode 2>/dev/null)" || fail "The signature ledger is not valid base64."
  else
    decoded="$(printf '%s' "$compact" | base64 -D 2>/dev/null)" || fail "The signature ledger is not valid base64."
  fi
  decoded_size="$(printf '%s' "$decoded" | wc -c | tr -d '[:space:]')"
  if ! [[ "$decoded_size" =~ ^[0-9]+$ ]] || (( decoded_size > MAX_LEDGER_BYTES )); then
    fail "The signature ledger exceeds its size limit."
  fi
  jq -e --arg login "${COMMENT_AUTHOR_LOGIN}" --arg author_id "${COMMENT_AUTHOR_ID}" \
    --arg comment_id "${COMMENT_ID}" --arg created_at "${COMMENT_CREATED_AT}" \
    --arg repo_id "$BASE_REPO_ID" --arg pr "${PR_NUMBER}" --argjson max "$MAX_LEDGER_SIGNATURES" '
      type == "object" and (.signedContributors | type == "array" and length <= $max) and
      any(.signedContributors[]?;
        (.name | type == "string" and (ascii_downcase == ($login | ascii_downcase))) and
        (.id | type == "number" and floor == . and . > 0 and . <= 9007199254740991 and tostring == $author_id) and
        (.comment_id | type == "number" and floor == . and . > 0 and . <= 9007199254740991 and tostring == $comment_id) and
        (.created_at | type == "string" and . == $created_at) and
        (.repoId | type == "number" and floor == . and . > 0 and . <= 9007199254740991 and tostring == $repo_id) and
        (.pullRequestNo | type == "number" and floor == . and . > 0 and . <= 9007199254740991 and tostring == $pr)
      )
    ' <<<"$decoded" >/dev/null 2>&1 || fail "The authenticated signature was not persisted by the CLA action."
}

validate_unique_head() {
  local pages_file
  pages_file="$(mktemp)"
  CLA_TEMP_FILES+=("$pages_file")
  : >"$pages_file"
  local page count
  for ((page = 1; page <= MAX_PAGES; page++)); do
    api_get "repos/${GH_REPO}/pulls?state=open&base=${TARGET_BASE_REF}&per_page=100&page=$page" ||
      fail "Could not enumerate open pull requests."
    local page_json="$api_body"
    jq -e '
      def safe_id: type == "number" and floor == . and . > 0 and . <= 9007199254740991;
      def safe_ref: type == "string" and length > 0 and test("^[^\\r\\n]+$");
      def safe_sha: type == "string" and test("^[0-9a-fA-F]{40}$");
      type == "array" and length <= 100 and all(.[];
        type == "object" and (.number | safe_id) and .state == "open" and
        (.base | type == "object") and (.base.ref | safe_ref) and
        (.base.repo | type == "object") and (.base.repo.full_name | type == "string" and length > 0) and (.base.repo.id | safe_id) and
        (.head | type == "object") and (.head.ref | safe_ref) and (.head.sha | safe_sha)
      )
    ' \
      <<<"$page_json" >/dev/null 2>&1 || fail "The open pull request response is malformed."
    count="$(jq -er 'length | numbers' <<<"$page_json")"
    printf '%s\n' "$page_json" >>"$pages_file"
    if (( count < 100 )) && [[ "$api_has_next" != true ]]; then break; fi
    (( page < MAX_PAGES )) || fail "The open pull request result window is truncated."
  done
  local all_open matching current_count sibling_count
  all_open="$(jq -s 'add' "$pages_file")" || fail "Could not combine open pull request pages."
  matching="$(jq -r --arg sha "$HEAD_SHA" '[.[] | select((.head.sha | ascii_downcase) == ($sha | ascii_downcase))] | length' <<<"$all_open")"
  current_count="$(jq -r --argjson pr "${PR_NUMBER}" --arg sha "$HEAD_SHA" '[.[] | select(.number == $pr and ((.head.sha | ascii_downcase) == ($sha | ascii_downcase)))] | length' <<<"$all_open")"
  [[ "$matching" =~ ^[0-9]+$ && "$current_count" =~ ^[0-9]+$ ]] || fail "Could not count open pull request identities."
  (( current_count == 1 )) || fail "The live pull request is missing from the open-head scan."
  sibling_count=$((matching - current_count))
  (( sibling_count == 0 )) || fail "The source head is shared by another open pull request."
  echo "Open-head collision scan passed."
}

find_workflow() {
  local pages_file
  pages_file="$(mktemp)"
  CLA_TEMP_FILES+=("$pages_file")
  : >"$pages_file"
  local page count
  for ((page = 1; page <= MAX_PAGES; page++)); do
    api_get "repos/${GH_REPO}/actions/workflows?per_page=100&page=$page" ||
      fail "Could not enumerate repository workflows."
    local page_json="$api_body"
    jq -e 'def safe_id: type == "number" and floor == . and . > 0 and . <= 9007199254740991; type == "object" and (.workflows | type == "array" and length <= 100) and all(.workflows[]; type == "object" and (.id | safe_id) and (.path | type == "string" and length > 0 and test("^[^\\r\\n]+$")) and (.state | type == "string"))' \
      <<<"$page_json" >/dev/null 2>&1 || fail "The workflow response is malformed."
    count="$(jq -er '.workflows | length | numbers' <<<"$page_json")"
    printf '%s\n' "$page_json" >>"$pages_file"
    if (( count < 100 )) && [[ "$api_has_next" != true ]]; then break; fi
    (( page < MAX_PAGES )) || fail "The workflow result window is truncated."
  done
  local all_workflows
  all_workflows="$(jq -s '[.[].workflows[]]' "$pages_file")" || fail "Could not combine workflow pages."
  WORKFLOW_ID="$(jq -r --arg path "$EXPECTED_WORKFLOW_PATH" '[.[] | select(.path == $path and .state == "active") | .id] | unique | if length == 1 then .[0] else empty end' <<<"$all_workflows")"
  is_safe_id "$WORKFLOW_ID" || fail "The expected CLA workflow is not uniquely active."
}

find_failed_run() {
  local pages_file
  pages_file="$(mktemp)"
  CLA_TEMP_FILES+=("$pages_file")
  : >"$pages_file"
  local page count
  for ((page = 1; page <= MAX_PAGES; page++)); do
    api_get "repos/${GH_REPO}/actions/workflows/$WORKFLOW_ID/runs?event=${TARGET_EVENT}&per_page=100&page=$page" ||
      fail "Could not enumerate CLA workflow runs."
    local page_json="$api_body"
    jq -e '
      def safe_id: type == "number" and floor == . and . > 0 and . <= 9007199254740991;
      def safe_ref: type == "string" and length > 0 and test("^[^\\r\\n]+$");
      def safe_sha: type == "string" and test("^[0-9a-fA-F]{40}$");
      def safe_timestamp: type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$");
      def valid_pr:
        type == "object" and (.number | safe_id) and
        (.base | type == "object") and (.base.ref | safe_ref) and (.base.repo | type == "object") and (.base.repo.id | safe_id) and
        (.head | type == "object") and (.head.ref | safe_ref) and (.head.sha | safe_sha);
      type == "object" and (.workflow_runs | type == "array" and length <= 100) and all(.workflow_runs[];
        type == "object" and (.id | safe_id) and (.workflow_id | safe_id) and
        (.path | type == "string" and length > 0 and test("^[^\\r\\n]+$")) and
        (.event | type == "string" and length > 0 and test("^[^\\r\\n]+$")) and
        (.status | type == "string" and length > 0 and test("^[^\\r\\n]+$")) and
        (.conclusion == null or (.conclusion | type == "string" and length > 0 and test("^[^\\r\\n]+$"))) and
        (.head_sha | safe_sha) and (.head_branch | safe_ref) and
        (.created_at | safe_timestamp) and
        (.pull_requests == null or (.pull_requests | type == "array" and all(.[]; valid_pr)))
      )
    ' \
      <<<"$page_json" >/dev/null 2>&1 || fail "The workflow-run response is malformed."
    count="$(jq -er '.workflow_runs | length | numbers' <<<"$page_json")"
    printf '%s\n' "$page_json" >>"$pages_file"
    if (( count < 100 )) && [[ "$api_has_next" != true ]]; then break; fi
    (( page < MAX_PAGES )) || fail "The workflow-run result window is truncated."
  done
  local all_runs candidates candidate_count
  all_runs="$(jq -s '[.[].workflow_runs[]]' "$pages_file")" || fail "Could not combine workflow-run pages."
  candidates="$(jq -c --arg workflow_id "$WORKFLOW_ID" --arg path "$EXPECTED_WORKFLOW_PATH" --arg event "${TARGET_EVENT}" --arg sha "$HEAD_SHA" --arg head_ref "$HEAD_REF" --arg base "${TARGET_BASE_REF}" --arg repo "${GH_REPO}" --arg before "${COMMENT_CREATED_AT}" --argjson pr "${PR_NUMBER}" --argjson base_repo_id "$BASE_REPO_ID" '
    def source_ok:
      if .pull_requests == null or (.pull_requests | length) == 0 then
        .head_sha == $sha and .head_branch == $head_ref
      else any(.pull_requests[]?;
        (.number | type == "number" and . == $pr) and
        (.base.ref == $base) and (.base.repo.id | type == "number" and . == $base_repo_id) and
        (.head.ref == $head_ref) and (.head.sha == $sha)
      ) end;
    [ .[] | select(
      (.id | type == "number" and floor == . and . > 0) and
      (.workflow_id | type == "number" and . == ($workflow_id | tonumber)) and
      .path == $path and .event == $event and .status == "completed" and .conclusion == "failure" and
      .head_sha == $sha and .head_branch == $head_ref and (.created_at | type == "string" and . <= $before) and
      source_ok
    ) ] | sort_by([.created_at, .id])
  ' <<<"$all_runs")" || fail "Could not select exact failed CLA workflow runs."
  candidate_count="$(jq -r 'length' <<<"$candidates")"
  [[ "$candidate_count" =~ ^[0-9]+$ ]] || fail "Could not count failed CLA workflow runs."
  # Keep the normal selection set small. The API scan itself is bounded to
  # MAX_FALLBACK_RUNS records, so a busy head cannot force an unbounded retry
  # or make the helper choose an arbitrary run outside the reviewed window.
  (( candidate_count <= MAX_FALLBACK_RUNS )) || fail "The exact source head has more than ${MAX_FALLBACK_RUNS} matching CLA workflow runs."
  (( candidate_count <= MAX_MATCHING_RUNS )) || fail "The exact source head has more than ${MAX_MATCHING_RUNS} matching CLA workflow runs; retry after old runs expire."
  if (( candidate_count == 0 )); then
    if [[ "${WRITER_POLICY_RESULT:-}" == true ]]; then
      no_op "no failed native CLA run exists for this exact source head"
    fi
    # A failed writer must never leave an earlier green required check as the
    # only visible result. Native v3 is the sole check producer, so an
    # administrator must start a fresh lifecycle run when no failed run can
    # be safely rerun from this comment event.
    fail "The writer did not confirm an all-signed policy and no failed native CLA run is available for a safe rerun."
  fi
  CANDIDATE_RUN_JSON="$(jq -c '.[-1]' <<<"$candidates")"
  RUN_ID="$(jq -er '.id | numbers | tostring' <<<"$CANDIDATE_RUN_JSON")"
  RUN_EXECUTION_SHA="$(jq -er '.head_sha | strings' <<<"$CANDIDATE_RUN_JSON")"
}

validate_run() {
  local run_json="$1"
  jq -e --arg run_id "$RUN_ID" --arg workflow_id "$WORKFLOW_ID" --arg path "$EXPECTED_WORKFLOW_PATH" --arg event "${TARGET_EVENT}" --arg sha "$HEAD_SHA" --arg head_ref "$HEAD_REF" --arg base "${TARGET_BASE_REF}" --arg repo "${GH_REPO}" --arg before "${COMMENT_CREATED_AT}" --argjson pr "${PR_NUMBER}" --argjson base_repo_id "$BASE_REPO_ID" '
    def source_ok:
      if .pull_requests == null or (.pull_requests | length) == 0 then .head_sha == $sha and .head_branch == $head_ref
      else any(.pull_requests[]?; (.number | type == "number" and . == $pr) and .base.ref == $base and (.base.repo.id | type == "number" and . == $base_repo_id) and .head.ref == $head_ref and .head.sha == $sha) end;
    type == "object" and (.id | type == "number" and floor == . and tostring == $run_id) and
    (.workflow_id | type == "number" and . == ($workflow_id | tonumber)) and .path == $path and .event == $event and
    .status == "completed" and .conclusion == "failure" and .head_sha == $sha and .head_branch == $head_ref and
    (.created_at | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$") and . <= $before) and
    .html_url == ("https://github.com/" + $repo + "/actions/runs/" + $run_id) and
    (.pull_requests == null or (.pull_requests | type == "array")) and source_ok
  ' <<<"$run_json" >/dev/null 2>&1 || fail "The selected workflow run is no longer exact."
}

fetch_jobs() {
  local target_run="$1"
  local pages_file
  pages_file="$(mktemp)"
  CLA_TEMP_FILES+=("$pages_file")
  : >"$pages_file"
  local page count
  for ((page = 1; page <= MAX_PAGES; page++)); do
    api_get "repos/${GH_REPO}/actions/runs/$target_run/jobs?per_page=100&page=$page" ||
      fail "Could not enumerate jobs for the selected workflow run."
    local page_json="$api_body"
    jq -e 'def safe_id: type == "number" and floor == . and . > 0 and . <= 9007199254740991; type == "object" and (.jobs | type == "array" and length <= 100) and all(.jobs[]; type == "object" and (.id | safe_id) and (.run_id | safe_id) and (.name | type == "string" and length > 0 and test("^[^\\r\\n]+$")) and (.status | type == "string" and length > 0 and test("^[^\\r\\n]+$")) and (.conclusion | type == "string" and length > 0 and test("^[^\\r\\n]+$")) and (.head_sha | type == "string" and test("^[0-9a-fA-F]{40}$")) and (.steps | type == "array" and length <= 100))' \
      <<<"$page_json" >/dev/null 2>&1 || fail "The workflow job response is malformed."
    count="$(jq -er '.jobs | length | numbers' <<<"$page_json")"
    printf '%s\n' "$page_json" >>"$pages_file"
    if (( count < 100 )) && [[ "$api_has_next" != true ]]; then break; fi
    (( page < MAX_PAGES )) || fail "The workflow job result window is truncated."
  done
  JOBS_JSON="$(jq -s '[.[].jobs[]]' "$pages_file")" || fail "Could not combine workflow job pages."
}

validate_jobs() {
  local jobs_json="$1" failed_json invalid_count native_total native_fold_total native_failed
  jq -e --arg run_id "$RUN_ID" --arg sha "$RUN_EXECUTION_SHA" '
    type == "array" and length <= 1000 and all(.[];
      (.id | type == "number" and floor == . and . > 0 and . <= 9007199254740991) and
      (.run_id | type == "number" and floor == . and . > 0 and . <= 9007199254740991 and . == ($run_id | tonumber)) and
      (.name | type == "string" and length > 0) and
      (.status == "completed") and (.conclusion | type == "string") and
      (.head_sha | type == "string" and (ascii_downcase == ($sha | ascii_downcase))) and
      (.steps | type == "array")
    )
  ' <<<"$jobs_json" >/dev/null 2>&1 || fail "The selected run contains malformed jobs."
  failed_json="$(jq -c '[.[] | select(.conclusion != "success" and .conclusion != "skipped")]' <<<"$jobs_json")"
  invalid_count="$(jq -r '[.[] | select(.conclusion != "failure")] | length' <<<"$failed_json")"
  (( invalid_count == 0 )) || fail "The selected run contains a cancelled or non-failure job."
  jq -e --arg assistant "$EXPECTED_ASSISTANT_JOB" --arg writer "$EXPECTED_WRITER_JOB" --arg gate "$EXPECTED_GATE_JOB" 'all(.[]; .name == $assistant or .name == $writer or .name == $gate)' <<<"$failed_json" >/dev/null 2>&1 ||
    fail "The selected run contains an unexpected failed job."
  native_total="$(jq -r --arg name "$EXPECTED_ASSISTANT_JOB" '[.[] | select(.name == $name)] | length' <<<"$jobs_json")"
  native_fold_total="$(jq -r --arg name "$EXPECTED_ASSISTANT_JOB" '[.[] | select((.name | ascii_downcase) == ($name | ascii_downcase))] | length' <<<"$jobs_json")"
  native_failed="$(jq -r --arg name "$EXPECTED_ASSISTANT_JOB" '[.[] | select(.name == $name and .conclusion == "failure")] | length' <<<"$jobs_json")"
  [[ "$native_total" =~ ^[0-9]+$ && "$native_fold_total" =~ ^[0-9]+$ && "$native_failed" =~ ^[0-9]+$ ]] || fail "Could not count native CLA jobs."
  (( native_total == 1 && native_fold_total == 1 && native_failed == 1 )) || fail "The selected run does not contain exactly one failed native CLA job."
  NATIVE_JOB_JSON="$(jq -c --arg name "$EXPECTED_ASSISTANT_JOB" --arg sha "$RUN_EXECUTION_SHA" --arg marker "CLA generation $EXPECTED_GENERATION" --arg run_id "$RUN_ID" --arg repo "${GH_REPO}" '[.[] | select(.name == $name and .conclusion == "failure" and ((.head_sha | ascii_downcase) == ($sha | ascii_downcase)) and .html_url == ("https://github.com/" + $repo + "/actions/runs/" + $run_id + "/job/" + (.id | tostring)) and ([.steps[] | select(.name == $marker and .status == "completed" and .conclusion == "success")] | length == 1))] | if length == 1 then .[0] else empty end' <<<"$jobs_json")" ||
    fail "Could not identify the exact failed native CLA job."
  [[ -n "$NATIVE_JOB_JSON" ]] || fail "The native job has an old generation or non-canonical URL."
  NATIVE_JOB_ID="$(jq -er '.id | numbers | tostring' <<<"$NATIVE_JOB_JSON")"
  # Rerun the complete exact run. The native job consumes writer outputs, so
  # rerunning the native job alone would reuse the old unsigned dependency.
  RERUN_ENDPOINT="repos/${GH_REPO}/actions/runs/$RUN_ID/rerun"
}

validate_native_check() {
  local pages_file
  pages_file="$(mktemp)"
  CLA_TEMP_FILES+=("$pages_file")
  : >"$pages_file"
  local page count
  for ((page = 1; page <= MAX_PAGES; page++)); do
    api_get "repos/${GH_REPO}/commits/$HEAD_SHA/check-runs?per_page=100&page=$page" ||
      fail "Could not enumerate checks for the selected source head."
    local page_json="$api_body"
    jq -e 'def safe_id: type == "number" and floor == . and . > 0 and . <= 9007199254740991; def safe_details: . == null or (. | type == "string" and length > 0 and test("^https://[^[:space:]\\r\\n]+$")); type == "object" and (.check_runs | type == "array" and length <= 100) and all(.check_runs[]; type == "object" and (.id | safe_id) and (.name | type == "string" and length > 0 and test("^[^\\r\\n]+$")) and (.head_sha | type == "string" and test("^[0-9a-fA-F]{40}$")) and (.status | type == "string" and length > 0 and test("^[^\\r\\n]+$")) and (.conclusion == null or (.conclusion | type == "string" and length > 0 and test("^[^\\r\\n]+$"))) and (.app | type == "object" and (.id | safe_id) and (.slug | type == "string" and length > 0 and test("^[^\\r\\n]+$"))) and (.details_url | safe_details))' \
      <<<"$page_json" >/dev/null 2>&1 || fail "The check-run response is malformed."
    count="$(jq -er '.check_runs | length | numbers' <<<"$page_json")"
    printf '%s\n' "$page_json" >>"$pages_file"
    if (( count < 100 )) && [[ "$api_has_next" != true ]]; then break; fi
    (( page < MAX_PAGES )) || fail "The check-run result window is truncated."
  done
  local all_checks canonical_url same_name_count same_name_nonfailure_count matching_count
  all_checks="$(jq -s '[.[].check_runs[]]' "$pages_file")" || fail "Could not combine check-run pages."
  canonical_url="https://github.com/${GH_REPO}/actions/runs/$RUN_ID/job/$NATIVE_JOB_ID"
  same_name_count="$(jq -r --arg name "$EXPECTED_ASSISTANT_JOB" --arg sha "$HEAD_SHA" --argjson app_id "$EXPECTED_APP_ID" --arg slug "$EXPECTED_APP_SLUG" '[.[] | select((.name | ascii_downcase) == ($name | ascii_downcase) and ((.head_sha | ascii_downcase) == ($sha | ascii_downcase)) and (.app.id | type == "number" and . == $app_id) and .app.slug == $slug)] | length' <<<"$all_checks")"
  [[ "$same_name_count" =~ ^[0-9]+$ ]] || fail "Could not count same-name native CLA checks."
  (( same_name_count <= MAX_FALLBACK_RUNS )) || fail "The source head has more than ${MAX_FALLBACK_RUNS} same-name native CLA checks."
  (( same_name_count >= 1 )) || fail "The source head has no native CLA check."
  # Repeated lifecycle events can legitimately leave several native checks for
  # one head. They are safe to tolerate only when every same-name result is a
  # completed failure. A successful or in-progress duplicate could satisfy
  # branch protection while the selected writer result is stale, so fail
  # closed instead of guessing which result GitHub will use.
  same_name_nonfailure_count="$(jq -r --arg name "$EXPECTED_ASSISTANT_JOB" --arg sha "$HEAD_SHA" --argjson app_id "$EXPECTED_APP_ID" --arg slug "$EXPECTED_APP_SLUG" '[.[] | select((.name | ascii_downcase) == ($name | ascii_downcase) and ((.head_sha | ascii_downcase) == ($sha | ascii_downcase)) and (.app.id | type == "number" and . == $app_id) and .app.slug == $slug and (.status != "completed" or .conclusion != "failure"))] | length' <<<"$all_checks")"
  [[ "$same_name_nonfailure_count" =~ ^[0-9]+$ ]] || fail "Could not validate same-name native CLA check conclusions."
  (( same_name_nonfailure_count == 0 )) || fail "The source head has a successful or incomplete duplicate native CLA check."
  matching_count="$(jq -r --arg name "$EXPECTED_ASSISTANT_JOB" --arg sha "$HEAD_SHA" --arg url "$canonical_url" --argjson app_id "$EXPECTED_APP_ID" --arg slug "$EXPECTED_APP_SLUG" '[.[] | select((.name | ascii_downcase) == ($name | ascii_downcase) and ((.head_sha | ascii_downcase) == ($sha | ascii_downcase)) and .status == "completed" and .conclusion == "failure" and (.app.id | type == "number" and . == $app_id) and .app.slug == $slug and .details_url == $url)] | length' <<<"$all_checks")"
  [[ "$matching_count" =~ ^[0-9]+$ ]] || fail "Could not count native CLA checks."
  (( matching_count == 1 )) || fail "The exact native CLA check is missing or colliding with another producer."
  CHECK_ID="$(jq -er --arg name "$EXPECTED_ASSISTANT_JOB" --arg sha "$HEAD_SHA" --arg url "$canonical_url" --argjson app_id "$EXPECTED_APP_ID" --arg slug "$EXPECTED_APP_SLUG" '[.[] | select(.name == $name and .head_sha == $sha and .status == "completed" and .conclusion == "failure" and .app.id == $app_id and .app.slug == $slug and .details_url == $url) | .id] | if length == 1 then .[0] else empty end' <<<"$all_checks")"
  is_safe_id "$CHECK_ID" || fail "The native CLA check ID is invalid."
  api_get "repos/${GH_REPO}/check-runs/$CHECK_ID" || fail "Could not re-read the native CLA check."
  jq -e --arg id "$CHECK_ID" --arg name "$EXPECTED_ASSISTANT_JOB" --arg sha "$HEAD_SHA" --arg url "$canonical_url" --argjson app_id "$EXPECTED_APP_ID" --arg slug "$EXPECTED_APP_SLUG" 'type == "object" and (.id | type == "number" and floor == . and tostring == $id) and .name == $name and ((.head_sha | ascii_downcase) == ($sha | ascii_downcase)) and .status == "completed" and .conclusion == "failure" and (.app.id | type == "number" and . == $app_id) and .app.slug == $slug and .details_url == $url' <<<"$api_body" >/dev/null 2>&1 ||
    fail "The native CLA check changed or is not bound to the selected job."
}

final_recheck() {
  read_pr
  validate_unique_head
  api_get "repos/${GH_REPO}/actions/runs/$RUN_ID" || fail "Could not re-read the selected run."
  validate_run "$api_body"
  fetch_jobs "$RUN_ID"
  validate_jobs "$JOBS_JSON"
  validate_native_check
}

require_inputs
read_issue
read_comment
read_pr
validate_signature
validate_unique_head
find_workflow
find_failed_run
api_get "repos/${GH_REPO}/actions/runs/$RUN_ID" || fail "Could not read the selected run."
validate_run "$api_body"
fetch_jobs "$RUN_ID"
validate_jobs "$JOBS_JSON"
api_get "repos/${GH_REPO}/actions/jobs/$NATIVE_JOB_ID" || fail "Could not read the selected native job."
jq -e --arg id "$NATIVE_JOB_ID" --arg run_id "$RUN_ID" --arg name "$EXPECTED_ASSISTANT_JOB" --arg sha "$RUN_EXECUTION_SHA" --arg repo "${GH_REPO}" --arg marker "CLA generation $EXPECTED_GENERATION" 'type == "object" and (.id | type == "number" and floor == . and tostring == $id) and (.run_id | type == "number" and tostring == $run_id) and .name == $name and .status == "completed" and .conclusion == "failure" and ((.head_sha | ascii_downcase) == ($sha | ascii_downcase)) and .html_url == ("https://github.com/" + $repo + "/actions/runs/" + $run_id + "/job/" + $id) and ([.steps[] | select(.name == $marker and .status == "completed" and .conclusion == "success")] | length == 1)' <<<"$api_body" >/dev/null 2>&1 ||
  fail "The selected native job no longer matches the exact failed job."
validate_native_check
final_recheck
api_post "$RERUN_ENDPOINT" || fail "Could not rerun the exact native CLA workflow run."
echo "Requested a full rerun of workflow run $RUN_ID for source head $HEAD_SHA."
