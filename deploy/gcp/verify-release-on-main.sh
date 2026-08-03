#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: verify-release-on-main.sh REPOSITORY TAG [EXPECTED_REVISION]

Resolves TAG to a commit and verifies that commit is on the repository's main
branch. The GitHub comparison is streamed into jq so a large response cannot
deadlock Bash while constructing a here-string.
EOF
}

if (( $# < 2 || $# > 3 )); then
  usage >&2
  exit 2
fi

repository="$1"
tag="$2"
expected_revision="${3:-}"

[[ "${repository}" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || {
  echo "repository is invalid" >&2
  exit 1
}
[[ "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || {
  echo "release tag is invalid" >&2
  exit 1
}
for command in gh jq; do
  command -v "${command}" >/dev/null 2>&1 || {
    echo "${command} is required" >&2
    exit 1
  }
done

revision="$(gh api "repos/${repository}/commits/${tag}" --jq '.sha')"
[[ "${revision}" =~ ^[0-9a-f]{40}$ ]] || {
  echo "${tag} did not resolve to a commit" >&2
  exit 1
}
if [[ -n "${expected_revision}" && "${revision}" != "${expected_revision}" ]]; then
  echo "${tag} revision does not match its hard pin" >&2
  exit 1
fi
if gh api "repos/${repository}/compare/${revision}...main" |
    jq -e --arg revision "${revision}" \
      '.merge_base_commit.sha == $revision and (.status == "ahead" or .status == "identical")' \
      >/dev/null; then
  :
else
  comparison_status=("${PIPESTATUS[@]}")
  if (( comparison_status[0] != 0 )); then
    echo "failed to fetch comparison for ${tag} against main" >&2
  elif (( comparison_status[1] != 0 )); then
    echo "failed to process comparison for ${tag} against main" >&2
  else
    echo "${tag} is not on main" >&2
  fi
  exit 1
fi
printf '%s\n' "${revision}"
