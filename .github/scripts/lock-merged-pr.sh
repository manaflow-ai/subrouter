#!/usr/bin/env bash
# Lock one verified merged Pull Request. This file is checked out from the
# trusted workflow revision and never from a Pull Request head.
set -euo pipefail

readonly EXPECTED_REPOSITORY='manaflow-ai/subrouter'
readonly MAX_RESPONSE_BYTES=262144
readonly MAX_TOTAL_BYTES=2000000
readonly MAX_SAFE_INTEGER=9007199254740991

fail() {
  echo "::error title=CLA lock policy::$1" >&2
  exit 1
}

is_safe_id() {
  local value="${1:-}"
  [[ "${value}" =~ ^[1-9][0-9]*$ ]] || return 1
  (( ${#value} <= 16 )) || return 1
  (( ${#value} != 16 || value <= MAX_SAFE_INTEGER ))
}

is_safe_text() {
  local value="${1:-}"
  [[ -n "${value}" && "${value}" != *$'\n'* && "${value}" != *$'\r'* ]]
}

is_sha() {
  [[ "${1:-}" =~ ^[0-9a-fA-F]{40}$ ]]
}

: "${GH_TOKEN:?GH_TOKEN is required}"
: "${GH_REPO:?GH_REPO is required}"
: "${PR_NUMBER:?PR_NUMBER is required}"

[[ "${GH_REPO}" == "${EXPECTED_REPOSITORY}" ]] || fail 'The workflow is not running in the canonical repository.'
is_safe_id "${PR_NUMBER}" || fail 'The Pull Request number is invalid.'
is_safe_id "${CANONICAL_REPOSITORY_ID:-}" || fail 'The canonical repository ID is invalid.'
[[ "${EVENT_NAME:-}" == pull_request_target && "${EVENT_ACTION:-}" == closed ]] || fail 'The event is not a closed Pull Request event.'
[[ "${EVENT_PR_STATE:-}" == closed && "${EVENT_PR_MERGED:-}" == true ]] || fail 'The event does not describe a merged Pull Request.'
[[ "${EVENT_PR_NUMBER:-}" == "${PR_NUMBER}" ]] || fail 'The event Pull Request number changed.'
[[ "${EVENT_REPOSITORY:-}" == "${EXPECTED_REPOSITORY}" ]] || fail 'The event repository is not canonical.'
is_safe_id "${EVENT_REPOSITORY_ID:-}" || fail 'The event repository ID is invalid.'
[[ "${EVENT_REPOSITORY_ID}" == "${CANONICAL_REPOSITORY_ID}" ]] || fail 'The event repository ID changed.'
[[ "${EVENT_BASE_REF:-}" == main && "${EVENT_BASE_REPOSITORY:-}" == "${EXPECTED_REPOSITORY}" ]] || fail 'The event base is not canonical main.'
is_safe_id "${EVENT_BASE_REPOSITORY_ID:-}" || fail 'The event base repository ID is invalid.'
[[ "${EVENT_BASE_REPOSITORY_ID}" == "${CANONICAL_REPOSITORY_ID}" ]] || fail 'The event base repository ID changed.'
is_safe_text "${EVENT_OPENER_LOGIN:-}" || fail 'The event opener login is invalid.'
is_safe_id "${EVENT_OPENER_ID:-}" || fail 'The event opener ID is invalid.'

API_FILE="$(mktemp)"
LOCK_FILE="$(mktemp)"
readonly API_FILE LOCK_FILE
trap 'rm -f -- "${API_FILE}" "${LOCK_FILE}"' EXIT
api_total_bytes=0
api_status=''
api_body=''

api_request() {
  local method="$1" endpoint="$2" raw_bytes command_status first_line
  : >"${API_FILE}"
  set +e
  (
    # The file limit is a pre-parser bound. The byte count below also rejects
    # an exact overflow and protects the aggregate request budget.
    ulimit -f $(( (MAX_RESPONSE_BYTES + 511) / 512 + 1 ))
    if [[ "${method}" == PUT ]]; then
      gh api --method PUT --include \
        --header 'Accept: application/vnd.github+json' \
        --header 'X-GitHub-Api-Version: 2022-11-28' \
        "${endpoint}" --field lock_reason=resolved >"${API_FILE}" 2>/dev/null
    else
      gh api --method GET --include \
        --header 'Accept: application/vnd.github+json' \
        --header 'X-GitHub-Api-Version: 2022-11-28' \
        "${endpoint}" >"${API_FILE}" 2>/dev/null
    fi
  )
  command_status=$?
  set -e
  raw_bytes="$(LC_ALL=C wc -c <"${API_FILE}" | tr -d '[:space:]')"
  [[ "${raw_bytes}" =~ ^[0-9]+$ ]] || return 1
  (( raw_bytes <= MAX_RESPONSE_BYTES )) || return 2
  api_total_bytes=$((api_total_bytes + raw_bytes))
  (( api_total_bytes <= MAX_TOTAL_BYTES )) || return 2

  IFS= read -r first_line <"${API_FILE}" || true
  if [[ "${first_line}" =~ ^HTTP/[0-9.]+[[:space:]]+[0-9]{3}([[:space:]]|$) ]]; then
    api_status="$(awk '/^HTTP\// { gsub("\r", "", $2); code=$2 } END { print code }' "${API_FILE}")"
    api_body="$(awk 'BEGIN { body=0 } { sub(/\r$/, "") } body { print; next } /^$/ { body=1 }' "${API_FILE}")"
  else
    api_status=200
    api_body="$(<"${API_FILE}")"
  fi
  [[ "${api_status}" =~ ^[0-9]{3}$ ]] || return 1
  (( command_status == 0 )) || return 1
  [[ "${api_status}" =~ ^2[0-9][0-9]$ ]]
}

api_get() { api_request GET "$1"; }

validate_live_pr() {
  local json="$1" expected_base_ref expected_base_repo expected_base_id expected_opener_login expected_opener_id
  jq -e --arg repo "${EXPECTED_REPOSITORY}" --arg pr "${PR_NUMBER}" '
    def safe_id: type == "number" and floor == . and . > 0 and . <= 9007199254740991;
    def text: type == "string" and length > 0 and test("^[^\\r\\n]+$");
    type == "object" and (.number | safe_id and tostring == $pr) and
    .state == "closed" and (.merged_at | type == "string" and length > 0) and
    (.base | type == "object" and .ref == "main" and (.repo | type == "object")) and
    .base.repo.full_name == $repo and (.base.repo.id | safe_id) and
    (.head | type == "object") and (.head.ref | text) and (.head.sha | type == "string" and test("^[0-9a-fA-F]{40}$")) and
    (if .head.repo == null then true else
      (.head.repo | type == "object" and (.full_name | text) and (.id | safe_id))
    end) and
    (.user | type == "object" and (.login | text) and (.id | safe_id))
  ' <<<"${json}" >/dev/null 2>&1 || return 1
  expected_base_ref="$(jq -er '.base.ref | strings' <<<"${json}")"
  expected_base_repo="$(jq -er '.base.repo.full_name | strings' <<<"${json}")"
  expected_base_id="$(jq -er '.base.repo.id | numbers | tostring' <<<"${json}")"
  expected_opener_login="$(jq -er '.user.login | strings' <<<"${json}")"
  expected_opener_id="$(jq -er '.user.id | numbers | tostring' <<<"${json}")"
  [[ "${expected_base_ref}" == "${EVENT_BASE_REF}" &&
     "${expected_base_repo}" == "${EVENT_BASE_REPOSITORY}" &&
     "${expected_base_id}" == "${EVENT_BASE_REPOSITORY_ID}" &&
     "${expected_opener_login,,}" == "${EVENT_OPENER_LOGIN,,}" &&
     "${expected_opener_id}" == "${EVENT_OPENER_ID}" ]]
}

pr_endpoint="repos/${GH_REPO}/pulls/${PR_NUMBER}"
api_get "${pr_endpoint}" || fail 'Could not read the live merged Pull Request.'
live_pr="${api_body}"
validate_live_pr "${live_pr}" || fail 'The live merged Pull Request identity changed.'

issue_endpoint="repos/${GH_REPO}/issues/${PR_NUMBER}"
lock_endpoint="${issue_endpoint}/lock"
api_get "${issue_endpoint}" || fail 'Could not read the Pull Request lock state.'
jq -e 'type == "object" and (.locked | type == "boolean")' <<<"${api_body}" >/dev/null 2>&1 || fail 'GitHub returned an invalid lock state.'
locked="$(jq -er '.locked | tostring' <<<"${api_body}")"
if [[ "${locked}" == true ]]; then
  api_get "${pr_endpoint}" || fail 'Could not re-read the merged Pull Request.'
  validate_live_pr "${api_body}" || fail 'The already-locked Pull Request identity changed.'
  echo "Pull Request ${PR_NUMBER} is already locked."
  exit 0
fi
[[ "${locked}" == false ]] || fail 'GitHub returned an unknown lock state.'

api_request PUT "${lock_endpoint}" || fail "Could not lock Pull Request ${PR_NUMBER}."
[[ "${api_status}" == 204 ]] || fail "GitHub returned HTTP ${api_status} while locking Pull Request ${PR_NUMBER}."

api_get "${issue_endpoint}" || fail 'Could not verify the Pull Request lock state.'
jq -e '.locked == true' <<<"${api_body}" >/dev/null 2>&1 || fail 'GitHub did not report the Pull Request as locked.'
api_get "${pr_endpoint}" || fail 'Could not verify the merged Pull Request identity after locking.'
validate_live_pr "${api_body}" || fail 'The Pull Request identity changed after locking; retain the lock for administrator review.'
echo "Locked merged Pull Request ${PR_NUMBER}."
