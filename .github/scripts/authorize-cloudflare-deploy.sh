#!/usr/bin/env bash
# Authorize the source used by the Cloudflare deployment workflow.
# A deployment is allowed only from the protected main branch at the exact
# commit that started this run. Pull requests are accepted for read-only tests.
set -euo pipefail

readonly EXPECTED_REPOSITORY='manaflow-ai/subrouter'
readonly EXPECTED_WORKFLOW_REF='manaflow-ai/subrouter/.github/workflows/cloudflare-do.yml@refs/heads/main'
readonly MAX_BRANCH_RESPONSE_BYTES=262144

fail() {
  echo "::error title=Cloudflare source policy::${1}" >&2
  exit 1
}

is_sha() {
  [[ "${1:-}" =~ ^[0-9a-fA-F]{40}$ ]]
}

[[ "${EVENT_REPOSITORY:-}" == "$EXPECTED_REPOSITORY" ]] ||
  fail "The workflow event is not from the canonical repository."
[[ "${GH_REPO:-}" == "$EXPECTED_REPOSITORY" ]] ||
  fail "The deployment guard is not running for the canonical repository."
is_sha "${EVENT_SHA:-}" || fail "The event commit SHA is invalid."

case "${EVENT_NAME:-}" in
  pull_request)
    # Pull request verification has no deployment credentials. Keep this path
    # available for forks, but never let its output authorize a deploy.
    [[ "${EVENT_REF:-}" =~ ^refs/pull/[1-9][0-9]*/(merge|head)$ ]] ||
      fail "The pull request ref is invalid."
    printf 'source_sha=%s\ndeploy_authorized=false\n' "${EVENT_SHA}" >> "${GITHUB_OUTPUT}"
    echo "Cloudflare verification authorized for pull request source ${EVENT_SHA}."
    exit 0
    ;;
  push|workflow_dispatch)
    [[ "${EVENT_REF:-}" == refs/heads/main ]] ||
      fail "Deployments require the protected main branch ref."
    [[ "${EVENT_REF_PROTECTED:-}" == true ]] ||
      fail "Deployments require a GitHub-protected main branch."
    [[ "${WORKFLOW_REF:-}" == "$EXPECTED_WORKFLOW_REF" ]] ||
      fail "The deployment workflow must execute from its protected main revision."
    is_sha "${WORKFLOW_SHA:-}" || fail "The workflow revision SHA is invalid."
    [[ "${WORKFLOW_SHA,,}" == "${EVENT_SHA,,}" ]] ||
      fail "The workflow revision and event commit differ."
    ;;
  *)
    fail "The event is not an approved Cloudflare workflow trigger."
    ;;
esac

branch_file="$(mktemp)"
trap 'rm -f -- "${branch_file}"' EXIT
set +e
gh api --method GET \
  --header 'Accept: application/vnd.github+json' \
  --header 'X-GitHub-Api-Version: 2022-11-28' \
  --silent "repos/${EXPECTED_REPOSITORY}/branches/main" 2>/dev/null |
  head -c "$((MAX_BRANCH_RESPONSE_BYTES + 1))" > "${branch_file}"
pipeline_status=("${PIPESTATUS[@]}")
set -e
branch_bytes="$(LC_ALL=C wc -c < "${branch_file}" | tr -d '[:space:]')"
[[ "${branch_bytes}" =~ ^[0-9]+$ ]] || fail "The protected main response size is invalid."
(( branch_bytes <= MAX_BRANCH_RESPONSE_BYTES )) ||
  fail "The protected main response exceeds the safety limit."
(( ${pipeline_status[0]:-1} == 0 && ${pipeline_status[1]:-1} == 0 )) ||
  fail "Could not read the protected main branch."

jq -e --arg expected_sha "${EVENT_SHA}" '
  type == "object" and
  .name == "main" and
  .protected == true and
  (.commit | type == "object") and
  (.commit.sha | type == "string" and test("^[0-9a-fA-F]{40}$")) and
  ((.commit.sha | ascii_downcase) == ($expected_sha | ascii_downcase))
' "${branch_file}" >/dev/null 2>&1 ||
  fail "The protected main branch is missing, unprotected, or moved during authorization."

printf 'source_sha=%s\ndeploy_authorized=true\n' "${EVENT_SHA}" >> "${GITHUB_OUTPUT}"
echo "Cloudflare deployment authorized for protected main source ${EVENT_SHA}."
