#!/usr/bin/env bash
set -euo pipefail

# Validate the immutable source context used by every publishing job. The
# caller must check out GITHUB_SHA before invoking this helper.
: "${GITHUB_SHA:?GITHUB_SHA is required}"
: "${GITHUB_REF:?GITHUB_REF is required}"
: "${GITHUB_REF_NAME:?GITHUB_REF_NAME is required}"
: "${GITHUB_REF_TYPE:?GITHUB_REF_TYPE is required}"
: "${GITHUB_EVENT_NAME:?GITHUB_EVENT_NAME is required}"
: "${GITHUB_REF_PROTECTED:?GITHUB_REF_PROTECTED is required}"

[[ "${GITHUB_SHA}" =~ ^[0-9a-f]{40}$ ]] || {
  echo "GITHUB_SHA is not a full lowercase commit SHA" >&2
  exit 1
}
[[ "${GITHUB_EVENT_NAME}" == "push" && "${GITHUB_REF_TYPE}" == "tag" && "${GITHUB_REF}" == "refs/tags/${GITHUB_REF_NAME}" && "${GITHUB_REF_PROTECTED}" == "true" ]] || {
  echo "release jobs require a protected tag push" >&2
  exit 1
}
[[ "${GITHUB_REF_NAME}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || {
  echo "release tag must be versioned, for example v0.1.52" >&2
  exit 1
}

head_sha="$(git rev-parse 'HEAD^{commit}')"
[[ "${head_sha}" == "${GITHUB_SHA}" ]] || {
  echo "checked out commit ${head_sha} does not match the event commit ${GITHUB_SHA}" >&2
  exit 1
}

# Fetch the event tag explicitly and force-update only this local ref. This
# catches a tag movement between the webhook and the checkout. Protected tag
# rulesets are still required, because this check cannot prevent a race after
# it completes.
git fetch --force --no-tags origin \
  "+refs/tags/${GITHUB_REF_NAME}:refs/tags/${GITHUB_REF_NAME}" >/dev/null
tag_sha="$(git rev-parse "refs/tags/${GITHUB_REF_NAME}^{commit}")"
[[ "${tag_sha}" == "${GITHUB_SHA}" ]] || {
  echo "tag ${GITHUB_REF_NAME} now resolves to ${tag_sha}, expected ${GITHUB_SHA}" >&2
  exit 1
}

git fetch --no-tags origin main >/dev/null
main_sha="$(git rev-parse 'origin/main^{commit}')"
git merge-base --is-ancestor "${GITHUB_SHA}" "${main_sha}" || {
  echo "release tag ${GITHUB_REF_NAME} is not an ancestor of protected main" >&2
  exit 1
}

printf '%s\n' "${GITHUB_SHA}"
