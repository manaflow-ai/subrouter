#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y ca-certificates curl

curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | env SUBROUTER_VERSION="${SUBROUTER_VERSION:-latest}" sh
/usr/local/bin/sr install-systemd --addr 0.0.0.0:31415 --cx-switch-interval 10m

# Install the Claude rate-limit reroute self-verifier so it survives VM rebuilds.
# Best-effort: a fetch failure must never abort provisioning of the proxy itself.
install_subrouter_verify() {
  local base="https://raw.githubusercontent.com/manaflow-ai/subrouter/main/deploy/gcp"
  curl -fsSL "${base}/subrouter-verify.sh" -o /usr/local/bin/subrouter-verify.sh || return 1
  chmod 0755 /usr/local/bin/subrouter-verify.sh
  curl -fsSL "${base}/subrouter-verify.service" -o /etc/systemd/system/subrouter-verify.service || return 1
  curl -fsSL "${base}/subrouter-verify.timer" -o /etc/systemd/system/subrouter-verify.timer || return 1
  systemctl daemon-reload
  systemctl enable --now subrouter-verify.timer
}
install_subrouter_verify || echo "startup: subrouter-verify install failed (non-fatal)"
