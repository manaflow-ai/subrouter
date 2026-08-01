#!/bin/sh
set -eu

repo="manaflow-ai/subrouter"
version="${SUBROUTER_VERSION:-latest}"
install_dir="${SUBROUTER_INSTALL_DIR:-}"
install_aliases="${SUBROUTER_INSTALL_ALIASES:-1}"

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
  darwin) platform="darwin" ;;
  linux) platform="linux" ;;
  *)
    echo "unsupported OS: $os" >&2
    exit 1
    ;;
esac

case "$arch" in
  x86_64|amd64) release_arch="amd64" ;;
  arm64|aarch64) release_arch="arm64" ;;
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

asset="subrouter_${version}_${platform}_${release_arch}"
base_url="${SUBROUTER_DOWNLOAD_BASE:-https://github.com/$repo/releases/download/v$version}"

if [ -z "$install_dir" ]; then
  if [ "$(id -u)" = "0" ]; then
    install_dir="/usr/local/bin"
  else
    install_dir="$HOME/bin"
  fi
fi

tmp_dir="$(mktemp -d)"
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

curl -fsSL "$base_url/$asset" -o "$tmp_dir/subrouter"
curl -fsSL "$base_url/SHA256SUMS" -o "$tmp_dir/SHA256SUMS"

if command -v sha256sum >/dev/null 2>&1; then
  expected="$(grep "  $asset\$" "$tmp_dir/SHA256SUMS" | awk '{print $1}')"
  actual="$(sha256sum "$tmp_dir/subrouter" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  expected="$(grep "  $asset\$" "$tmp_dir/SHA256SUMS" | awk '{print $1}')"
  actual="$(shasum -a 256 "$tmp_dir/subrouter" | awk '{print $1}')"
else
  expected=""
  actual=""
  echo "warning: sha256sum or shasum not found; skipping checksum verification" >&2
fi

if [ -n "$actual" ]; then
  if [ -z "$expected" ]; then
    echo "checksum not found for $asset" >&2
    exit 1
  fi
  if [ "$expected" != "$actual" ]; then
    echo "checksum mismatch for $asset" >&2
    exit 1
  fi
fi

mkdir -p "$install_dir"
chmod 0755 "$tmp_dir/subrouter"
install "$tmp_dir/subrouter" "$install_dir/subrouter"
if [ "$install_aliases" = "1" ]; then
  ln -sf "$install_dir/subrouter" "$install_dir/sr"
  ln -sf "$install_dir/subrouter" "$install_dir/cx"
fi

echo "Installed Subrouter $version to $install_dir/subrouter"
if [ "$install_aliases" = "1" ]; then
  echo "Installed aliases: $install_dir/sr, $install_dir/cx"
fi
