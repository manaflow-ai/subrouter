#!/usr/bin/env bash
# Install the per-user CLI updater as a LaunchAgent. Run once per machine:
#
#   curl -fsSL https://raw.githubusercontent.com/manaflow-ai/subrouter/main/deploy/macos/install-cli-autoupdate.sh | bash
#
# After this the machine keeps its own `sr` current, so nobody has to remember
# to reinstall after a release.
set -euo pipefail

REPO="${SUBROUTER_REPO:-manaflow-ai/subrouter}"
LABEL="${SUBROUTER_CLI_UPDATE_LABEL:-ai.manaflow.subrouter-cli-autoupdate}"
SCRIPT_PATH="${SUBROUTER_CLI_UPDATE_SCRIPT:-$HOME/.subrouter/bin/subrouter-cli-autoupdate.sh}"
PLIST="$HOME/Library/LaunchAgents/${LABEL}.plist"
LOG_DIR="${SUBROUTER_CLI_UPDATE_LOG_DIR:-$HOME/Library/Logs}"
INTERVAL="${SUBROUTER_CLI_UPDATE_INTERVAL:-86400}"
SCRIPT_URL="${SUBROUTER_CLI_UPDATE_SCRIPT_URL:-https://raw.githubusercontent.com/$REPO/main/deploy/macos/subrouter-cli-autoupdate.sh}"

[ "$(uname -s)" = "Darwin" ] || { echo "macOS only" >&2; exit 1; }
[ "$(id -u)" != "0" ] || { echo "run as your own user, not root: the agent updates a per-user CLI" >&2; exit 1; }

mkdir -p "$(dirname "$SCRIPT_PATH")" "$LOG_DIR" "$HOME/Library/LaunchAgents"
if [ -f "${SUBROUTER_CLI_UPDATE_LOCAL_SCRIPT:-}" ]; then
  install -m 0755 "$SUBROUTER_CLI_UPDATE_LOCAL_SCRIPT" "$SCRIPT_PATH"
else
  curl -fsSL "$SCRIPT_URL" -o "$SCRIPT_PATH.new"
  chmod 0755 "$SCRIPT_PATH.new"
  mv -f "$SCRIPT_PATH.new" "$SCRIPT_PATH"
fi

cat >"$PLIST.new" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>${LABEL}</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/bash</string>
		<string>${SCRIPT_PATH}</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>StartInterval</key>
	<integer>${INTERVAL}</integer>
	<key>StandardOutPath</key>
	<string>${LOG_DIR}/subrouter-cli-autoupdate.log</string>
	<key>StandardErrorPath</key>
	<string>${LOG_DIR}/subrouter-cli-autoupdate.err.log</string>
	<key>ProcessType</key>
	<string>Background</string>
	<key>LowPriorityIO</key>
	<true/>
</dict>
</plist>
PLIST_EOF
mv -f "$PLIST.new" "$PLIST"
plutil -lint "$PLIST" >/dev/null

service="gui/$(id -u)/${LABEL}"
launchctl bootout "$service" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"

echo "Installed ${LABEL}; it checks for a new release every $((INTERVAL / 3600))h and logs to ${LOG_DIR}/subrouter-cli-autoupdate.log"
