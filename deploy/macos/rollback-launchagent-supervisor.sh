#!/usr/bin/env bash
# Restore an exact pre-supervisor LaunchAgent plist. The command refuses an
# unexpected loaded program or PID change and waits for label, PID, and listener
# absence before bootstrapping the preserved plist.
set -euo pipefail

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
# shellcheck disable=SC1091
. "$SCRIPT_DIR/launchagent-transition-lib.sh"
# shellcheck disable=SC1091
. "$SCRIPT_DIR/mutation-lease-lib.sh"

export SUBROUTER_TRANSITION_NAME="rollback-launchagent-supervisor"
LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter}"
PLIST="${SUBROUTER_PLIST:-$HOME/Library/LaunchAgents/${LABEL}.plist}"
MUTATION_LOCK_FILE="${SUBROUTER_MUTATION_LOCK_FILE:-${PLIST}.supervisor-mutation.lock}"
DOMAIN="${SUBROUTER_LAUNCHD_DOMAIN:-gui/$(id -u)}"
BACKUP="${SUBROUTER_ROLLBACK_PLIST:-}"
EXPECTED_PROGRAM="${SUBROUTER_EXPECTED_PROGRAM:-}"
EXPECTED_RUNNING_PROGRAM="${SUBROUTER_EXPECTED_RUNNING_PROGRAM:-}"
BACKUP_SHA256="${SUBROUTER_ROLLBACK_PLIST_SHA256:-}"
PUBLIC_ADDR_OVERRIDE="${SUBROUTER_PUBLIC_ADDR:-}"
EXPECTED_FILES=()
EXPECTED_FILE_SHAS=()
ROLLBACK_DESTINATIONS=()
ROLLBACK_ARTIFACTS=()
ROLLBACK_ARTIFACT_SHAS=()
ROLLBACK_ARTIFACT_MODES=()
INSTALLED_DEPENDENCY_PATHS=()
INSTALLED_DEPENDENCY_SNAPSHOTS=()
INSTALLED_DEPENDENCY_SHAS=()
INSTALLED_DEPENDENCY_MODES=()
SERVING_STORE_BINDING=""
SERVING_STORE_BINDING_BACKUP=""
SERVING_STORE_BINDING_BACKUP_SHA256=""
SERVING_STORE_BINDING_BACKUP_MODE=""
SERVING_STORE_BINDING_WAS_ABSENT=0
EXPECTED_SERVING_STORE_BINDING_SHA256=""
TRANSACTION_DIR="${SUBROUTER_TRANSACTION_DIR:-}"

set_rollback_phase() {
  [ -n "$TRANSACTION_DIR" ] || return 0
  python3 - "$TRANSACTION_DIR" "$1" <<'PY'
import os
import sys

directory, phase = sys.argv[1:]
if not os.path.isdir(directory):
    raise SystemExit("rollback transaction directory disappeared")
next_path = os.path.join(directory, "phase.rollback-next")
descriptor = os.open(next_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
try:
    os.write(descriptor, (phase + "\n").encode())
    os.fsync(descriptor)
finally:
    os.close(descriptor)
os.replace(next_path, os.path.join(directory, "phase"))
directory_descriptor = os.open(directory, os.O_RDONLY)
try:
    os.fsync(directory_descriptor)
finally:
    os.close(directory_descriptor)
PY
  if [ "${SUBROUTER_ROLLBACK_FAULT_INJECT_HARD_PHASE:-}" = "$1" ]; then
    kill -KILL $$
  fi
  if [ "${SUBROUTER_ROLLBACK_FAULT_INJECT_HARD_OWNER_PHASE:-}" = "$1" ]; then
    kill -KILL "$PPID"
    kill -KILL $$
  fi
}

usage() {
  local status="${1:-2}" destination=/dev/stderr
  [ "$status" -ne 0 ] || destination=/dev/stdout
  cat >"$destination" <<EOF
usage: $0 --backup PLIST --backup-sha256 SHA \\
  --rollback-artifact DEST ARTIFACT SHA MODE [--rollback-artifact ...] \\
  [--serving-store-binding PATH \\
    (--serving-store-binding-backup ARTIFACT SHA MODE | \\
     --serving-store-binding-absent) \\
    --expected-serving-store-binding-sha256 SHA] \\
  [--public-addr HOST:PORT] [--expected-program PATH] \\
  [--expected-file-sha256 PATH SHA] \\
  [--expected-running-program PATH]

Restore the identity-checked legacy LaunchAgent only after proving the loaded
service, captured process, and public listener are absent. A new migration also
restores the exact prior default-shell serving-store binding after the legacy
service is healthy. Use the complete command printed by a successful supervised
activation.
EOF
  exit "$status"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --backup) [ "$#" -ge 2 ] || usage; BACKUP="$2"; shift 2 ;;
    --backup-sha256) [ "$#" -ge 2 ] || usage; BACKUP_SHA256="$2"; shift 2 ;;
    --expected-file-sha256)
      [ "$#" -ge 3 ] || usage
      EXPECTED_FILES+=("$2"); EXPECTED_FILE_SHAS+=("$3"); shift 3 ;;
    --rollback-artifact)
      [ "$#" -ge 5 ] || usage
      ROLLBACK_DESTINATIONS+=("$2"); ROLLBACK_ARTIFACTS+=("$3")
      ROLLBACK_ARTIFACT_SHAS+=("$4"); ROLLBACK_ARTIFACT_MODES+=("$5"); shift 5 ;;
    --serving-store-binding)
      [ "$#" -ge 2 ] || usage
      SERVING_STORE_BINDING="$2"; shift 2 ;;
    --serving-store-binding-backup)
      [ "$#" -ge 4 ] || usage
      SERVING_STORE_BINDING_BACKUP="$2"
      SERVING_STORE_BINDING_BACKUP_SHA256="$3"
      SERVING_STORE_BINDING_BACKUP_MODE="$4"
      shift 4 ;;
    --serving-store-binding-absent)
      SERVING_STORE_BINDING_WAS_ABSENT=1; shift ;;
    --expected-serving-store-binding-sha256)
      [ "$#" -ge 2 ] || usage
      EXPECTED_SERVING_STORE_BINDING_SHA256="$2"; shift 2 ;;
    --expected-program) [ "$#" -ge 2 ] || usage; EXPECTED_PROGRAM="$2"; shift 2 ;;
    --public-addr) [ "$#" -ge 2 ] || usage; PUBLIC_ADDR_OVERRIDE="$2"; shift 2 ;;
    --expected-running-program)
      [ "$#" -ge 2 ] || usage; EXPECTED_RUNNING_PROGRAM="$2"; shift 2 ;;
    -h|--help) usage 0 ;;
    *) usage ;;
  esac
done

[ -n "$BACKUP" ] || usage
[ -n "$BACKUP_SHA256" ] || usage
[ "${#ROLLBACK_ARTIFACTS[@]}" -gt 0 ] || usage
if [ -n "$SERVING_STORE_BINDING" ]; then
  [ "$SERVING_STORE_BINDING" = "$HOME/.subrouter/codex/.local-serving-store.json" ] \
    || launchagent_die "serving-store binding must be the current user's default-shell binding"
  case "$EXPECTED_SERVING_STORE_BINDING_SHA256" in
    ????????????????????????????????????????????????????????????????) ;;
    *) launchagent_die "expected serving-store binding sha256 is invalid" ;;
  esac
  case "$EXPECTED_SERVING_STORE_BINDING_SHA256" in
    *[!0-9a-f]*) launchagent_die "expected serving-store binding sha256 is invalid" ;;
  esac
  if [ -n "$SERVING_STORE_BINDING_BACKUP" ]; then
    [ "$SERVING_STORE_BINDING_WAS_ABSENT" -eq 0 ] || usage
    python3 - "$SERVING_STORE_BINDING_BACKUP_MODE" <<'PY'
import sys

value = sys.argv[1]
if not value or any(character not in "01234567" for character in value):
    raise SystemExit("serving-store binding backup mode must be octal")
mode = int(value, 8)
if mode & 0o077 or mode & ~0o777:
    raise SystemExit("serving-store binding backup mode must be private and non-special")
PY
  else
    [ "$SERVING_STORE_BINDING_WAS_ABSENT" -eq 1 ] || usage
  fi
else
  [ -z "$SERVING_STORE_BINDING_BACKUP" ] \
    && [ "$SERVING_STORE_BINDING_WAS_ABSENT" -eq 0 ] \
    && [ -z "$EXPECTED_SERVING_STORE_BINDING_SHA256" ] || usage
fi

validate_public_addr() {
  python3 - "$1" <<'PY'
import sys

value = sys.argv[1]
host, separator, port_text = value.rpartition(":")
if not separator or not host or not port_text.isdigit():
    raise SystemExit("public address must be HOST:PORT or [IPv6]:PORT")
port = int(port_text)
if not 1 <= port <= 65535:
    raise SystemExit("public address port must be between 1 and 65535")
if (host.startswith("[") or host.endswith("]")) and not (
    host.startswith("[") and host.endswith("]")
):
    raise SystemExit("IPv6 public address must use [IPv6]:PORT form")
if ":" in host and not (host.startswith("[") and host.endswith("]")):
    raise SystemExit("IPv6 public address must use [IPv6]:PORT form")
PY
}

[ -z "$PUBLIC_ADDR_OVERRIDE" ] || validate_public_addr "$PUBLIC_ADDR_OVERRIDE"

serving_store_binding_transaction() {
  [ -n "$SERVING_STORE_BINDING" ] || return 0
  python3 - \
    "$1" \
    "$SERVING_STORE_BINDING" \
    "$EXPECTED_SERVING_STORE_BINDING_SHA256" \
    "$SERVING_STORE_BINDING_BACKUP" \
    "$SERVING_STORE_BINDING_BACKUP_SHA256" \
    "$SERVING_STORE_BINDING_BACKUP_MODE" \
    "$SERVING_STORE_BINDING_WAS_ABSENT" <<'PY'
import fcntl
import hashlib
import os
import stat
import sys
import tempfile

operation, path, candidate_sha, backup_path, prior_sha, prior_mode_raw, prior_absent_raw = sys.argv[1:]
if operation not in {"check", "restore"}:
    raise SystemExit("invalid serving-store binding transaction operation")
prior_absent = prior_absent_raw == "1"
parent = os.path.dirname(path)
os.makedirs(parent, mode=0o700, exist_ok=True)
parent_info = os.lstat(parent)
if (
    not stat.S_ISDIR(parent_info.st_mode)
    or stat.S_ISLNK(parent_info.st_mode)
    or parent_info.st_uid != os.getuid()
    or parent_info.st_mode & 0o022
):
    raise SystemExit("serving-store binding directory has an unsafe identity")

lock_path = os.path.join(parent, ".local-serving-store.lock")
lock_flags = os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0)
lock_descriptor = os.open(lock_path, lock_flags, 0o600)
try:
    lock_opened = os.fstat(lock_descriptor)
    lock_named = os.lstat(lock_path)
    if (
        not stat.S_ISREG(lock_opened.st_mode)
        or stat.S_ISLNK(lock_named.st_mode)
        or (lock_opened.st_dev, lock_opened.st_ino) != (lock_named.st_dev, lock_named.st_ino)
        or lock_opened.st_uid != os.getuid()
        or lock_opened.st_mode & 0o077
    ):
        raise SystemExit("serving-store binding lock has an unsafe identity")
    fcntl.flock(lock_descriptor, fcntl.LOCK_EX)

    current_kind = "missing"
    if os.path.lexists(path):
        flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
        descriptor = os.open(path, flags)
        try:
            opened = os.fstat(descriptor)
            named = os.lstat(path)
            if (
                not stat.S_ISREG(opened.st_mode)
                or stat.S_ISLNK(named.st_mode)
                or (opened.st_dev, opened.st_ino) != (named.st_dev, named.st_ino)
                or opened.st_uid != os.getuid()
                or opened.st_mode & 0o077
                or opened.st_size > 4096
            ):
                raise SystemExit("serving-store binding has an unsafe identity")
            digest = hashlib.sha256()
            for chunk in iter(lambda: os.read(descriptor, 4096), b""):
                digest.update(chunk)
        finally:
            os.close(descriptor)
        actual_sha = digest.hexdigest()
        actual_mode = stat.S_IMODE(opened.st_mode)
        if actual_sha == candidate_sha and actual_mode == 0o600:
            current_kind = "candidate"
        elif (
            prior_sha
            and actual_sha == prior_sha
            and actual_mode == int(prior_mode_raw, 8)
        ):
            current_kind = "prior"
        else:
            raise SystemExit("serving-store binding changed outside the deployment transaction")
    elif not prior_absent:
        raise SystemExit("serving-store binding disappeared outside the transaction")

    if operation == "check" or current_kind == "prior" or (
        current_kind == "missing" and prior_absent
    ):
        raise SystemExit(0)

    if prior_absent:
        os.unlink(path)
    else:
        backup_flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
        backup_descriptor = os.open(backup_path, backup_flags)
        try:
            backup_opened = os.fstat(backup_descriptor)
            backup_named = os.lstat(backup_path)
            if (
                not stat.S_ISREG(backup_opened.st_mode)
                or stat.S_ISLNK(backup_named.st_mode)
                or (backup_opened.st_dev, backup_opened.st_ino)
                != (backup_named.st_dev, backup_named.st_ino)
            ):
                raise SystemExit("serving-store binding backup has an unsafe identity")
            backup = bytearray()
            backup_digest = hashlib.sha256()
            for chunk in iter(lambda: os.read(backup_descriptor, 4096), b""):
                backup.extend(chunk)
                backup_digest.update(chunk)
        finally:
            os.close(backup_descriptor)
        if backup_digest.hexdigest() != prior_sha:
            raise SystemExit("serving-store binding backup identity changed")
        temporary_descriptor, temporary_path = tempfile.mkstemp(
            prefix=".local-serving-store.rollback-", dir=parent
        )
        try:
            os.fchmod(temporary_descriptor, int(prior_mode_raw, 8))
            view = memoryview(backup)
            while view:
                written = os.write(temporary_descriptor, view)
                view = view[written:]
            os.fsync(temporary_descriptor)
            os.close(temporary_descriptor)
            temporary_descriptor = -1
            os.replace(temporary_path, path)
        finally:
            if temporary_descriptor >= 0:
                os.close(temporary_descriptor)
            if os.path.lexists(temporary_path):
                os.unlink(temporary_path)
    if prior_absent:
        if os.path.lexists(path):
            raise SystemExit("serving-store binding remained after transactional removal")
    else:
        restored_descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        try:
            restored_opened = os.fstat(restored_descriptor)
            restored_digest = hashlib.sha256()
            for chunk in iter(lambda: os.read(restored_descriptor, 4096), b""):
                restored_digest.update(chunk)
        finally:
            os.close(restored_descriptor)
        if (
            restored_digest.hexdigest() != prior_sha
            or stat.S_IMODE(restored_opened.st_mode) != int(prior_mode_raw, 8)
        ):
            raise SystemExit("restored serving-store binding identity check failed")
    parent_descriptor = os.open(parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(parent_descriptor)
    finally:
        os.close(parent_descriptor)
finally:
    os.close(lock_descriptor)
PY
}

# Adopt the migration's kernel lease (or acquire it for a standalone rollback)
# before hashing or parsing any rollback input. If the migration is killed
# while this child validates a large bundle, the helper must already know this
# exact child generation so no updater can interpose before live restoration.
owns_mutation_lease=0
if ! subrouter_mutation_lease_is_held_by_parent "$MUTATION_LOCK_FILE"; then
  acquire_subrouter_mutation_lease "$MUTATION_LOCK_FILE" \
    || { status=$?; echo "rollback-launchagent-supervisor: another deployment or worker update holds $MUTATION_LOCK_FILE" >&2; exit "$status"; }
  owns_mutation_lease=1
fi
restore_next=""
cleanup() {
  local snapshot
  [ -z "$restore_next" ] || rm -f "$restore_next"
  if [ "${INSTALLED_DEPENDENCY_SNAPSHOTS[0]+present}" = present ]; then
    for snapshot in "${INSTALLED_DEPENDENCY_SNAPSHOTS[@]}"; do
      rm -f "$snapshot"
    done
  fi
  [ "$owns_mutation_lease" -eq 0 ] || release_subrouter_mutation_lease
}
trap cleanup EXIT

[ -f "$BACKUP" ] || launchagent_die "rollback plist $BACKUP not found"
[ ! -L "$BACKUP" ] || launchagent_die "rollback plist must not be a symlink"
verify_file_sha256 "$BACKUP" "$BACKUP_SHA256" \
  || launchagent_die "rollback plist content identity check failed"
[ "$(plist_value "$BACKUP" Label)" = "$LABEL" ] \
  || launchagent_die "rollback plist does not declare label $LABEL"
rollback_program="$(plist_program "$BACKUP")"
[ -x "$rollback_program" ] || launchagent_die "rollback program $rollback_program is not executable"
program_identity_present=0
for index in "${!ROLLBACK_ARTIFACTS[@]}"; do
  verify_file_sha256 "${ROLLBACK_ARTIFACTS[$index]}" "${ROLLBACK_ARTIFACT_SHAS[$index]}" \
    || launchagent_die "rollback bundle artifact identity check failed"
  [ "${ROLLBACK_DESTINATIONS[$index]}" = "$rollback_program" ] && program_identity_present=1
done
if [ -n "$SERVING_STORE_BINDING_BACKUP" ]; then
  verify_file_sha256 "$SERVING_STORE_BINDING_BACKUP" "$SERVING_STORE_BINDING_BACKUP_SHA256" \
    || launchagent_die "serving-store binding backup identity check failed"
fi
serving_store_binding_transaction check \
  || launchagent_die "serving-store binding identity check failed; rollback withheld"
if [ "${#EXPECTED_FILES[@]}" -gt 0 ]; then
  for index in "${!EXPECTED_FILES[@]}"; do
    path="${EXPECTED_FILES[$index]}"
    expected_sha="${EXPECTED_FILE_SHAS[$index]}"
    verify_file_sha256 "$path" "$expected_sha" \
      || launchagent_die "rollback executable content identity check failed"
    [ "$path" = "$rollback_program" ] && program_identity_present=1
  done
fi
[ "$program_identity_present" -eq 1 ] \
  || launchagent_die "rollback program has no expected content identity"
while IFS= read -r required_path; do
  [ -n "$required_path" ] || continue
  required_identity_present=0
  if [ "${#EXPECTED_FILES[@]}" -gt 0 ]; then
    for path in "${EXPECTED_FILES[@]}"; do
      [ "$path" = "$required_path" ] && required_identity_present=1
    done
  fi
  for path in "${ROLLBACK_DESTINATIONS[@]}"; do
    [ "$path" = "$required_path" ] && required_identity_present=1
  done
  [ "$required_identity_present" -eq 1 ] \
    || launchagent_die "rollback dependency $required_path has no expected content identity"
done < <(plist_executable_dependencies "$BACKUP")

service="$DOMAIN/$LABEL"
captured_pid=""
installed_plist_sha=""
installed_plist_mode=""
installed_job_was_loaded=0
if [ -e "$PLIST" ]; then
  [ ! -L "$PLIST" ] || launchagent_die "installed plist must not be a symlink"
  if [ -z "$EXPECTED_PROGRAM" ]; then
    EXPECTED_PROGRAM="$(plist_program "$PLIST")"
  fi
  require_plist_identity "$PLIST" "$LABEL" "$EXPECTED_PROGRAM"
  [ -n "$EXPECTED_RUNNING_PROGRAM" ] || EXPECTED_RUNNING_PROGRAM="$EXPECTED_PROGRAM"
  installed_public_addr="$(plist_public_addr "$PLIST" 2>/dev/null || true)"
  if [ -n "$installed_public_addr" ]; then
    validate_public_addr "$installed_public_addr"
  elif [ -z "$PUBLIC_ADDR_OVERRIDE" ]; then
    launchagent_die "installed plist has no --addr; pass --public-addr"
  fi
  if [ -n "$installed_public_addr" ] && [ -n "$PUBLIC_ADDR_OVERRIDE" ] \
    && [ "$PUBLIC_ADDR_OVERRIDE" != "$installed_public_addr" ]; then
    launchagent_die "public address override does not match installed plist"
  fi
  public_addr="${installed_public_addr:-$PUBLIC_ADDR_OVERRIDE}"
  restore_next="${PLIST}.rollback-current.$$"
  rm -f "$restore_next"
  installed_plist_sha="$(sha256_file "$PLIST")"
  installed_plist_mode="$(stat -f '%Lp' "$PLIST")"
  copy_file_nofollow "$PLIST" "$restore_next" "$installed_plist_mode" \
    || launchagent_die "snapshot installed plist before rollback"
  verify_file_sha256 "$PLIST" "$installed_plist_sha" \
    || launchagent_die "installed plist changed while snapshotting rollback state"
  verify_file_sha256 "$restore_next" "$installed_plist_sha" \
    || launchagent_die "installed plist rollback snapshot identity check failed"
  dependency_index=0
  while IFS= read -r dependency_path; do
    [ -n "$dependency_path" ] || continue
    dependency_snapshot="${PLIST}.rollback-current.$$.dependency.${dependency_index}"
    dependency_sha="$(sha256_file "$dependency_path")"
    dependency_mode="$(stat -f '%Lp' "$dependency_path")"
    INSTALLED_DEPENDENCY_PATHS+=("$dependency_path")
    INSTALLED_DEPENDENCY_SNAPSHOTS+=("$dependency_snapshot")
    INSTALLED_DEPENDENCY_SHAS+=("$dependency_sha")
    INSTALLED_DEPENDENCY_MODES+=("$dependency_mode")
    rm -f "$dependency_snapshot"
    verify_file_sha256 "$dependency_path" "$dependency_sha" \
      || launchagent_die "installed executable dependency has an unsafe identity"
    copy_file_nofollow "$dependency_path" "$dependency_snapshot" "$dependency_mode" \
      || launchagent_die "snapshot installed executable dependency before rollback"
    verify_file_sha256 "$dependency_path" "$dependency_sha" \
      || launchagent_die "installed executable dependency changed while snapshotting rollback state"
    verify_file_sha256 "$dependency_snapshot" "$dependency_sha" \
      || launchagent_die "installed executable dependency rollback snapshot identity check failed"
    dependency_index=$((dependency_index + 1))
  done < <(plist_executable_dependencies "$PLIST")
  if launchagent_job_loaded "$service"; then
    installed_job_was_loaded=1
    captured_pid="$(capture_loaded_identity "$service" "$EXPECTED_RUNNING_PROGRAM")"
    launchctl bootout "$service"
  fi
else
  launchagent_job_loaded "$service" \
    && launchagent_die "installed plist is absent but $service is still loaded"
  if [ -n "$PUBLIC_ADDR_OVERRIDE" ]; then
    public_addr="$PUBLIC_ADDR_OVERRIDE"
  else
    public_addr="$(plist_public_addr "$BACKUP")"
  fi
fi
validate_public_addr "$public_addr"
wait_for_full_absence "$service" "$captured_pid" "$public_addr"

trap 'exit 130' INT
trap 'exit 143' TERM
for index in "${!ROLLBACK_ARTIFACTS[@]}"; do
  atomic_restore_nofollow \
    "${ROLLBACK_ARTIFACTS[$index]}" "${ROLLBACK_DESTINATIONS[$index]}" \
    "${ROLLBACK_ARTIFACT_SHAS[$index]}" "${ROLLBACK_ARTIFACT_MODES[$index]}" \
    || launchagent_die "rollback executable restore failed"
done
verify_file_sha256 "$BACKUP" "$BACKUP_SHA256" \
  || launchagent_die "rollback plist changed immediately before restore"
atomic_restore_nofollow "$BACKUP" "$PLIST" "$BACKUP_SHA256" 0644 \
  || launchagent_die "rollback plist restore failed"
plutil -lint "$PLIST" >/dev/null

if ! serving_store_binding_transaction check; then
  if [ -n "$restore_next" ]; then
    for index in "${!INSTALLED_DEPENDENCY_PATHS[@]}"; do
      atomic_restore_nofollow \
        "${INSTALLED_DEPENDENCY_SNAPSHOTS[$index]}" \
        "${INSTALLED_DEPENDENCY_PATHS[$index]}" \
        "${INSTALLED_DEPENDENCY_SHAS[$index]}" \
        "${INSTALLED_DEPENDENCY_MODES[$index]}" \
        || launchagent_die "serving-store binding changed and captured supervisor executable restoration failed"
    done
    atomic_restore_nofollow \
      "$restore_next" "$PLIST" "$installed_plist_sha" "$installed_plist_mode" \
      || launchagent_die "serving-store binding changed and captured supervisor plist restoration failed"
    plutil -lint "$PLIST" >/dev/null \
      || launchagent_die "serving-store binding changed and captured supervisor plist is invalid"
    if [ "$installed_job_was_loaded" -eq 1 ]; then
      bootstrap_with_retry "$DOMAIN" "$PLIST" "$service" "$public_addr" \
        || launchagent_die "serving-store binding changed and captured supervisor failed to restart"
      capture_loaded_identity "$service" "$EXPECTED_RUNNING_PROGRAM" >/dev/null \
        || launchagent_die "serving-store binding changed and captured supervisor identity was not restored"
      launchagent_die "serving-store binding changed immediately before legacy bootstrap; rollback withheld and captured supervisor restored"
    fi
    launchagent_job_loaded "$service" \
      && launchagent_die "serving-store binding changed and an originally unloaded supervisor became loaded"
    launchagent_die "serving-store binding changed immediately before legacy bootstrap; rollback withheld, installed supervisor plist restored, and job left unloaded"
  fi
  launchagent_die "serving-store binding changed immediately before legacy bootstrap; rollback withheld"
fi

set_rollback_phase rollback_legacy_bootstrap_requested
bootstrap_with_retry "$DOMAIN" "$PLIST" "$service" "$public_addr" \
  || launchagent_die "rollback LaunchAgent failed to bootstrap"
restored_pid="$(capture_loaded_identity "$service" "$rollback_program")" \
  || launchagent_die "rollback LaunchAgent is not loaded"

health_host="$public_addr"
case "$health_host" in
  0.0.0.0:*) health_host="127.0.0.1:${public_addr#0.0.0.0:}" ;;
  \[::\]:*) health_host="127.0.0.1:${public_addr#\[::\]:}" ;;
esac
health_url="${SUBROUTER_HEALTH_URL:-http://${health_host}/_subrouter/health}"
ready_url="${SUBROUTER_READY_URL:-http://${health_host}/_subrouter/ready}"
wait_for_http_acceptance "$health_url" "$ready_url" \
  || launchagent_die "rollback LaunchAgent failed health/readiness acceptance"
set_rollback_phase rollback_legacy_accepted

# Keep new launches fail-closed on the candidate binding until the legacy
# process has proved its own executable identity and health. Candidate traffic
# is unavailable after full absence; a failed legacy bootstrap never publishes
# an unproved rollback binding.
serving_store_binding_transaction restore \
  || launchagent_die "serving-store binding changed before post-acceptance restore; healthy legacy retained and binding rollback withheld"
set_rollback_phase rollback_binding_restored

echo "restored $BACKUP as $PLIST"
echo "rollback LaunchAgent healthy and ready (pid $restored_pid)"
