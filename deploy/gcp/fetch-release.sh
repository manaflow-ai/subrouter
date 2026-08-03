#!/usr/bin/env bash
# Download one immutable Linux release candidate and publish it locally only
# after its release checksum matches.
set -euo pipefail

REPO="${SUBROUTER_REPO:-manaflow-ai/subrouter}"
RELEASE_TAG="${SUBROUTER_RELEASE_TAG:?set SUBROUTER_RELEASE_TAG to an explicit version tag such as v0.1.52}"
RELEASE_ARCH="${SUBROUTER_RELEASE_ARCH:-amd64}"
OUTPUT="${SUBROUTER_RELEASE_OUTPUT:?set SUBROUTER_RELEASE_OUTPUT}"

[[ "${RELEASE_TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || {
  echo "fetch-release: SUBROUTER_RELEASE_TAG must be a versioned tag such as v0.1.52" >&2
  exit 1
}
[[ "${RELEASE_ARCH}" == "amd64" || "${RELEASE_ARCH}" == "arm64" ]] || {
  echo "fetch-release: unsupported Linux architecture: ${RELEASE_ARCH}" >&2
  exit 1
}

version="${RELEASE_TAG#v}"
asset="subrouter_${version}_linux_${RELEASE_ARCH}"
base="${SUBROUTER_RELEASE_BASE:-https://github.com/${REPO}/releases/download/${RELEASE_TAG}}"
output_dir="$(dirname "${OUTPUT}")"
mkdir -p "${output_dir}"
if [[ -e "${OUTPUT}" || -L "${OUTPUT}" ]]; then
  [[ -f "${OUTPUT}" ]] || {
    echo "fetch-release: output target is not a regular file: ${OUTPUT}" >&2
    exit 1
  }
fi
tmp="$(mktemp -d "${output_dir}/.subrouter-release.XXXXXX")"
cleanup() {
  rm -rf -- "${tmp}"
}
trap cleanup EXIT INT TERM

curl -fsSL --retry 3 --retry-all-errors -o "${tmp}/${asset}" "${base}/${asset}"
curl -fsSL --retry 3 --retry-all-errors -o "${tmp}/SHA256SUMS" "${base}/SHA256SUMS"

awk -v asset="${asset}" '$2 == asset || $2 == "*" asset {print $1 "  " asset}' \
  "${tmp}/SHA256SUMS" >"${tmp}/candidate.SHA256SUM"
[[ "$(wc -l <"${tmp}/candidate.SHA256SUM" | tr -d '[:space:]')" == "1" ]] || {
  echo "fetch-release: SHA256SUMS must contain exactly one checksum for ${asset}" >&2
  exit 1
}
expected="$(awk '{print $1}' "${tmp}/candidate.SHA256SUM")"
[[ "${expected}" =~ ^[0-9a-fA-F]{64}$ ]] || {
  echo "fetch-release: invalid checksum for ${asset}" >&2
  exit 1
}
(cd "${tmp}" && sha256sum -c candidate.SHA256SUM >/dev/null)

candidate="$(mktemp "${output_dir}/.subrouter-candidate.XXXXXX")"
trap 'rm -f -- "${candidate}"; cleanup' EXIT INT TERM
install -m 0755 "${tmp}/${asset}" "${candidate}"
actual="$(sha256sum "${candidate}" | awk '{print $1}')"
[[ "${actual}" == "${expected}" ]] || {
  echo "fetch-release: staged candidate checksum changed" >&2
  exit 1
}
mv -f -- "${candidate}" "${OUTPUT}"
printf '%s\n' "${expected}" >"${OUTPUT}.sha256"
trap cleanup EXIT INT TERM
echo "fetch-release: verified ${RELEASE_TAG} ${asset} (${expected})"
