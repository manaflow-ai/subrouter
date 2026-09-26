#!/usr/bin/env bash
set -euo pipefail

asset_dir="${1:?usage: publish-github-release.sh <release-asset-directory>}"
: "${GH_REPO:?GH_REPO is required}"
: "${TAG_NAME:?TAG_NAME is required}"
: "${SOURCE_COMMIT:?SOURCE_COMMIT is required}"

asset_dir="$(cd "${asset_dir}" && pwd)"
[[ -f "${asset_dir}/SHA256SUMS" ]] || {
  echo "release checksum manifest is missing" >&2
  exit 1
}

(
  cd "${asset_dir}"
  sha256sum --check SHA256SUMS
)

work_dir="$(mktemp -d)"
local_assets="${work_dir}/local-assets.tsv"
remote_assets="${work_dir}/remote-assets.tsv"
release_error="${work_dir}/release-view.stderr"
verification_dir="${work_dir}/attestation-verification"
mkdir -p "${verification_dir}"

verify_immutable_asset() {
  local asset="$1"
  local max_attempts=12
  local retry_delay_seconds=5
  local attempt

  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    if gh release verify-asset "${TAG_NAME}" "${asset}" \
        --repo "${GH_REPO}" --format json >/dev/null
    then
      return 0
    fi
    if (( attempt == max_attempts )); then
      echo "immutable release asset verification failed after ${max_attempts} attempts: $(basename "${asset}")" >&2
      return 1
    fi
    echo "immutable release asset verification is pending for $(basename "${asset}"); retrying (${attempt}/${max_attempts})" >&2
    sleep "${retry_delay_seconds}"
  done
}

release_state=""
release_found=false
if release_state="$(gh release view "${TAG_NAME}" --repo "${GH_REPO}" --json isDraft,isImmutable 2>"${release_error}")"; then
  release_found=true
elif [[ "$(<"${release_error}")" != "release not found" ]]; then
  cat "${release_error}" >&2
  exit 1
fi

published_immutable=false
if [[ "${release_found}" == true ]]; then
  if jq -e '.isDraft == true and .isImmutable == false' <<<"${release_state}" >/dev/null; then
    gh release delete "${TAG_NAME}" --repo "${GH_REPO}" --yes
  elif jq -e '.isDraft == false and .isImmutable == true' <<<"${release_state}" >/dev/null; then
    published_immutable=true
  else
    echo "refusing to mutate published non-immutable release ${TAG_NAME}" >&2
    exit 1
  fi
fi

if [[ "${published_immutable}" != true ]]; then
  gh release create "${TAG_NAME}" --repo "${GH_REPO}" --draft --verify-tag \
    --title "Subrouter ${TAG_NAME}" \
    --notes "Subrouter ${TAG_NAME}"
  gh release upload "${TAG_NAME}" --repo "${GH_REPO}" "${asset_dir}"/*
fi

while IFS= read -r -d '' asset; do
  digest="$(sha256sum "${asset}" | awk '{print $1}')"
  printf '%s\tsha256:%s\n' "$(basename "${asset}")" "${digest}"
done < <(find "${asset_dir}" -maxdepth 1 -type f -print0 | sort -z) \
  | LC_ALL=C sort >"${local_assets}"

gh release view "${TAG_NAME}" --repo "${GH_REPO}" --json isDraft,isImmutable,assets \
  --jq 'if (.isDraft == true or (.isDraft == false and .isImmutable == true)) then .assets[] | [.name,.digest] | @tsv else error("release state changed before verification") end' \
  | LC_ALL=C sort >"${remote_assets}"
diff -u "${local_assets}" "${remote_assets}"

while IFS= read -r -d '' asset; do
  name="$(basename "${asset}")"
  verification="$(gh attestation verify "${asset}" \
    --repo "${GH_REPO}" \
    --signer-workflow "${GH_REPO}/.github/workflows/release.yml" \
    --source-ref "refs/tags/${TAG_NAME}" \
    --source-digest "${SOURCE_COMMIT}" \
    --deny-self-hosted-runners --format json)"
  jq --arg asset "${name}" --arg source_revision "${SOURCE_COMMIT}" \
    '{asset:$asset,source_revision:$source_revision,verified_attestations:length,
      subjects:[.[].verificationResult.statement.subject[] | {name,digest}]}' \
    <<<"${verification}" >"${verification_dir}/${name}.json"
  jq -e '.verified_attestations > 0' "${verification_dir}/${name}.json" >/dev/null
done < <(find "${asset_dir}" -maxdepth 1 -type f -print0 | sort -z)

if [[ "${published_immutable}" != true ]]; then
  gh release edit "${TAG_NAME}" --repo "${GH_REPO}" --draft=false
fi
gh release view "${TAG_NAME}" --repo "${GH_REPO}" --json isDraft,isImmutable \
  --jq '(.isDraft == false and .isImmutable == true) or error("published release is not immutable")'
while IFS= read -r -d '' asset; do
  verify_immutable_asset "${asset}"
done < <(find "${asset_dir}" -maxdepth 1 -type f -print0 | sort -z)
