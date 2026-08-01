#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="${1:-$(sed -n 's/^version = "\([^"]*\)"/\1/p' "${root}/Cargo.toml" | head -n 1)}"
if [[ "$#" -gt 0 ]]; then
  shift
fi
if [[ -z "${version}" ]]; then
  echo "release version is empty" >&2
  exit 1
fi
out_dir="${SUBROUTER_DIST_DIR:-${root}/dist/release}"

mkdir -p "${out_dir}"

build() {
  local target="$1"
  local platform=""
  local arch=""
  local suffix=""
  case "${target}" in
    x86_64-apple-darwin) platform="darwin"; arch="amd64" ;;
    aarch64-apple-darwin) platform="darwin"; arch="arm64" ;;
    x86_64-unknown-linux-musl|x86_64-unknown-linux-gnu) platform="linux"; arch="amd64" ;;
    aarch64-unknown-linux-musl|aarch64-unknown-linux-gnu) platform="linux"; arch="arm64" ;;
    i686-unknown-linux-musl|i686-unknown-linux-gnu) platform="linux"; arch="386" ;;
    arm-unknown-linux-musleabihf|arm-unknown-linux-gnueabihf) platform="linux"; arch="armv6" ;;
    armv7-unknown-linux-musleabihf|armv7-unknown-linux-gnueabihf) platform="linux"; arch="armv7" ;;
    x86_64-pc-windows-msvc|x86_64-pc-windows-gnu|x86_64-pc-windows-gnullvm) platform="windows"; arch="amd64"; suffix=".exe" ;;
    aarch64-pc-windows-msvc|aarch64-pc-windows-gnullvm) platform="windows"; arch="arm64"; suffix=".exe" ;;
    i686-pc-windows-msvc|i686-pc-windows-gnu|i686-pc-windows-gnullvm) platform="windows"; arch="386"; suffix=".exe" ;;
    x86_64-unknown-freebsd) platform="freebsd"; arch="amd64" ;;
    aarch64-unknown-freebsd) platform="freebsd"; arch="arm64" ;;
    i686-unknown-freebsd) platform="freebsd"; arch="386" ;;
    x86_64-unknown-openbsd) platform="openbsd"; arch="amd64" ;;
    aarch64-unknown-openbsd) platform="openbsd"; arch="arm64" ;;
    x86_64-unknown-netbsd) platform="netbsd"; arch="amd64" ;;
    aarch64-unknown-netbsd) platform="netbsd"; arch="arm64" ;;
    *) echo "unsupported Rust target: ${target}" >&2; exit 1 ;;
  esac

  local source="${root}/target/${target}/release/subrouter${suffix}"
  local output="${out_dir}/subrouter_${version}_${platform}_${arch}${suffix}"
  echo "building ${target} -> ${output}"
  cargo build --locked --release --bin subrouter --target "${target}"
  cp "${source}" "${output}"
}

if [[ "$#" -eq 0 ]]; then
  host="$(rustc -vV | sed -n 's/^host: //p')"
  build "${host}"
else
  for target in "$@"; do
    build "${target}"
  done
fi

if command -v sha256sum >/dev/null 2>&1; then
  (cd "${out_dir}" && sha256sum "subrouter_${version}_"* > SHA256SUMS)
else
  (cd "${out_dir}" && shasum -a 256 "subrouter_${version}_"* > SHA256SUMS)
fi
