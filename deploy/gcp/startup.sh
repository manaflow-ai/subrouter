#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y ca-certificates curl jq util-linux

metadata_url="http://metadata.google.internal/computeMetadata/v1/instance/attributes/subrouter-release-tag"
release_tag="$(curl -fsSL -H 'Metadata-Flavor: Google' "${metadata_url}")"
if [[ ! "${release_tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  echo "startup: instance metadata subrouter-release-tag is not an explicit version tag" >&2
  exit 1
fi
release_base="https://github.com/manaflow-ai/subrouter/releases/download/${release_tag}"
curl -fsSL "${release_base}/install.sh" \
  | env SUBROUTER_VERSION="${release_tag}" SUBROUTER_DOWNLOAD_BASE="${release_base}" sh
# Provision the unit without exposing a token-free control plane. The operator
# publish step sends distinct admin and import tokens over IAP, then starts it.
/usr/local/bin/sr install-systemd --addr 0.0.0.0:31415 --cx-switch-interval 10m --start=false

# Install the Claude rate-limit reroute self-verifier so it survives VM rebuilds.
# Best-effort: a fetch failure must never abort provisioning of the proxy itself.
install_subrouter_verify() {
  local base="https://raw.githubusercontent.com/manaflow-ai/subrouter/${release_tag}/deploy/gcp"
  curl -fsSL "${base}/subrouter-verify.sh" -o /usr/local/bin/subrouter-verify.sh || return 1
  chmod 0755 /usr/local/bin/subrouter-verify.sh
  curl -fsSL "${base}/subrouter-verify.service" -o /etc/systemd/system/subrouter-verify.service || return 1
  curl -fsSL "${base}/subrouter-verify.timer" -o /etc/systemd/system/subrouter-verify.timer || return 1
  systemctl daemon-reload
  systemctl enable --now subrouter-verify.timer
}
install_subrouter_verify || echo "startup: subrouter-verify install failed (non-fatal)"
