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
LABELS="${SUBROUTER_LOG_ROTATE_LABELS:-ai.manaflow.subrouter-team ai.manaflow.subrouter-guard ai.manaflow.subrouter ai.manaflow.subrouter-cli-autoupdate ai.manaflow.subrouter-log-rotate}"
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
# Also injectable so the test can drive the root code paths without root.
SUDO="${SUBROUTER_SUDO:-sudo}"
RUN_UID="${SUBROUTER_LOG_ROTATE_RUN_UID:-$(id -u)}"

SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)/$(basename "${BASH_SOURCE[0]}")"

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

# private_dir succeeds when a directory is owned by uid, has no ACL, and is
# writable by nobody else.
private_dir() { # private_dir <dir> <uid>
  local owner mode
  owner=$(stat -f %u "$1" 2>/dev/null) || return 1
  mode=$(stat -f %Lp "$1" 2>/dev/null) || return 1
  [ "$owner" = "$2" ] || return 1
  [ $((8#$mode & 8#022)) -eq 0 ] || return 1
  # macOS ACLs can grant another user add_file or delete_child on a 755
  # directory; ls marks those with a trailing "+".
  case "$(ls -lde "$1" 2>/dev/null | head -1 | cut -c11)" in "+") return 1 ;; esac
}

# discover_logs prints "<trust>TAB<path>" for every StandardOutPath and
# StandardErrorPath the known labels configure. Reading the plist rather than
# hardcoding paths keeps this correct when an operator installs to a custom
# log directory.
#
# Trust comes from whoever could have written the plist, never from the log
# it names: every user's LaunchAgents directory is in scope, so a plist may be
# written by any local user. A plist counts only when it is a regular file
# whose owner also owns its directory and nobody else can write there; its
# owner's uid is the trust. Operator-named extra logs are trusted as root.
discover_logs() {
  local label dir plist value key trust
  for label in $LABELS; do
    for dir in $PLIST_DIRS; do
      plist="${dir}/${label}.plist"
      [ -f "$plist" ] && [ ! -L "$plist" ] || continue
      trust=$(stat -f %u "$plist" 2>/dev/null) || continue
      private_dir "$dir" "$trust" || { log "ignoring ${plist}: its directory is not private to its owner"; continue; }
      for key in StandardOutPath StandardErrorPath; do
        value="$("$PLUTIL" -extract "$key" raw -o - "$plist" 2>/dev/null)" || continue
        [ -n "$value" ] && printf '%s\t%s\n' "$trust" "$value"
      done
    done
  done
  local extra
  for extra in $EXTRA_LOGS; do printf '0\t%s\n' "$extra"; done
}

# archive_and_truncate runs in perl because the shell cannot do the two things
# that make this safe in a directory someone else can write: open the log with
# O_NOFOLLOW and truncate that same descriptor. It refuses anything but a
# regular file with one link, creates the archive with O_EXCL|O_NOFOLLOW so a
# planted name or symlink cannot redirect it, and gzips from the open
# descriptor. Exit 10 means the log is at or under the threshold.
# shellcheck disable=SC2016 # $vars below are perl's, not the shell's.
ARCHIVE_AND_TRUNCATE='
use strict;
use Fcntl qw(O_RDWR O_WRONLY O_CREAT O_EXCL O_NOFOLLOW O_NONBLOCK);
my ($path, $archive, $max) = @ARGV;
sysopen(my $log, $path, O_RDWR | O_NOFOLLOW | O_NONBLOCK) or die "open: $!\n";
my @st = stat($log) or die "stat: $!\n";
-f _ or die "not a regular file\n";
$st[3] == 1 or die "has $st[3] hard links\n";
exit 10 if $st[7] <= $max;
sysopen(my $out, $archive, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW, 0600) or die "create archive: $!\n";
my $pid = fork();
defined $pid or die "fork: $!\n";
if ($pid == 0) {
  open(STDIN, "<&", $log) or exit 111;
  open(STDOUT, ">&", $out) or exit 111;
  exec("gzip", "-c") or exit 111;
}
waitpid($pid, 0);
if ($? != 0) { close($out); unlink($archive); die "gzip failed\n"; }
close($out) or die "close archive: $!\n";
truncate($log, 0) or die "truncate: $!\n";
print "$st[7]\n";
'

# root_only_path succeeds when every directory above an absolute log path, as
# written and as resolved, is private to root, so no other user can swap a
# component while root works on it.
root_only_path() {
  local dir physical
  case "$1" in /*) ;; *) return 1 ;; esac
  dir="$(dirname "$1")"
  physical="$(cd -P "$dir" 2>/dev/null && pwd -P)" || return 1
  for dir in "$dir" "$physical"; do
    while :; do
      private_dir "$dir" 0 || return 1
      [ "$dir" = "/" ] && break
      dir="$(dirname "$dir")"
    done
  done
}

# rotate_one archives and truncates a single log when it exceeds the threshold.
rotate_one() { # rotate_one <trust-uid> <path>
  local trust="$1" log_path="$2" owner stamp archive out rc act
  [ -n "$log_path" ] || return 0
  # launchd resolves a relative log path against its own cwd, not ours.
  case "$log_path" in /*) ;; *) log "refusing a relative log path: ${log_path}"; return 0 ;; esac
  # /dev/null is a legitimate value for a plist log key; never touch a device.
  [ -f "$log_path" ] || return 0
  [ -L "$log_path" ] && { log "refusing a symlinked log: ${log_path}"; return 0; }
  owner=$(stat -f %u "$log_path" 2>/dev/null) || return 0

  # A plist written by a user may only name that user's own logs.
  if [ "$trust" != 0 ] && [ "$owner" != "$trust" ]; then
    log "refusing ${log_path}: owned by uid ${owner}, but named by a plist owned by uid ${trust}"
    return 0
  fi
  # --one is the already-dispatched child; it never dispatches again.
  if [ "$RUN_UID" -eq 0 ] && [ -z "$ONE_SHOT" ]; then
    if [ "$owner" != 0 ]; then
      # A user's log sits where that user can rename or relink the path. Act
      # with that user's privileges, so nothing done here can reach a file the
      # user could not already change.
      act="$owner"
      "$SUDO" -n -u "#${act}" env \
        SUBROUTER_LOG_MAX_BYTES="$MAX_BYTES" SUBROUTER_LOG_KEEP="$KEEP" \
        SUBROUTER_LOG_MAX_AGE_DAYS="$MAX_AGE_DAYS" \
        SUBROUTER_VERIFY_STATE="$STATE" SUBROUTER_MAINTENANCE_FILE="$MAINTENANCE" \
        /bin/bash "$SELF" --one "$act" "$log_path"
      return $?
    fi
    if ! root_only_path "$log_path"; then
      log "refusing root-owned ${log_path}: a directory above it is not private to root"
      return 0
    fi
  fi
  [ -w "$log_path" ] || { log "not writable, skipping: ${log_path}"; return 0; }

  stamp=$(date -u +%Y%m%d-%H%M%SZ)
  archive="${log_path}.${stamp}.gz"
  out=$(perl -e "$ARCHIVE_AND_TRUNCATE" "$log_path" "$archive" "$MAX_BYTES" 2>&1)
  rc=$?
  case "$rc" in
    0)
      log "rotated ${log_path} (${out} bytes) -> ${archive}"
      ;;
    10)
      # Retention still applies to a log that is not rotating right now. A
      # quiet host may never cross the size threshold again, and without this
      # its old archives would outlive both bounds.
      ;;
    *)
      log "FAILED to rotate ${log_path}: ${out}"
      return 1
      ;;
  esac
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

ONE_SHOT=""
if [ "${1:-}" = "--one" ]; then
  [ $# -eq 3 ] || { log "usage: --one <uid> <path>"; exit 2; }
  ONE_SHOT=1
  rotate_one "$2" "$3"
  exit $?
fi

status=0
seen=""
while IFS=$'\t' read -r trust candidate; do
  [ -n "$candidate" ] || continue
  # The same path is reachable through several labels and through both the out
  # and err keys; rotate it once.
  case " ${seen} " in *" ${trust}:${candidate} "*) continue ;; esac
  seen="${seen} ${trust}:${candidate}"
  rotate_one "$trust" "$candidate" || status=1
done < <(discover_logs)

exit "$status"
