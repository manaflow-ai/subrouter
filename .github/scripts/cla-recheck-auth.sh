#!/usr/bin/env bash
# Authenticate the exact `recheck` comment without granting a writer token.
# The caller treats `retry` as a no-op and retries after the API recovers.
set -euo pipefail

readonly EXPECTED_REPOSITORY='manaflow-ai/subrouter'
readonly MAX_RESPONSE_BYTES=262144
readonly MAX_TOTAL_BYTES=2000000
readonly MAX_ATTEMPTS=3
readonly MAX_SAFE_INTEGER=9007199254740991
readonly RETRY_DELAY_SECONDS="${CLA_RECHECK_RETRY_DELAY_SECONDS:-1}"

: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

authorized=false
decision=error
check_action=fail
api_total_bytes=0
raw_file="$(mktemp)"
trap 'rm -f -- "${raw_file}"' EXIT

emit() {
  printf 'authorized=%s\ndecision=%s\ncheck_action=%s\n' \
    "${authorized}" "${decision}" "${check_action}" >>"${GITHUB_OUTPUT}"
}

finish() {
  decision="$1"
  check_action="$2"
  emit
  printf '%s\n' "$3"
  [[ "$4" == 0 ]] || exit "$4"
  exit 0
}

fail_input() {
  authorized=false
  decision=error
  check_action=fail
  emit
  echo "::error title=CLA recheck policy::$1" >&2
  exit 1
}

retry_later() {
  authorized=false
  decision=retry
  check_action=preserve
  emit
  echo "::warning title=CLA recheck retry::$1" >&2
  exit 1
}

is_safe_id() {
  local value="${1:-}"
  [[ "$value" =~ ^[1-9][0-9]*$ ]] || return 1
  (( ${#value} <= 16 )) || return 1
  (( ${#value} != 16 || value <= MAX_SAFE_INTEGER ))
}

is_safe_login() {
  [[ "${1:-}" =~ ^[A-Za-z0-9][A-Za-z0-9-]{0,38}$ ]]
}

is_timestamp() {
  [[ "${1:-}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]]
}

is_valid_not_found() {
  local body="${1:-}"
  jq -e 'type == "object" and ((.status == 404) or (.status == "404")) and (.message | type == "string" and length > 0)' \
    <<<"${body}" >/dev/null 2>&1
}

extract_http_body() {
  awk '
    BEGIN { in_headers = 0; body_started = 0; body = "" }
    {
      sub(/\r$/, "")
      if ($0 ~ /^HTTP\/[0-9.]+[[:space:]][0-9][0-9][0-9]([[:space:]]|$)/) {
        in_headers = 1
        body_started = 0
        body = ""
        next
      }
      if (in_headers && $0 == "") {
        in_headers = 0
        body_started = 1
        next
      }
      if (body_started) body = body $0 "\n"
    }
    END { printf "%s", body }
  ' "${raw_file}"
}

read_response() {
  local endpoint="$1"
  local attempt raw_bytes command_status http_status response_body
  api_kind=retry
  api_body=""

  for ((attempt = 1; attempt <= MAX_ATTEMPTS; attempt++)); do
    : >"${raw_file}"
    set +e
    (
      # `ulimit -f` is a pre-parser bound. The extra block permits the limit
      # check below to distinguish an exact-bound response from an overflow.
      ulimit -f $(( (MAX_RESPONSE_BYTES + 511) / 512 + 1 ))
      gh api --method GET --include \
        --header 'Accept: application/vnd.github+json' \
        --header 'X-GitHub-Api-Version: 2022-11-28' \
        "${endpoint}" >"${raw_file}" 2>/dev/null
    )
    command_status=$?
    set -e

    raw_bytes="$(LC_ALL=C wc -c <"${raw_file}" | tr -d '[:space:]')"
    if ! [[ "${raw_bytes}" =~ ^[0-9]+$ ]] || (( raw_bytes > MAX_RESPONSE_BYTES )); then
      api_kind=retry
    else
      api_total_bytes=$((api_total_bytes + raw_bytes))
      if (( api_total_bytes > MAX_TOTAL_BYTES )); then
        api_kind=retry
      else
        http_status="$(awk '/^HTTP\// { gsub("\r", "", $2); code=$2 } END { print code }' "${raw_file}")"
        if [[ -z "${http_status}" ]]; then
          http_status=200
          response_body="$(<"${raw_file}")"
        else
          response_body="$(extract_http_body)"
        fi
        api_body="${response_body}"
        if [[ "${http_status}" == 404 ]] && is_valid_not_found "${response_body}"; then
          api_kind=not_found
          return 2
        elif (( command_status == 0 )) && [[ "${http_status}" =~ ^2[0-9][0-9]$ ]]; then
          api_kind=ok
          return 0
        else
          api_kind=retry
        fi
      fi
    fi
    if (( attempt < MAX_ATTEMPTS )); then
      sleep "${RETRY_DELAY_SECONDS}"
    fi
  done
  return 1
}

require_inputs() {
  [[ "${GH_REPO:-}" == "${EXPECTED_REPOSITORY}" ]] || fail_input 'The helper is not running in the canonical repository.'
  [[ "${COMMENT_BODY:-}" == 'recheck' ]] || fail_input 'The comment is not the exact recheck command.'
  is_safe_id "${PR_NUMBER:-}" || fail_input 'The pull request number is invalid.'
  is_safe_id "${COMMENT_ID:-}" || fail_input 'The comment ID is invalid.'
  is_safe_id "${COMMENT_USER_ID:-}" || fail_input 'The commenter ID is invalid.'
  is_safe_login "${COMMENT_USER_LOGIN:-}" || fail_input 'The commenter login is invalid.'
  [[ "${COMMENT_USER_TYPE:-User}" == User ]] || fail_input 'The commenter is not an authenticated human user.'
  is_timestamp "${COMMENT_CREATED_AT:-}" || fail_input 'The comment timestamp is invalid.'
}

require_inputs

validate_state() {
  local resource="$1"
  if ! jq -e '
    type == "object" and
    (.state | type == "string" and (. == "open" or . == "closed"))
  ' <<<"${api_body}" >/dev/null 2>&1; then
    fail_input "GitHub returned an invalid ${resource} pull request state."
  fi
}

# Read the issue resource first. It uses the issue read permission and remains
# available for fork Pull Requests when the Pulls endpoint is temporarily
# unavailable. It proves that this exact issue is still an open Pull Request.
if ! read_response "repos/${GH_REPO}/issues/${PR_NUMBER}"; then
  [[ "${api_kind}" == not_found ]] && finish unauthorized preserve 'CLA recheck ignored: the pull request no longer exists or is closed.' 0
  retry_later 'GitHub could not read the pull request issue after bounded retries.'
fi
if ! jq -e --arg pr "${PR_NUMBER}" '
  def safe_id: type == "number" and floor == . and . > 0 and . <= 9007199254740991;
  type == "object" and (.number | safe_id and tostring == $pr)
' <<<"${api_body}" >/dev/null 2>&1; then
  fail_input 'GitHub returned malformed pull request issue data.'
fi
validate_state issue
if jq -e '.state == "closed"' <<<"${api_body}" >/dev/null 2>&1; then
  finish unauthorized preserve 'CLA recheck ignored: the pull request is no longer open.' 0
fi
if ! jq -e --arg repo "${GH_REPO}" --arg pr "${PR_NUMBER}" '
  (.state | type == "string") and
  (.pull_request | type == "object") and
  .pull_request.url == ("https://api.github.com/repos/" + $repo + "/pulls/" + $pr)
' <<<"${api_body}" >/dev/null 2>&1; then
  fail_input 'GitHub returned incomplete pull request issue data.'
fi

# The Pulls resource is the authoritative base/head identity. A validated
# issue response is not enough to authorize a privileged refresh. If this read
# remains unavailable, preserve any existing check and ask for a later retry.
if ! read_response "repos/${GH_REPO}/pulls/${PR_NUMBER}"; then
  [[ "${api_kind}" == not_found ]] && finish unauthorized preserve 'CLA recheck ignored: the pull request no longer exists.' 0
  retry_later 'GitHub could not read the live pull request identity after bounded retries.'
fi
if ! jq -e --arg pr "${PR_NUMBER}" '
  def safe_id: type == "number" and floor == . and . > 0 and . <= 9007199254740991;
  type == "object" and (.number | safe_id and tostring == $pr)
' <<<"${api_body}" >/dev/null 2>&1; then
  fail_input 'GitHub returned malformed live pull request identity data.'
fi
validate_state pull request
if jq -e '.state == "closed"' <<<"${api_body}" >/dev/null 2>&1; then
  finish unauthorized preserve 'CLA recheck ignored: the pull request is no longer open.' 0
fi
if ! jq -e '
  has("merged_at") and
  (.merged_at == null or
   (.merged_at | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?Z$"))) and
  (.state == "closed" or (.state == "open" and .merged_at == null))
' <<<"${api_body}" >/dev/null 2>&1; then
  fail_input 'GitHub returned an inconsistent merged_at value for the live pull request.'
fi
if ! jq -e --arg repo "${GH_REPO}" '
  def safe_id: type == "number" and floor == . and . > 0 and . <= 9007199254740991;
  (.base | type == "object") and .base.ref == "main" and
  (.base.repo | type == "object") and
  ((.base.repo.full_name | type == "string") and (.base.repo.full_name | ascii_downcase) == ($repo | ascii_downcase)) and
  (.base.repo.id | safe_id) and
  (.head | type == "object") and (.head.sha | type == "string" and test("^[0-9a-fA-F]{40}$")) and
  (.head.repo | type == "object") and (.head.repo.id | safe_id) and
  (.user | type == "object") and (.user.id | safe_id)
' <<<"${api_body}" >/dev/null 2>&1; then
  if jq -e --arg repo "${GH_REPO}" '
    type == "object" and
    ((.base | type == "object") and
      ((.base.ref | type == "string") and .base.ref != "main" or
       (.base.repo | type == "object") and
       (.base.repo.full_name | type == "string") and
       (.base.repo.full_name | ascii_downcase) != ($repo | ascii_downcase)))
  ' <<<"${api_body}" >/dev/null 2>&1; then
    finish unauthorized preserve 'CLA recheck ignored: the pull request no longer targets main.' 0
  fi
  fail_input 'GitHub returned incomplete live pull request identity data.'
fi

# Confirm the event comment is still the exact, unedited snapshot. A validated
# 404 or an immutable mismatch is ordinary unauthorized input. Other failures
# retain the explicit retry state.
if ! read_response "repos/${GH_REPO}/issues/comments/${COMMENT_ID}"; then
  [[ "${api_kind}" == not_found ]] && finish unauthorized preserve 'CLA recheck ignored: the command comment was deleted.' 0
  retry_later 'GitHub could not read the live recheck comment after bounded retries.'
fi
if ! jq -e --arg id "${COMMENT_ID}" --arg body "${COMMENT_BODY}" --arg user_id "${COMMENT_USER_ID}" \
          --arg login "${COMMENT_USER_LOGIN}" --arg issue_url "https://api.github.com/repos/${GH_REPO}/issues/${PR_NUMBER}" \
          --arg created "${COMMENT_CREATED_AT}" '
  type == "object" and (.id | type == "number" and floor == . and tostring == $id) and
  .body == $body and (.user | type == "object") and
  (.user.id | type == "number" and floor == . and tostring == $user_id) and
  (.user.login | type == "string" and ascii_downcase == ($login | ascii_downcase)) and
  .user.type == "User" and .issue_url == $issue_url and
  (.created_at | type == "string" and . == $created) and .updated_at == .created_at
' <<<"${api_body}" >/dev/null 2>&1; then
  if jq -e 'type == "object" and (.id | type == "number") and (.body | type == "string") and (.user | type == "object")' <<<"${api_body}" >/dev/null 2>&1; then
    finish unauthorized preserve 'CLA recheck ignored: the command comment changed after delivery.' 0
  fi
  fail_input 'GitHub returned malformed live recheck comment data.'
fi

# A 404 from the collaborator permission endpoint is a validated ordinary
# denial. Do not turn it into an infrastructure failure.
if ! read_response "repos/${GH_REPO}/collaborators/${COMMENT_USER_LOGIN}/permission"; then
  [[ "${api_kind}" == not_found ]] && finish unauthorized preserve 'CLA recheck ignored: requester is not an admin or maintainer.' 0
  retry_later 'GitHub could not verify the requester permission after bounded retries.'
fi
if ! jq -e --arg id "${COMMENT_USER_ID}" '
  type == "object" and (.user | type == "object") and
  (.user.id | type == "number" and floor == . and tostring == $id) and
  (.permission | type == "string") and
  ((.role_name // "") | type == "string")
' <<<"${api_body}" >/dev/null 2>&1; then
  fail_input 'GitHub returned malformed requester permission data.'
fi
role_name="$(jq -r '.role_name // .permission' <<<"${api_body}")"
case "${role_name}" in
  admin|maintain)
    finish authorized refresh 'CLA recheck requester has live admin or maintainer permission.' 0
    ;;
  push|write|none|read|pull|triage)
    finish unauthorized preserve 'CLA recheck ignored: requester is not an admin or maintainer.' 0
    ;;
  *)
    retry_later 'GitHub returned an unknown requester permission role.'
    ;;
esac
