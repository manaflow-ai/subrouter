#!/usr/bin/env bash
# Exercises subrouter-cli-autoupdate.sh (the laptop LaunchAgent updater)
# against fake `subrouter` binaries: one that has `update --auto`, which the
# updater must hand over to, and older ones, which must still go through
# install.sh. HOME, PATH and every URL point into a throwaway directory, and a
# curl wrapper fails any http(s) request, so nothing here reaches the network
# or a real local daemon.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPDATER="$HERE/../subrouter-cli-autoupdate.sh"
REAL_CURL="$(command -v curl)"
failures=0

check() { # check <description> <condition-result>
  if [ "$2" -eq 0 ]; then
    printf 'ok   %s\n' "$1"
  else
    printf 'FAIL %s\n' "$1"
    failures=$((failures + 1))
  fi
}

setup() {
  ROOT="$(mktemp -d)"
  mkdir -p "$ROOT/home" "$ROOT/fakebin" "$ROOT/install" "$ROOT/fixture"
  # Only file:// is allowed through; anything else is recorded and fails.
  cat >"$ROOT/fakebin/curl" <<EOF
#!/bin/sh
for arg in "\$@"; do
  case "\$arg" in
    http://*|https://*) echo "\$arg" >>"$ROOT/network.log"; exit 7 ;;
  esac
done
echo "\$*" >>"$ROOT/curl.log"
exec "$REAL_CURL" "\$@"
EOF
  chmod +x "$ROOT/fakebin/curl"
  printf '{"tag_name":"v9.9.9"}\n' >"$ROOT/fixture/release.json"
  # Stands in for install.sh: records its environment and installs a binary.
  cat >"$ROOT/fixture/install.sh" <<EOF
echo "version=\$SUBROUTER_VERSION dir=\$SUBROUTER_INSTALL_DIR" >>"$ROOT/install.log"
mkdir -p "\$SUBROUTER_INSTALL_DIR"
printf '#!/bin/sh\nexit 0\n' >"\$SUBROUTER_INSTALL_DIR/subrouter"
chmod +x "\$SUBROUTER_INSTALL_DIR/subrouter"
mkdir -p "\$(dirname "\$SUBROUTER_VERSION_FILE")"
echo "\$SUBROUTER_VERSION" >"\$SUBROUTER_VERSION_FILE"
EOF
}

teardown() {
  [ ! -e "$ROOT/network.log" ]
  check "  ...and made no http(s) request" $?
  rm -rf "$ROOT"
}

# fake_sr <help-text-or-empty> [update-exit-status]: an installed CLI that
# reports v0.1.100 and records every call.
fake_sr() {
  cat >"$ROOT/install/subrouter" <<EOF
#!/bin/sh
echo "\$* NO_AUTO=\${SUBROUTER_NO_AUTO_UPDATE:-}" >>"$ROOT/sr.log"
case "\$1" in
  version) echo "subrouter v0.1.100"; exit 0 ;;
  update)
    if [ "\$2" = "--help" ]; then
      [ -n "$1" ] && printf '%s\n' "$1"
      exit ${2:-0}
    fi
    exit 0 ;;
esac
exit 0
EOF
  chmod +x "$ROOT/install/subrouter"
}

run_updater() {
  env -i \
    HOME="$ROOT/home" \
    PATH="$ROOT/fakebin:/usr/bin:/bin" \
    SUBROUTER_INSTALL_DIR="$ROOT/install" \
    SUBROUTER_VERSION_FILE="$ROOT/home/.subrouter/cli-version" \
    SUBROUTER_RELEASE_API_URL="file://$ROOT/fixture/release.json" \
    SUBROUTER_RELEASE_LATEST_URL="file://$ROOT/fixture/none" \
    SUBROUTER_INSTALL_URL="file://$ROOT/fixture/install.sh" \
    SUBROUTER_DOWNLOAD_BASE="file://$ROOT/fixture" \
    bash "$UPDATER" >"$ROOT/out.log" 2>&1
}

AUTO_HELP='Usage:
  sr update [--version vX.Y.Z] [--check] [--yes]
  sr update --auto'
OLD_HELP='Usage:
  sr update [--version vX.Y.Z] [--check] [--yes]'

# A CLI with `update --auto` is handed the update, and the updater itself
# downloads nothing.
test_delegates_to_update_auto() {
  setup
  fake_sr "$AUTO_HELP"
  run_updater
  check "auto: updater exits 0" $?
  grep -qx 'update --auto NO_AUTO=' "$ROOT/sr.log"
  check "auto: runs subrouter update --auto" $?
  grep -qx 'update --help NO_AUTO=1' "$ROOT/sr.log"
  check "auto: the help probe cannot start a background update" $?
  grep -q 'subrouter-cli-autoupdate: running .*/subrouter update --auto' "$ROOT/out.log"
  check "auto: logs the hand-over" $?
  [ ! -e "$ROOT/curl.log" ] && [ ! -e "$ROOT/install.log" ]
  check "auto: no release lookup and no install.sh" $?
  teardown
}

# A CLI whose `update` predates --auto still goes through install.sh, onto a
# release that has it.
test_falls_back_without_auto() {
  setup
  fake_sr "$OLD_HELP"
  run_updater
  check "old: updater exits 0" $?
  ! grep -q 'update --auto' "$ROOT/sr.log"
  check "old: never runs update --auto" $?
  grep -qx 'version=v9.9.9 dir='"$ROOT"'/install' "$ROOT/install.log"
  check "old: installs the latest release through install.sh" $?
  grep -q 'updating CLI none -> v9.9.9' "$ROOT/out.log"
  check "old: logs the install.sh update" $?
  teardown
}

# A CLI with no `update` command at all fails the probe, which is the same as
# not supporting it.
test_falls_back_when_update_is_unknown() {
  setup
  fake_sr "" 1
  run_updater
  check "pre-update CLI: updater exits 0" $?
  [ -s "$ROOT/install.log" ]
  check "pre-update CLI: installs through install.sh" $?
  teardown
}

test_falls_back_when_binary_missing() {
  setup
  run_updater
  check "missing CLI: updater exits 0" $?
  [ -s "$ROOT/install.log" ] && [ -x "$ROOT/install/subrouter" ]
  check "missing CLI: installs through install.sh" $?
  teardown
}

test_delegates_to_update_auto
test_falls_back_without_auto
test_falls_back_when_update_is_unknown
test_falls_back_when_binary_missing

[ "$failures" -eq 0 ] || { printf '%d failure(s)\n' "$failures"; exit 1; }
printf 'all cli autoupdate checks passed\n'
