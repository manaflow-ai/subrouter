#!/bin/sh
set -eu

repo="manaflow-ai/subrouter"
version="${SUBROUTER_VERSION:-latest}"
install_dir="${SUBROUTER_INSTALL_DIR:-}"
install_aliases="${SUBROUTER_INSTALL_ALIASES:-1}"
version_file="${SUBROUTER_VERSION_FILE:-}"

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "$1 is required" >&2
    exit 1
  fi
}

need curl
need uname

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$os" in
  darwin) goos="darwin" ;;
  linux) goos="linux" ;;
  freebsd) goos="freebsd" ;;
  netbsd) goos="netbsd" ;;
  openbsd) goos="openbsd" ;;
  *)
    echo "unsupported OS: $os" >&2
    exit 1
    ;;
esac

case "$arch" in
  x86_64|amd64) goarch="amd64" ;;
  arm64|aarch64) goarch="arm64" ;;
  i386|i686) goarch="386" ;;
  armv6l) goarch="armv6" ;;
  armv7l|armv7) goarch="armv7" ;;
  *)
    echo "unsupported architecture: $arch" >&2
    exit 1
    ;;
esac

if [ "$version" = "latest" ]; then
  version="$(
    curl -fsSIL -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest" |
      sed -n 's#.*/tag/v\{0,1\}\([^/?#]*\).*#\1#p' |
      head -n 1
  )"
  if [ -z "$version" ]; then
    version="$(
      curl -fsSL "https://api.github.com/repos/$repo/releases/latest" |
        sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\{0,1\}\([^"]*\)".*/\1/p' |
        head -n 1
    )"
  fi
fi

if [ -z "$version" ]; then
  echo "could not resolve Subrouter version" >&2
  exit 1
fi
version="${version#v}"
case "$version" in
  ""|-*|*[!0-9A-Za-z.-]*)
    echo "invalid Subrouter version: $version" >&2
    exit 1
    ;;
esac
case "$install_aliases" in
  0|1) ;;
  *)
    echo "SUBROUTER_INSTALL_ALIASES must be 0 or 1" >&2
    exit 1
    ;;
esac

asset="subrouter_${version}_${goos}_${goarch}"
base_url="${SUBROUTER_DOWNLOAD_BASE:-https://github.com/$repo/releases/download/v$version}"

if [ -z "$install_dir" ]; then
  if [ "$(id -u)" = "0" ]; then
    install_dir="/usr/local/bin"
  else
    install_dir="$HOME/bin"
  fi
fi

# GCP auto-update and verification compare this marker with the release bytes
# on disk. Non-root installs remain side-effect free unless the caller chooses
# a version-file path explicitly.
if [ -z "$version_file" ] && [ "$goos" = "linux" ] && [ "$(id -u)" = "0" ]; then
  version_file="/etc/subrouter-version"
fi

tmp_dir="$(mktemp -d)"
staged_binary=""
staged_version=""
cleanup() {
  [ -z "$staged_binary" ] || rm -f -- "$staged_binary"
  [ -z "$staged_version" ] || rm -f -- "$staged_version"
  rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

curl -fsSL "$base_url/$asset" -o "$tmp_dir/subrouter"
curl -fsSL "$base_url/SHA256SUMS" -o "$tmp_dir/SHA256SUMS"

if command -v sha256sum >/dev/null 2>&1; then
  expected="$(awk -v asset="$asset" '$2 == asset || $2 == "*" asset {print $1}' "$tmp_dir/SHA256SUMS")"
  actual="$(sha256sum "$tmp_dir/subrouter" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  expected="$(awk -v asset="$asset" '$2 == asset || $2 == "*" asset {print $1}' "$tmp_dir/SHA256SUMS")"
  actual="$(shasum -a 256 "$tmp_dir/subrouter" | awk '{print $1}')"
else
  echo "sha256sum or shasum is required" >&2
  exit 1
fi

checksum_count="$(printf '%s\n' "$expected" | awk 'NF {count++} END {print count + 0}')"
if [ "$checksum_count" != "1" ]; then
  echo "expected exactly one checksum for $asset, found $checksum_count" >&2
  exit 1
fi
if [ "$expected" != "$actual" ]; then
  echo "checksum mismatch for $asset" >&2
  exit 1
fi

# Stage every required destination before replacing the working binary. This
# prevents a read-only or malformed version-marker path from turning a failed
# install into a successful binary update with stale metadata.
mkdir -p "$install_dir"
staged_binary="$(mktemp "$install_dir/.subrouter.install.XXXXXX")"
install -m 0755 "$tmp_dir/subrouter" "$staged_binary"
if [ -n "$version_file" ]; then
  version_dir="$(dirname "$version_file")"
  mkdir -p "$version_dir"
  if { [ -e "$version_file" ] || [ -L "$version_file" ]; } && [ ! -f "$version_file" ]; then
    echo "version marker target is not a regular file: $version_file" >&2
    exit 1
  fi
  staged_version="$(mktemp "$version_dir/.subrouter-version.install.XXXXXX")"
  printf 'v%s\n' "$version" >"$staged_version"
  chmod 0644 "$staged_version"
fi

binary_path="$install_dir/subrouter"
had_binary=0
if [ -e "$binary_path" ]; then
  [ -f "$binary_path" ] || {
    echo "install target is not a regular file: $binary_path" >&2
    exit 1
  }
  cp -p "$binary_path" "$tmp_dir/previous-subrouter"
  had_binary=1
fi

mv -f "$staged_binary" "$binary_path"
staged_binary=""
if [ -n "$version_file" ]; then
  if ! mv -f "$staged_version" "$version_file"; then
    if [ "$had_binary" = "1" ]; then
      install -m 0755 "$tmp_dir/previous-subrouter" "$binary_path.rollback"
      mv -f "$binary_path.rollback" "$binary_path"
    else
      rm -f "$binary_path"
    fi
    echo "could not install version marker; restored the previous binary" >&2
    exit 1
  fi
  staged_version=""
fi

if [ "$install_aliases" = "1" ]; then
  ln -sf "$binary_path" "$install_dir/sr"
  ln -sf "$binary_path" "$install_dir/cx"
fi

echo "Installed Subrouter $version to $binary_path"
if [ "$install_aliases" = "1" ]; then
  echo "Installed aliases: $install_dir/sr, $install_dir/cx"
fi
