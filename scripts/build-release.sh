#!/usr/bin/env bash
set -euo pipefail

version="${1:-0.1.0}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out_dir="${root}/dist/release"

rm -rf "${out_dir}"
mkdir -p "${out_dir}"

build() {
  local goos="$1"
  local goarch="$2"
  local goarm="${3:-}"
  local suffix=""
  local arch_name="${goarch}"
  if [[ -n "${goarm}" ]]; then
    arch_name="armv${goarm}"
  fi
  if [[ "${goos}" == "windows" ]]; then
    suffix=".exe"
  fi
  local output="${out_dir}/subrouter_${version}_${goos}_${arch_name}${suffix}"
  local commit
  commit="$(git -C "${root}" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
  local built
  built="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  local ldflags="-s -w -X main.version=${version} -X main.commit=${commit} -X main.buildDate=${built}"
  echo "building ${output}"
  if [[ -n "${goarm}" ]]; then
    GOOS="${goos}" GOARCH="${goarch}" GOARM="${goarm}" CGO_ENABLED=0 go build -trimpath -ldflags="${ldflags}" -o "${output}" ./cmd/subrouter
  else
    GOOS="${goos}" GOARCH="${goarch}" CGO_ENABLED=0 go build -trimpath -ldflags="${ldflags}" -o "${output}" ./cmd/subrouter
  fi
}

build darwin amd64
build darwin arm64
build linux amd64
build linux arm64
build linux 386
build linux arm 6
build linux arm 7
build windows amd64
build windows arm64
build windows 386
build freebsd amd64
build freebsd arm64
build freebsd 386
build openbsd amd64
build openbsd arm64
build netbsd amd64
build netbsd arm64

(cd "${out_dir}" && shasum -a 256 subrouter_* > SHA256SUMS)
