#!/usr/bin/env bash
# Install the shared "team" Subrouter on a macOS host, with full parity to the
# GCP deployment: a dedicated _subrouter service user, a system LaunchDaemon,
# pf egress isolation, auto-update, and the Claude-reroute self-verifier.
#
# Self-contained: scp just this directory (deploy/macos/) to the host and run
#   sudo ./install-macos.sh
# Idempotent: safe to re-run to upgrade the binary or refresh units.
#
# It does NOT touch account state. Seed /var/lib/subrouter/codex/accounts
# separately (migration copies it from the current live router). By default the
# daemon comes up with no accounts (a healthy but empty standby).
set -euo pipefail

REPO="${SUBROUTER_REPO:-manaflow-ai/subrouter}"
LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter-team}"
ADDR="${SUBROUTER_ADDR:-0.0.0.0:31415}"
STATE_DIR="${SUBROUTER_STATE_DIR:-/var/lib/subrouter}"
SVC_USER="${SUBROUTER_USER:-_subrouter}"
BIN="${SUBROUTER_BIN:-/usr/local/bin/subrouter}"
VERSION="${SUBROUTER_VERSION:-latest}"
VERSION_FILE="/etc/subrouter-version"

# --- Bedrock (Fable) gateway, opt-in --------------------------------------
# Set SUBROUTER_ENABLE_BEDROCK=1 to serve Claude Fable from AWS Bedrock. Supply
# an IAM access key scoped to bedrock:InvokeModel on claude-fable-5 (SigV4; the
# API-key/Mantle path is blocked by the default project's data-retention mode,
# so SigV4 is required). See BEDROCK.md.
ENABLE_BEDROCK="${SUBROUTER_ENABLE_BEDROCK:-0}"
BEDROCK_REGION="${SUBROUTER_BEDROCK_REGION:-us-east-1}"
BEDROCK_PROFILE="${SUBROUTER_BEDROCK_PROFILE:-aw1}"
BEDROCK_GATEWAY_TOKEN="${SUBROUTER_BEDROCK_GATEWAY_TOKEN:-}"
FABLE_BEDROCK_PRIMARY="${SUBROUTER_FABLE_BEDROCK_PRIMARY:-0}"
BEDROCK_AWS_ACCESS_KEY_ID="${SUBROUTER_BEDROCK_AWS_ACCESS_KEY_ID:-}"
BEDROCK_AWS_SECRET_ACCESS_KEY="${SUBROUTER_BEDROCK_AWS_SECRET_ACCESS_KEY:-}"
ANCHOR="ai.manaflow.subrouter"
ANCHOR_DST="/etc/pf.anchors/${ANCHOR}"
LAUNCHD_DIR="/Library/LaunchDaemons"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
log() { echo "install-macos: $*"; }

if [ "$(id -u)" != "0" ]; then
  echo "install-macos.sh must run as root; use sudo" >&2
  exit 1
fi
if [ "$(uname -s)" != "Darwin" ]; then
  echo "install-macos.sh is macOS-only" >&2
  exit 1
fi

case "$(uname -m)" in
  arm64|aarch64) arch="arm64" ;;
  x86_64|amd64) arch="amd64" ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

# --- 1. dedicated service user + group -------------------------------------
find_free_id() {
  local id=440
  local used
  used="$(dscl . -list /Users UniqueID | awk '{print $2}'; dscl . -list /Groups PrimaryGroupID | awk '{print $2}')"
  while grep -qx "$id" <<<"$used"; do id=$((id + 1)); done
  echo "$id"
}
if ! dscl . -read "/Users/${SVC_USER}" >/dev/null 2>&1; then
  uid="$(find_free_id)"
  log "creating service user ${SVC_USER} (uid=${uid})"
  dscl . -create "/Groups/${SVC_USER}"
  dscl . -create "/Groups/${SVC_USER}" PrimaryGroupID "${uid}"
  dscl . -create "/Groups/${SVC_USER}" RealName "Subrouter Service"
  dscl . -create "/Users/${SVC_USER}"
  dscl . -create "/Users/${SVC_USER}" RealName "Subrouter Service"
  dscl . -create "/Users/${SVC_USER}" UniqueID "${uid}"
  dscl . -create "/Users/${SVC_USER}" PrimaryGroupID "${uid}"
  dscl . -create "/Users/${SVC_USER}" UserShell /usr/bin/false
  dscl . -create "/Users/${SVC_USER}" NFSHomeDirectory "${STATE_DIR}"
  dscl . -create "/Users/${SVC_USER}" IsHidden 1
else
  log "service user ${SVC_USER} already exists"
fi
gid="$(dscl . -read "/Users/${SVC_USER}" PrimaryGroupID | awk '{print $2}')"

# --- 2. state + log dirs ----------------------------------------------------
install -d -m 0750 -o "${SVC_USER}" -g "${gid}" "${STATE_DIR}"
install -d -m 0750 -o "${SVC_USER}" -g "${gid}" "${STATE_DIR}/codex" "${STATE_DIR}/codex/accounts"
install -d -m 0750 -o root -g wheel /var/lib/subrouter-verify
for f in /var/log/subrouter.log /var/log/subrouter.err.log; do
  touch "$f"; chown "${SVC_USER}:${gid}" "$f"; chmod 0640 "$f"
done

# --- 3. binary --------------------------------------------------------------
fetch_binary() {
  local ver="$1"
  if [ "$ver" = "latest" ]; then
    ver="$(curl -fsSL -H 'Accept: application/vnd.github+json' \
      "https://api.github.com/repos/${REPO}/releases/latest" \
      | python3 -c 'import json,sys;print(json.load(sys.stdin)["tag_name"])')"
  fi
  [ -n "$ver" ] || { echo "could not resolve version" >&2; exit 1; }
  local v="${ver#v}"
  local asset="subrouter_${v}_darwin_${arch}"
  local base="https://github.com/${REPO}/releases/download/${ver}"
  local tmp; tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' RETURN
  log "downloading ${asset}"
  curl -fsSL -o "${tmp}/${asset}" "${base}/${asset}"
  curl -fsSL -o "${tmp}/SHA256SUMS" "${base}/SHA256SUMS"
  ( cd "$tmp" && grep " ${asset}\$" SHA256SUMS | shasum -a 256 -c - >/dev/null )
  install -m 0755 "${tmp}/${asset}" "${BIN}"
  ln -sf "${BIN}" /usr/local/bin/sr
  ln -sf "${BIN}" /usr/local/bin/cx
  echo "${ver}" > "${VERSION_FILE}"
  log "installed subrouter ${ver} to ${BIN}"
}
fetch_binary "${VERSION}"

# --- 4. helper scripts + pf anchor ------------------------------------------
install -m 0755 "${here}/subrouter-autoupdate.sh" /usr/local/bin/subrouter-autoupdate.sh
install -m 0755 "${here}/subrouter-verify.sh"     /usr/local/bin/subrouter-verify.sh
install -m 0755 "${here}/subrouter-pf.sh"         /usr/local/bin/subrouter-pf.sh
install -d -m 0755 /etc/pf.anchors
install -m 0644 "${here}/pf-subrouter.conf" "${ANCHOR_DST}"

# --- 4b. Bedrock AWS credentials + serve flags ------------------------------
# Extra <string> lines injected into the team plist ProgramArguments.
EXTRA_ARGS_XML=""
if [ "${ENABLE_BEDROCK}" = "1" ]; then
  if [ -z "${BEDROCK_AWS_ACCESS_KEY_ID}" ] || [ -z "${BEDROCK_AWS_SECRET_ACCESS_KEY}" ]; then
    echo "install-macos: SUBROUTER_ENABLE_BEDROCK=1 requires SUBROUTER_BEDROCK_AWS_ACCESS_KEY_ID and SUBROUTER_BEDROCK_AWS_SECRET_ACCESS_KEY" >&2
    exit 1
  fi
  log "installing Bedrock AWS profile '${BEDROCK_PROFILE}' for ${SVC_USER}"
  install -d -m 0700 -o "${SVC_USER}" -g "${gid}" "${STATE_DIR}/.aws"
  umask 077
  printf '[%s]\naws_access_key_id = %s\naws_secret_access_key = %s\n' \
    "${BEDROCK_PROFILE}" "${BEDROCK_AWS_ACCESS_KEY_ID}" "${BEDROCK_AWS_SECRET_ACCESS_KEY}" \
    > "${STATE_DIR}/.aws/credentials"
  printf '[profile %s]\nregion = %s\n' "${BEDROCK_PROFILE}" "${BEDROCK_REGION}" \
    > "${STATE_DIR}/.aws/config"
  chown "${SVC_USER}:${gid}" "${STATE_DIR}/.aws/credentials" "${STATE_DIR}/.aws/config"
  chmod 0600 "${STATE_DIR}/.aws/credentials" "${STATE_DIR}/.aws/config"

  add_arg() { EXTRA_ARGS_XML="${EXTRA_ARGS_XML}		<string>$1</string>"$'\n'; }
  add_arg "--bedrock"
  add_arg "--bedrock-region"; add_arg "${BEDROCK_REGION}"
  add_arg "--bedrock-profiles"; add_arg "${BEDROCK_PROFILE}"
  if [ -n "${BEDROCK_GATEWAY_TOKEN}" ]; then
    add_arg "--bedrock-gateway-token"; add_arg "${BEDROCK_GATEWAY_TOKEN}"
  fi
  if [ "${FABLE_BEDROCK_PRIMARY}" = "1" ]; then
    add_arg "--fable-bedrock-primary"
  fi
  # strip trailing newline
  EXTRA_ARGS_XML="${EXTRA_ARGS_XML%$'\n'}"
fi

# --- 5. LaunchDaemons -------------------------------------------------------
render_team_plist() {
  ADDR="${ADDR}" EXTRA_ARGS_XML="${EXTRA_ARGS_XML}" python3 - \
    "${here}/ai.manaflow.subrouter-team.plist" \
    "${LAUNCHD_DIR}/ai.manaflow.subrouter-team.plist" <<'PY'
import os, sys
src, dst = sys.argv[1], sys.argv[2]
text = open(src).read().replace("__ADDR__", os.environ["ADDR"])
extra = os.environ.get("EXTRA_ARGS_XML", "")
# Replace only the standalone __EXTRA_ARGS__ line (never a substring), so args
# containing "--" never leak into the XML comment above.
out = []
for line in text.splitlines():
    if line.strip() == "__EXTRA_ARGS__":
        if extra:
            out.append(extra)
        # else: drop the line
    else:
        out.append(line)
open(dst, "w").write("\n".join(out) + "\n")
PY
}
render_team_plist
install -m 0644 "${here}/ai.manaflow.subrouter-pf.plist"         "${LAUNCHD_DIR}/"
install -m 0644 "${here}/ai.manaflow.subrouter-autoupdate.plist" "${LAUNCHD_DIR}/"
install -m 0644 "${here}/ai.manaflow.subrouter-verify.plist"     "${LAUNCHD_DIR}/"
chown root:wheel "${LAUNCHD_DIR}/ai.manaflow.subrouter-"*.plist
chmod 0644 "${LAUNCHD_DIR}/ai.manaflow.subrouter-"*.plist

boot() { # label plist
  local label="$1" plist="$2"
  launchctl bootout "system/${label}" 2>/dev/null || true
  launchctl bootstrap system "${plist}"
  launchctl enable "system/${label}" 2>/dev/null || true
}
boot ai.manaflow.subrouter-pf         "${LAUNCHD_DIR}/ai.manaflow.subrouter-pf.plist"
boot ai.manaflow.subrouter-team       "${LAUNCHD_DIR}/ai.manaflow.subrouter-team.plist"
boot ai.manaflow.subrouter-autoupdate "${LAUNCHD_DIR}/ai.manaflow.subrouter-autoupdate.plist"
boot ai.manaflow.subrouter-verify     "${LAUNCHD_DIR}/ai.manaflow.subrouter-verify.plist"

# --- 6. health --------------------------------------------------------------
log "waiting for health on 127.0.0.1:31415"
i=0
until curl -fsS http://127.0.0.1:31415/_subrouter/health >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 30 ]; then
    echo "install-macos: health check failed; see /var/log/subrouter.err.log" >&2
    exit 1
  fi
  sleep 1
done
log "healthy. daemon ${LABEL} listening on ${ADDR}"
log "accounts dir: ${STATE_DIR}/codex/accounts (seed via migration)"
