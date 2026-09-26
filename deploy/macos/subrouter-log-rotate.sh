#!/usr/bin/env bash
# subrouter-log-rotate.sh bounds the launchd logs this repo installs.
#
# install-daemon writes StandardOutPath/StandardErrorPath into the service
# plist, the guard plist logs to /var/log/subrouter-guard.log, and nothing
# prunes either. A busy proxy writes one INFO line per upstream request, so on
# a real pool these files reach hundreds of megabytes in days and then grow
# until the disk fills.
#
# Why this cannot be a newsyslog rule
# -----------------------------------
# launchd opens StandardOutPath/StandardErrorPath once and holds that
# descriptor for the lifetime of the job. newsyslog rotates by renaming, which
# moves the name but not the inode: the service keeps writing into the rotated
# file while the freshly created one stays empty forever. Rotation appears to
# work and silently does nothing.
#
# So this copies the contents out and then truncates IN PLACE. The inode -- and
# therefore launchd's descriptor -- survives, and the service keeps logging to
# the same path with no restart and no signal.
#
# The trade, stated rather than hidden: lines written between the copy and the
# truncate are lost. That is why rotation triggers on size rather than on a
# schedule alone, so it happens rarely and never mid-burst by design.
#
# newsyslog remains correct for logs no long-running process holds open; this
# script deliberately does not try to replace it for those.
set -uo pipefail

# Rotation triggers on size; retention is bounded by BOTH a generation count and
# an age. Either alone is wrong: a count-only bound keeps a quiet machine's
# archives forever, and an age-only bound lets a busy pool keep hundreds of
# files inside the window. Whichever limit is reached first wins.
MAX_BYTES="${SUBROUTER_LOG_MAX_BYTES:-67108864}"          # rotate above 64 MiB
KEEP="${SUBROUTER_LOG_KEEP:-5}"                            # compressed generations
MAX_AGE_DAYS="${SUBROUTER_LOG_MAX_AGE_DAYS:-14}"           # and nothing older than this; 0 disables
STATE="${SUBROUTER_VERIFY_STATE:-/var/lib/subrouter-verify}"
MAINTENANCE="${SUBROUTER_MAINTENANCE_FILE:-${STATE}/maintenance}"
# Labels whose plists are inspected for log paths. Space separated so an
# operator can add a site-specific job without editing this script.
# Its own label is included so this job bounds its own log too; otherwise the
# one file guaranteed to exist wherever rotation runs would be the one nothing
# prunes.
LABELS="${SUBROUTER_LOG_ROTATE_LABELS:-ai.manaflow.subrouter-team ai.manaflow.subrouter-guard ai.manaflow.subrouter ai.manaflow.subrouter-log-rotate}"
# Directories searched for those plists, in order. Unquoted expansion below
# globs these, which is deliberate: `install-daemon` writes its plist to the
# LaunchAgents directory of the user who ran it, and when this job runs as a
# system LaunchDaemon $HOME is root's, so ${HOME}/Library/LaunchAgents would
# never find it. Every user's LaunchAgents directory has to be in scope.
PLIST_DIRS="${SUBROUTER_LOG_ROTATE_PLIST_DIRS:-/Library/LaunchDaemons /Library/LaunchAgents /Users/*/Library/LaunchAgents ${HOME}/Library/LaunchAgents}"
# Extra paths an operator names explicitly, space separated.
EXTRA_LOGS="${SUBROUTER_LOG_ROTATE_EXTRA:-}"
# Injectable so the test can assert on discovery without a real plist tool,
# mirroring how subrouter-guard.sh injects launchctl.
PLUTIL="${SUBROUTER_PLUTIL:-plutil}"

log() { printf '%s subrouter-log-rotate: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }

# The guard and verify jobs honor a maintenance sentinel only while it is fresh,
# so a forgotten one cannot disable protection for good. Rotation follows the
# same rule for the same reason, and with more at stake: a stale sentinel that
# suppressed rotation forever would end in a full disk.
MAINTENANCE_MAX_AGE_MINS="${SUBROUTER_MAINTENANCE_MAX_AGE_MINS:-90}"
if [ -e "$MAINTENANCE" ]; then
  if [ -n "$(find "$MAINTENANCE" -mmin -"$MAINTENANCE_MAX_AGE_MINS" 2>/dev/null)" ]; then
    log "maintenance sentinel at ${MAINTENANCE} is fresh; not rotating"
    exit 0
  fi
  log "maintenance sentinel at ${MAINTENANCE} is older than ${MAINTENANCE_MAX_AGE_MINS}m; rotating anyway"
fi

# discover_logs prints every StandardOutPath/StandardErrorPath configured by the
# known labels. Reading the plist rather than hardcoding paths keeps this
# correct when an operator installs to a custom log directory, and means a
# rotated path is always one the service actually writes.
discover_logs() {
  local label dir plist value key
  for label in $LABELS; do
    for dir in $PLIST_DIRS; do
      plist="${dir}/${label}.plist"
      [ -f "$plist" ] || continue
      for key in StandardOutPath StandardErrorPath; do
        value="$("$PLUTIL" -extract "$key" raw -o - "$plist" 2>/dev/null)" || continue
        [ -n "$value" ] && printf '%s\n' "$value"
      done
    done
  done
  local extra
  for extra in $EXTRA_LOGS; do printf '%s\n' "$extra"; done
}

# rotate_one archives and truncates a single log when it exceeds the threshold.
rotate_one() {
  local log_path="$1" size stamp archive inode_before inode_after
  [ -n "$log_path" ] || return 0
  # /dev/null is a legitimate value for a plist log key; never touch a device.
  [ -f "$log_path" ] || return 0
  # A symlinked log path would let a rename target be chosen elsewhere, and we
  # would truncate whatever it points at. Refuse rather than follow.
  [ -L "$log_path" ] && { log "refusing a symlinked log: ${log_path}"; return 0; }
  [ -w "$log_path" ] || { log "not writable, skipping: ${log_path}"; return 0; }

  size=$(stat -f %z "$log_path" 2>/dev/null || echo 0)
  if [ "$size" -le "$MAX_BYTES" ]; then
    # Retention still applies to a log that is not rotating right now. A quiet
    # host may never cross the size threshold again, and without this its old
    # archives would outlive both bounds -- which would silently defeat the age
    # bound precisely where it matters most.
    prune "$log_path"
    return 0
  fi

  stamp=$(date -u +%Y%m%d-%H%M%SZ)
  archive="${log_path}.${stamp}.gz"

  # Read the contents through a descriptor opened once, not by re-opening the
  # path. gzip on a large log takes real time, and a log directory writable by
  # someone else (a user LaunchAgent's own ~/Library/Logs, say) gives them a
  # window to swap the path for a symlink mid-archive. Holding the descriptor
  # means the bytes archived are the bytes we checked.
  inode_before=$(stat -f %i "$log_path" 2>/dev/null || echo 0)
  exec 9<"$log_path" || { log "FAILED to open ${log_path}; left intact"; return 1; }
  # umask so an archive of a restrictive log cannot become world-readable
  # through whatever umask launchd happened to hand us.
  if ! (umask 077; gzip -c <&9 >"$archive" 2>/dev/null); then
    exec 9<&-
    rm -f "$archive"
    log "FAILED to archive ${log_path}; left intact"
    return 1
  fi
  exec 9<&-

  # Re-verify immediately before truncating. If the path was swapped while we
  # were archiving, truncating it now would empty someone else's file.
  inode_after=$(stat -f %i "$log_path" 2>/dev/null || echo 0)
  if [ -L "$log_path" ] || [ "$inode_before" != "$inode_after" ]; then
    log "REFUSING to truncate ${log_path}: it changed while being archived; archive retained at ${archive}"
    return 1
  fi

  # Truncate, never recreate: the inode must outlive this so launchd's open
  # descriptor keeps pointing at the file the service still writes to.
  if ! : >"$log_path"; then
    log "FAILED to truncate ${log_path}; archive retained at ${archive}"
    return 1
  fi
  log "rotated ${log_path} (${size} bytes) -> ${archive}"

  prune "$log_path"
}

# prune enforces both retention bounds for one log: keep at most $KEEP archives,
# and drop anything older than $MAX_AGE_DAYS regardless of how few remain.
prune() {
  local log_path="$1" count=0 file
  while IFS= read -r file; do
    count=$((count + 1))
    if [ "$count" -gt "$KEEP" ]; then
      rm -f "$file" && log "pruned ${file} (over ${KEEP} generations)"
    fi
  done < <(ls -t "${log_path}".*.gz 2>/dev/null)

  [ "$MAX_AGE_DAYS" -gt 0 ] 2>/dev/null || return 0
  # -mtime +N on macOS find is "older than N 24-hour periods", which is the
  # bound we want and avoids parsing the timestamp out of the filename.
  while IFS= read -r file; do
    [ -n "$file" ] || continue
    rm -f "$file" && log "pruned ${file} (older than ${MAX_AGE_DAYS}d)"
  done < <(find "$(dirname "$log_path")" -maxdepth 1 -name "$(basename "$log_path").*.gz" -type f -mtime +"$MAX_AGE_DAYS" 2>/dev/null)
}

status=0
seen=""
while IFS= read -r candidate; do
  [ -n "$candidate" ] || continue
  # The same path is reachable through several labels and through both the out
  # and err keys; rotate it once.
  case " ${seen} " in *" ${candidate} "*) continue ;; esac
  seen="${seen} ${candidate}"
  rotate_one "$candidate" || status=1
done < <(discover_logs)

exit "$status"
