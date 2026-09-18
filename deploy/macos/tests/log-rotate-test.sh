#!/usr/bin/env bash
# Exercises subrouter-log-rotate.sh against fake plists and throwaway logs.
# Nothing here touches a real service, a real log, or launchd.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROTATE="$HERE/../subrouter-log-rotate.sh"
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
  mkdir -p "$ROOT/plists" "$ROOT/logs" "$ROOT/state"
  cat >"$ROOT/plists/ai.manaflow.subrouter-team.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>ai.manaflow.subrouter-team</string>
	<key>StandardOutPath</key><string>$ROOT/logs/out.log</string>
	<key>StandardErrorPath</key><string>$ROOT/logs/err.log</string>
</dict>
</plist>
PLIST
  export SUBROUTER_LOG_ROTATE_LABELS="ai.manaflow.subrouter-team"
  export SUBROUTER_LOG_ROTATE_PLIST_DIRS="$ROOT/plists"
  export SUBROUTER_VERIFY_STATE="$ROOT/state"
  export SUBROUTER_LOG_ROTATE_EXTRA=""
  unset SUBROUTER_LOG_MAX_BYTES SUBROUTER_LOG_KEEP
}

teardown() { rm -rf "$ROOT"; }

big() { # big <path> <bytes>
  : >"$1"
  while [ "$(stat -f %z "$1")" -lt "$2" ]; do
    head -c 4096 /dev/zero | tr '\0' 'x' >>"$1"
  done
}

# A log over the threshold is archived, emptied, and -- the property the whole
# design depends on -- keeps its inode so launchd's open descriptor survives.
test_rotates_and_preserves_inode() {
  setup
  big "$ROOT/logs/err.log" 2048
  : >"$ROOT/logs/out.log"
  local before after
  before=$(stat -f %i "$ROOT/logs/err.log")
  SUBROUTER_LOG_MAX_BYTES=1024 "$ROTATE" >/dev/null 2>&1
  after=$(stat -f %i "$ROOT/logs/err.log")

  [ "$before" = "$after" ]; check "rotation preserves the inode" $?
  [ "$(stat -f %z "$ROOT/logs/err.log")" -eq 0 ]; check "rotated log is truncated to empty" $?
  [ "$(ls "$ROOT/logs"/err.log.*.gz 2>/dev/null | wc -l)" -eq 1 ]; check "one compressed archive is written" $?
  # An append after rotation must land in the same file, which is what proves a
  # long-running writer is unaffected.
  printf 'after-rotation\n' >>"$ROOT/logs/err.log"
  grep -q 'after-rotation' "$ROOT/logs/err.log"; check "writes after rotation still land in the log" $?
  teardown
}

test_leaves_small_logs_alone() {
  setup
  printf 'small\n' >"$ROOT/logs/err.log"
  SUBROUTER_LOG_MAX_BYTES=1048576 "$ROTATE" >/dev/null 2>&1
  [ "$(ls "$ROOT/logs"/err.log.*.gz 2>/dev/null | wc -l)" -eq 0 ]; check "a log under the threshold is not rotated" $?
  grep -q 'small' "$ROOT/logs/err.log"; check "a log under the threshold keeps its contents" $?
  teardown
}

test_prunes_to_keep() {
  setup
  local i
  for i in 1 2 3 4 5 6 7; do
    : >"$ROOT/logs/err.log.2026010${i}-000000Z.gz"
    sleep 0.01
  done
  big "$ROOT/logs/err.log" 2048
  SUBROUTER_LOG_MAX_BYTES=1024 SUBROUTER_LOG_KEEP=3 "$ROTATE" >/dev/null 2>&1
  [ "$(ls "$ROOT/logs"/err.log.*.gz 2>/dev/null | wc -l)" -eq 3 ]; check "archives are pruned to SUBROUTER_LOG_KEEP" $?
  teardown
}

# Age retention must drop an old archive even when the generation count alone
# would have kept it.
test_prunes_by_age() {
  setup
  : >"$ROOT/logs/err.log.20260101-000000Z.gz"
  touch -t 202601010000 "$ROOT/logs/err.log.20260101-000000Z.gz"
  : >"$ROOT/logs/err.log.20260102-000000Z.gz"
  big "$ROOT/logs/err.log" 2048
  SUBROUTER_LOG_MAX_BYTES=1024 SUBROUTER_LOG_KEEP=99 SUBROUTER_LOG_MAX_AGE_DAYS=1 "$ROTATE" >/dev/null 2>&1
  [ ! -e "$ROOT/logs/err.log.20260101-000000Z.gz" ]; check "an archive past SUBROUTER_LOG_MAX_AGE_DAYS is pruned" $?
  [ -e "$ROOT/logs/err.log.20260102-000000Z.gz" ]; check "a recent archive survives age pruning" $?
  teardown
}

# Age pruning must be switchable off for anyone who wants count-only retention.
test_age_zero_disables() {
  setup
  : >"$ROOT/logs/err.log.20260101-000000Z.gz"
  touch -t 202601010000 "$ROOT/logs/err.log.20260101-000000Z.gz"
  big "$ROOT/logs/err.log" 2048
  SUBROUTER_LOG_MAX_BYTES=1024 SUBROUTER_LOG_KEEP=99 SUBROUTER_LOG_MAX_AGE_DAYS=0 "$ROTATE" >/dev/null 2>&1
  [ -e "$ROOT/logs/err.log.20260101-000000Z.gz" ]; check "SUBROUTER_LOG_MAX_AGE_DAYS=0 disables age pruning" $?
  teardown
}

# Retention must apply to a log that is NOT rotating. A quiet host may never
# cross the size threshold again, and its archives would otherwise outlive both
# bounds -- defeating the age bound exactly where it matters most.
test_prunes_when_below_threshold() {
  setup
  : >"$ROOT/logs/err.log.20260101-000000Z.gz"
  touch -t 202601010000 "$ROOT/logs/err.log.20260101-000000Z.gz"
  printf 'small\n' >"$ROOT/logs/err.log"
  SUBROUTER_LOG_MAX_BYTES=1048576 SUBROUTER_LOG_MAX_AGE_DAYS=1 "$ROTATE" >/dev/null 2>&1
  [ ! -e "$ROOT/logs/err.log.20260101-000000Z.gz" ]; check "retention applies even when no rotation is due" $?
  grep -q 'small' "$ROOT/logs/err.log"; check "the un-rotated log itself is untouched" $?
  teardown
}

# An archive of a restrictive log must not become readable by other local users
# through whatever umask launchd supplies.
test_archive_permissions() {
  setup
  big "$ROOT/logs/err.log" 2048
  ( umask 000; SUBROUTER_LOG_MAX_BYTES=1024 "$ROTATE" >/dev/null 2>&1 )
  local mode
  mode=$(stat -f %Lp "$ROOT/logs"/err.log.*.gz 2>/dev/null | head -1)
  [ "$mode" = "600" ]; check "archives are created 0600 regardless of umask (got ${mode:-none})" $?
  teardown
}

test_honors_maintenance() {
  setup
  : >"$ROOT/state/maintenance"
  big "$ROOT/logs/err.log" 2048
  SUBROUTER_LOG_MAX_BYTES=1024 "$ROTATE" >/dev/null 2>&1
  [ "$(stat -f %z "$ROOT/logs/err.log")" -gt 1024 ]; check "maintenance file suppresses rotation" $?
  teardown
}

# A sentinel nobody cleaned up must not suppress rotation forever; the disk
# would fill. Matches the freshness rule the guard and verify jobs already use.
test_stale_maintenance_does_not_suppress() {
  setup
  : >"$ROOT/state/maintenance"
  touch -t 202601010000 "$ROOT/state/maintenance"
  big "$ROOT/logs/err.log" 2048
  SUBROUTER_LOG_MAX_BYTES=1024 "$ROTATE" >/dev/null 2>&1
  [ "$(stat -f %z "$ROOT/logs/err.log")" -eq 0 ]; check "a stale maintenance sentinel does not suppress rotation" $?
  teardown
}

# Truncating through a symlink would empty whatever it points at, which may not
# be a log at all.
test_refuses_symlink() {
  setup
  big "$ROOT/logs/real-target" 2048
  ln -s "$ROOT/logs/real-target" "$ROOT/logs/err.log"
  SUBROUTER_LOG_MAX_BYTES=1024 "$ROTATE" >/dev/null 2>&1
  [ "$(stat -f %z "$ROOT/logs/real-target")" -gt 1024 ]; check "a symlinked log path is refused, not followed" $?
  teardown
}

# /dev/null is a legitimate StandardOutPath; rotating it would be a bug.
test_ignores_devnull() {
  setup
  cat >"$ROOT/plists/ai.manaflow.subrouter-team.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>ai.manaflow.subrouter-team</string>
	<key>StandardOutPath</key><string>/dev/null</string>
	<key>StandardErrorPath</key><string>/dev/null</string>
</dict>
</plist>
PLIST
  SUBROUTER_LOG_MAX_BYTES=1 "$ROTATE" >/dev/null 2>&1
  [ -c /dev/null ]; check "/dev/null is left as a character device" $?
  teardown
}

test_missing_plist_is_not_an_error() {
  setup
  rm -f "$ROOT/plists/ai.manaflow.subrouter-team.plist"
  SUBROUTER_LOG_MAX_BYTES=1024 "$ROTATE" >/dev/null 2>&1
  check "no configured plist exits cleanly" $?
  teardown
}

test_rotates_and_preserves_inode
test_leaves_small_logs_alone
test_prunes_to_keep
test_prunes_by_age
test_age_zero_disables
test_prunes_when_below_threshold
test_archive_permissions
test_honors_maintenance
test_stale_maintenance_does_not_suppress
test_refuses_symlink
test_ignores_devnull
test_missing_plist_is_not_an_error

if [ "$failures" -ne 0 ]; then
  printf '\n%d check(s) failed\n' "$failures"
  exit 1
fi
printf '\nall log-rotate checks passed\n'
