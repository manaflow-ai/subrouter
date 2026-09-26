#!/usr/bin/env bash
# subrouter-deploy.sh installs a worker binary onto a supervised host without
# ever closing the public listener.
#
# Why this exists: on 2026-09-04 a hand-built worker was copied over
# /usr/local/bin/subrouter and the LaunchDaemon was booted out and bootstrapped
# to pick it up. That binary needed longer than the supervisor's 30s
# --ready-timeout to answer /_subrouter/ready, so `supervise` aborted before it
# bound 0.0.0.0:31415, launchd restarted it, and the whole team lost the proxy
# for seven minutes. A supervisor restart makes worker readiness fatal.
#
# The supervisor already has a failure-atomic upgrade path: replace the worker
# binary, then POST /_subrouter/upgrade on the control socket. The supervisor
# starts the new generation behind the still-bound listener, waits for its
# readiness, and keeps serving from the old generation if the new one never
# becomes ready. subrouter-autoupdate.sh has always used it. This script gives
# operators and agents the same path for a binary that did not come from a
# release, so the only way to lose the listener is to bypass this script.
set -euo pipefail

BIN="${SUBROUTER_BIN:-/usr/local/bin/subrouter}"
LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter-team}"
PLIST="${SUBROUTER_PLIST:-/Library/LaunchDaemons/${LABEL}.plist}"
STATE="${SUBROUTER_DEPLOY_STATE:-/var/lib/subrouter-verify}"
LAST_GOOD="${SUBROUTER_LAST_GOOD:-${STATE}/subrouter.last-good}"
VERSION_FILE="${SUBROUTER_VERSION_FILE:-/etc/subrouter-version}"
HEALTH_URL="${SUBROUTER_HEALTH_URL:-http://127.0.0.1:31415/_subrouter/health}"
UPGRADE_INHIBIT_FILE="${SUBROUTER_UPGRADE_INHIBIT_FILE:-${PLIST}.supervisor-transaction/upgrade-inhibited}"
HEALTH_TIMEOUT_SECS="${SUBROUTER_DEPLOY_HEALTH_TIMEOUT_SECS:-45}"
LOCK_DIR="${SUBROUTER_DEPLOY_LOCK_DIR:-${STATE}/deploy.lock}"
SUPERVISOR_BIN="${SUBROUTER_SUPERVISOR_BIN:-/usr/local/libexec/subrouter-supervisor}"
MAINTENANCE="${SUBROUTER_MAINTENANCE_FILE:-${STATE}/maintenance}"
LAUNCHCTL="${SUBROUTER_LAUNCHCTL:-launchctl}"
RESTART_WAIT_SECS="${SUBROUTER_DEPLOY_RESTART_WAIT_SECS:-12}"
REPO_URL="${SUBROUTER_DEPLOY_REPO_URL:-https://github.com/manaflow-ai/subrouter.git}"
REPO_CACHE="${SUBROUTER_DEPLOY_REPO_CACHE:-${STATE}/subrouter.git}"
REVISIONS_DIR="${SUBROUTER_DEPLOY_REVISIONS_DIR:-${STATE}/revisions}"
WORKER_CONFIG="${SUBROUTER_WORKER_CONFIG:-/var/lib/subrouter/worker-config.json}"
LOCK_WAIT_SECS="${SUBROUTER_DEPLOY_LOCK_WAIT_SECS:-90}"
REPO="${SUBROUTER_REPO:-manaflow-ai/subrouter}"
RELEASE_DOWNLOAD_URL="${SUBROUTER_RELEASE_DOWNLOAD_URL:-https://github.com/${REPO}/releases/download}"
BACKUP_DIR="${SUBROUTER_BACKUP_DIR:-${STATE}/backups}"
KEEP_BACKUPS="${SUBROUTER_KEEP_BACKUPS:-3}"
RELEASE_TMP=""

log() { printf 'subrouter-deploy: %s\n' "$*" >&2; }
die() { log "$*"; exit 1; }

usage() {
  cat <<'EOF'
Usage:
  subrouter-deploy.sh install <candidate-binary> --revision <commit> [--label <version-text>]
  subrouter-deploy.sh install <candidate-binary> --allow-unrelated <reason> [--label <version-text>]
  subrouter-deploy.sh record-revision <commit>
  subrouter-deploy.sh reconfigure <worker-config.json>
  subrouter-deploy.sh install-release <vX.Y.Z>
  subrouter-deploy.sh install-supervisor <candidate-binary>
  subrouter-deploy.sh restart-daemon
  subrouter-deploy.sh rollback [--to <vX.Y.Z>]
  subrouter-deploy.sh pin [<vX.Y.Z>]
  subrouter-deploy.sh unpin
  subrouter-deploy.sh list
  subrouter-deploy.sh status

install   Hot-swap the worker behind the live listener and roll back by itself
          if the candidate never becomes ready or public health drops.
          --revision names the pushed commit the candidate was built from. The
          install is refused unless that commit contains the commit of the
          live worker, so a build from an old branch cannot silently drop
          fixes that are serving now. --allow-unrelated skips the check and
          logs the reason; use it only for an emergency rollback to a build
          that has no recorded revision.
record-revision
          Record the commit of the live worker when it was installed without
          --revision. The commit must exist in the repository.
reconfigure
          Change worker flags or environment behind the live listener. The
          supervisor re-reads its --worker-config file for every generation,
          so this installs the file and hot-upgrades; a bad file is refused
          or reverted and the old worker keeps serving. Edit the plist only
          for supervisor flags, never for worker flags or worker env.
install-release
          Download a release worker (the darwin asset for this CPU), verify it
          against the release SHA256SUMS the way subrouter-autoupdate.sh does,
          then install it like `install --label <vX.Y.Z> --revision <tag commit>`.
install-supervisor
          Replace the supervisor, which owns the listener and therefore needs a
          restart, then verify health and put the old binary back if it does
          not return.
restart-daemon
          Stop and start the LaunchDaemon as one detached operation that
          finishes even if the shell or ssh session that started it dies.
rollback  Put the recorded last-good worker back the same way, or with --to
          a kept backup of that version.
pin       Stop subrouter-autoupdate.sh from replacing the worker. With a
          version, install that release first (the pin is written before the
          install, so autoupdate cannot slip in between).
unpin     Remove the pin (or the guard's rollback sentinel) so autoupdate
          resumes on its next run.
list      Print the installed version, whether autoupdate is pinned, and the
          kept backups. Every install and rollback keeps the replaced worker
          in the backup directory; the newest three are kept.
status    Print the live binary and its commit, the recorded last-good, and health.

Never run `launchctl bootout` on the subrouter LaunchDaemon by hand. A restart
turns a slow or broken worker into a total outage, because the supervisor binds
the public port only after the first worker is ready, and a bootout whose shell
dies before the bootstrap leaves the service out of the launchd domain with the
port closed. `restart-daemon` exists so that sequence cannot be interrupted.
EOF
}

control_socket() {
  if [ -n "${SUBROUTER_CONTROL_SOCKET:-}" ]; then
    printf '%s\n' "$SUBROUTER_CONTROL_SOCKET"
    return
  fi
  [ -f "$PLIST" ] || die "cannot find $PLIST; set SUBROUTER_CONTROL_SOCKET"
  PLIST="$PLIST" python3 - <<'PY'
import os, plistlib
with open(os.environ["PLIST"], "rb") as stream:
    arguments = plistlib.load(stream).get("ProgramArguments") or []
for index, argument in enumerate(arguments):
    if argument == "--control-socket" and index + 1 < len(arguments):
        print(arguments[index + 1])
        break
    if argument.startswith("--control-socket="):
        print(argument.split("=", 1)[1])
        break
PY
}

sha_of() { [ -f "$1" ] && shasum -a 256 "$1" | awk '{print $1}' || echo "missing"; }

health_ok() { curl -fsS --max-time 5 "$HEALTH_URL" >/dev/null 2>&1; }

wait_health() {
  local deadline=$((SECONDS + HEALTH_TIMEOUT_SECS))
  while [ "$SECONDS" -lt "$deadline" ]; do
    health_ok && return 0
    sleep 1
  done
  return 1
}

request_upgrade() {
  local socket="$1"
  curl -fsS --max-time 120 --unix-socket "$socket" -X POST "http://localhost/_subrouter/upgrade"
}

restore_binary() {
  local source="$1"
  install -m 0755 "$source" "${BIN}.rollback"
  mv -f "${BIN}.rollback" "$BIN"
}

# version_label turns the version marker into a file-name-safe label:
# "v0.1.130" stays, "rollback:abc123 (was v0.1.131)" becomes "rollback-abc123".
version_label() {
  local label
  label="$(sed -n '1p' "$VERSION_FILE" 2>/dev/null || true)"
  label="${label%% *}"
  label="${label//:/-}"
  printf '%s' "${label:-unknown}" | tr -c 'A-Za-z0-9._+-' '_'
}

normalize_tag() { # normalize_tag <version> -> vX.Y.Z, or fails
  local tag="v${1#v}"
  [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || return 1
  printf '%s\n' "$tag"
}

now_ns() { python3 -c 'import time; print(time.time_ns())'; }

# keep_backup copies a worker into $BACKUP_DIR as <epoch-ns>_<version-label>.
# The nanosecond prefix orders backups even when several land in one second.
keep_backup() { # keep_backup <binary> -> prints the backup path
  mkdir -p "$BACKUP_DIR"
  local path
  path="${BACKUP_DIR}/$(now_ns)_$(version_label)"
  cp -p "$1" "$path"
  printf '%s\n' "$path"
}

backup_files() { # newest first
  [ -d "$BACKUP_DIR" ] || return 0
  find "$BACKUP_DIR" -maxdepth 1 -type f -name '[0-9]*_*' 2>/dev/null | LC_ALL=C sort -r
}

find_backup() { # find_backup <vX.Y.Z> -> newest kept backup of that version
  local tag="$1" path
  while IFS= read -r path; do
    [ "${path##*_}" = "$tag" ] && { printf '%s\n' "$path"; return 0; }
  done < <(backup_files)
  return 1
}

# prune_backups keeps the newest $KEEP_BACKUPS in $BACKUP_DIR, and as many of
# the loose ${BIN}.backup-* / ${BIN}.rejected-* copies that older deploys,
# subrouter-autoupdate.sh and subrouter-guard.sh left next to the binary.
prune_backups() {
  local path kind
  backup_files | tail -n +"$((KEEP_BACKUPS + 1))" | while IFS= read -r path; do
    rm -f "$path"
  done || true
  for kind in backup rejected; do
    # shellcheck disable=SC2012 # the names are ours: <bin>.<kind>-<timestamp>
    ls -1t "${BIN}.${kind}-"* 2>/dev/null | tail -n +"$((KEEP_BACKUPS + 1))" | while IFS= read -r path; do
      rm -f "$path"
    done || true
  done
}

# $LOCK_DIR is the one lock for every writer of the worker binary on this
# host: this script, subrouter-autoupdate.sh (while it swaps), and
# subrouter-guard.sh (while it promotes last-good or rolls back). Each holder
# names itself in $LOCK_DIR/owner.
lock_owner() {
  sed -n '1p' "$LOCK_DIR/owner" 2>/dev/null | grep . || echo "another deploy"
}

take_lock() {
  mkdir -p "$STATE"
  local deadline=$((SECONDS + LOCK_WAIT_SECS))
  while ! mkdir "$LOCK_DIR" 2>/dev/null; do
    # A crashed deploy must not block the next one forever, but a live deploy
    # must not be joined by a second writer either.
    if [ -d "$LOCK_DIR" ] && [ -z "$(find "$LOCK_DIR" -maxdepth 0 -mmin -30 2>/dev/null)" ]; then
      log "clearing stale lock $LOCK_DIR ($(lock_owner))"
      rm -f "$LOCK_DIR/owner"
      rmdir "$LOCK_DIR" 2>/dev/null || true
      mkdir "$LOCK_DIR" 2>/dev/null || die "cannot take $LOCK_DIR"
      break
    fi
    # The guard and autoupdate hold the lock for seconds to a couple of
    # minutes; wait for them rather than failing the operator's command.
    if [ "$SECONDS" -ge "$deadline" ]; then
      die "$(lock_owner) holds $LOCK_DIR (started less than 30 minutes ago)"
    fi
    sleep 1
  done
  printf 'subrouter-deploy.sh pid %s\n' "$$" >"$LOCK_DIR/owner"
  trap release_lock EXIT
}

# An operator or subrouter-guard.sh can pin worker autoupdate by writing the
# inhibit sentinel. A deploy must borrow that file, not consume it: clearing
# someone else's pin on exit would let the updater reinstall the release the
# pin exists to keep out.
HAD_INHIBIT=0
WROTE_INHIBIT=0
PREEXISTING_INHIBIT=""

release_lock() {
  [ -z "$RELEASE_TMP" ] || rm -rf "$RELEASE_TMP"
  # Only this run's own sentinel may be removed. restart-daemon and
  # install-supervisor never write one, and deleting the operator's pin on
  # their way out re-armed the updater on a host that is pinned precisely
  # because the next release cannot become ready there.
  if [ "$HAD_INHIBIT" -eq 1 ]; then
    printf '%s\n' "$PREEXISTING_INHIBIT" >"$UPGRADE_INHIBIT_FILE" 2>/dev/null || true
    chmod 0600 "$UPGRADE_INHIBIT_FILE" 2>/dev/null || true
  elif [ "$WROTE_INHIBIT" -eq 1 ]; then
    rm -f "$UPGRADE_INHIBIT_FILE" 2>/dev/null || true
  fi
  rm -f "$LOCK_DIR/owner" 2>/dev/null || true
  rmdir "$LOCK_DIR" 2>/dev/null || true
}

inhibit_autoupdate() {
  mkdir -p "$(dirname "$UPGRADE_INHIBIT_FILE")"
  if [ -e "$UPGRADE_INHIBIT_FILE" ]; then
    HAD_INHIBIT=1
    PREEXISTING_INHIBIT="$(cat "$UPGRADE_INHIBIT_FILE" 2>/dev/null || true)"
    log "an autoupdate pin is in place; it will be restored when this deploy finishes"
  fi
  printf 'subrouter-deploy.sh running (pid %s)\n' "$$" >"$UPGRADE_INHIBIT_FILE"
  chmod 0600 "$UPGRADE_INHIBIT_FILE"
  WROTE_INHIBIT=1
}

swap_and_verify() {
  # swap_and_verify <new-binary> <fallback-binary> <description>
  local candidate="$1" fallback="$2" description="$3"
  local socket
  socket="$(control_socket)"
  [ -S "$socket" ] || die "control socket $socket is not a socket; is ${LABEL} running?"

  install -m 0755 "$candidate" "${BIN}.new"
  mv -f "${BIN}.new" "$BIN"

  if ! request_upgrade "$socket" >/dev/null; then
    log "${description} never became ready; the old generation is still serving"
    restore_binary "$fallback"
    request_upgrade "$socket" >/dev/null 2>&1 || true
    return 1
  fi

  if ! wait_health; then
    log "${description} took the new generation but public health failed; restoring"
    restore_binary "$fallback"
    request_upgrade "$socket" >/dev/null 2>&1 || true
    wait_health || log "health is still down after restoring; check subrouter-guard.log"
    return 1
  fi
  return 0
}

# --- lineage ---------------------------------------------------------------
# On 2026-09-22 a worker built from a feature branch cut before the usage-sweep
# fixes replaced a worker that had them. Health stayed 200, so nothing caught
# it; the sweep timed out for a third of the pool until the build was merged
# with main and redeployed. Each installed binary is now mapped to the commit it
# was built from, and a candidate must contain the live worker's commit.

valid_revision() { printf '%s' "$1" | grep -Eq '^[0-9a-f]{40}$'; }

revision_of_binary() { # revision_of_binary <binary-sha256>
  local file="${REVISIONS_DIR}/$1"
  [ -f "$file" ] && head -n 1 "$file"
}

record_binary_revision() { # record_binary_revision <binary-sha256> <commit>
  mkdir -p "$REVISIONS_DIR"
  printf '%s\n' "$2" >"${REVISIONS_DIR}/$1.new"
  mv -f "${REVISIONS_DIR}/$1.new" "${REVISIONS_DIR}/$1"
}

refresh_repo_cache() {
  if [ ! -d "$REPO_CACHE" ]; then
    git init --quiet --bare "$REPO_CACHE" || return 1
  fi
  # Every branch, so a revision that only lives on a deploy branch resolves.
  GIT_TERMINAL_PROMPT=0 git --git-dir="$REPO_CACHE" fetch --quiet --prune --no-tags \
    "$REPO_URL" '+refs/heads/*:refs/heads/*'
}

commit_known() { git --git-dir="$REPO_CACHE" cat-file -e "$1^{commit}" 2>/dev/null; }

# check_lineage <candidate-commit>: fails unless the candidate contains the
# commit recorded for the live worker. A live worker without a record is
# allowed once, with a warning, so the first guarded install can bootstrap.
check_lineage() {
  local candidate_rev="$1" live_sha live_rev
  refresh_repo_cache || die "cannot fetch $REPO_URL to verify --revision; retry, or pass --allow-unrelated <reason> in an emergency"
  commit_known "$candidate_rev" || die "revision $candidate_rev is not in $REPO_URL; push the branch you built from first"
  live_sha="$(sha_of "$BIN")"
  live_rev="$(revision_of_binary "$live_sha" || true)"
  if [ -z "$live_rev" ]; then
    log "warning: the live worker ${live_sha:0:12} has no recorded commit, so lineage is not checked this time (subrouter-deploy.sh record-revision <commit> records it)"
    return 0
  fi
  commit_known "$live_rev" || die "the live worker's commit $live_rev is no longer in $REPO_URL; restore that branch or pass --allow-unrelated <reason>"
  if ! git --git-dir="$REPO_CACHE" merge-base --is-ancestor "$live_rev" "$candidate_rev"; then
    die "candidate commit ${candidate_rev:0:12} does not contain the live worker's commit ${live_rev:0:12}. Merge ${live_rev:0:12} into your branch, rebuild, push, and retry. Deploying it anyway would drop the live fixes."
  fi
  log "lineage ok: ${candidate_rev:0:12} contains live ${live_rev:0:12}"
}

cmd_record_revision() {
  local revision="${1:-}"
  valid_revision "$revision" || die "record-revision needs a full 40-character commit"
  refresh_repo_cache || die "cannot fetch $REPO_URL"
  commit_known "$revision" || die "revision $revision is not in $REPO_URL"
  local live_sha
  live_sha="$(sha_of "$BIN")"
  record_binary_revision "$live_sha" "$revision"
  log "recorded live worker ${live_sha:0:12} as ${revision:0:12}"
}

cmd_install() {
  local candidate="${1:-}"
  shift || true
  local version_label="" revision="" allow_unrelated=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --label) version_label="${2:-}"; shift 2 ;;
      --revision) revision="${2:-}"; shift 2 ;;
      --allow-unrelated) allow_unrelated="${2:-}"; shift 2 ;;
      *) die "unknown option $1" ;;
    esac
  done
  if [ -n "$revision" ]; then
    valid_revision "$revision" || die "--revision needs a full 40-character commit, got '$revision'"
  elif [ -z "$allow_unrelated" ]; then
    die "pass --revision <full commit the candidate was built from>; the commit must be pushed and must contain the live worker's commit"
  fi

  [ -n "$candidate" ] || { usage; exit 2; }
  [ -f "$candidate" ] || die "$candidate does not exist"
  [ -x "$candidate" ] || die "$candidate is not executable"
  "$candidate" --help >/dev/null 2>&1 || die "$candidate does not answer --help; wrong arch or a corrupt download"

  local candidate_sha current_sha
  candidate_sha="$(sha_of "$candidate")"
  current_sha="$(sha_of "$BIN")"
  if [ "$candidate_sha" = "$current_sha" ]; then
    log "candidate is already installed ($candidate_sha)"
    if [ -n "$version_label" ] && [ "$(sed -n '1p' "$VERSION_FILE" 2>/dev/null || true)" != "$version_label" ]; then
      printf '%s\n' "$version_label" >"${VERSION_FILE}.new"
      mv -f "${VERSION_FILE}.new" "$VERSION_FILE"
      log "version marker now reads $version_label"
    fi
    exit 0
  fi

  health_ok || die "public health is down right now; fix the outage before installing (subrouter-deploy.sh rollback, or check /var/log/subrouter-guard.log)"

  if [ -n "$revision" ]; then
    check_lineage "$revision"
  else
    log "lineage check skipped with --allow-unrelated: $allow_unrelated"
  fi

  take_lock
  inhibit_autoupdate

  # The binary that is serving traffic right now is by definition good.
  mkdir -p "$(dirname "$LAST_GOOD")"
  cp -p "$BIN" "$LAST_GOOD"
  # The rollback source is this private copy, never $LAST_GOOD. That file is
  # shared with subrouter-guard.sh, which promotes whatever binary is answering
  # health; between the swap below and the generation switch the old worker is
  # still serving, so a guard tick there would record the untested candidate as
  # last-good and this rollback would "restore" the candidate over itself.
  local backup
  backup="$(keep_backup "$BIN")"
  log "current worker ${current_sha:0:12} saved to $LAST_GOOD and $backup"

  if ! swap_and_verify "$candidate" "$backup" "candidate ${candidate_sha:0:12}"; then
    die "install failed and the previous worker was restored; the listener never dropped"
  fi

  printf '%s\n' "${version_label:-local:${candidate_sha:0:12}}" >"${VERSION_FILE}.new"
  mv -f "${VERSION_FILE}.new" "$VERSION_FILE"
  [ -z "$revision" ] || record_binary_revision "$candidate_sha" "$revision"
  prune_backups
  log "installed ${candidate_sha:0:12}; old connections are draining"
  if [ -z "$version_label" ]; then
    log "note: /etc/subrouter-version now reads local:${candidate_sha:0:12}, so subrouter-autoupdate.sh will replace this build with the next release"
  fi
}

# validate_worker_config mirrors resolveWorkerLaunch in cmd/subrouter/supervisor.go
# so a bad file is refused here before the supervisor ever sees it.
validate_worker_config() {
  python3 - "$1" <<'PY'
import json, sys
path = sys.argv[1]
try:
    with open(path) as stream:
        doc = json.load(stream)
except Exception as error:
    sys.exit(f"{path}: not valid JSON: {error}")
if not isinstance(doc, dict):
    sys.exit(f"{path}: top level must be an object")
unknown = set(doc) - {"args", "env"}
if unknown:
    sys.exit(f"{path}: unknown keys {sorted(unknown)}")
args = doc.get("args")
if not isinstance(args, list) or not all(isinstance(a, str) for a in args):
    sys.exit(f'{path}: "args" must be a list of strings')
for arg in args:
    for owned in ("--addr", "--local-data-socket"):
        if arg == owned or arg.startswith(owned + "="):
            sys.exit(f"{path}: {owned} is owned by the supervisor")
env = doc.get("env", {})
if env is None:
    env = {}
if not isinstance(env, dict) or not all(isinstance(k, str) and isinstance(v, str) for k, v in env.items()):
    sys.exit(f'{path}: "env" must map strings to strings')
for key, value in env.items():
    if not key or "=" in key or "\0" in key or "\0" in value:
        sys.exit(f"{path}: invalid env entry {key!r}")
    if key in ("SUBROUTER_LISTEN_FD", "SUBROUTER_PRIVATE_DATA_ROUTER"):
        sys.exit(f"{path}: env {key} is owned by the supervisor")
PY
}

# The supervisor ignores the file unless the plist passes --worker-config.
worker_config_wired() {
  [ -f "$PLIST" ] || return 0
  PLIST="$PLIST" WORKER_CONFIG="$WORKER_CONFIG" python3 - <<'PY'
import os, plistlib, sys
with open(os.environ["PLIST"], "rb") as stream:
    arguments = plistlib.load(stream).get("ProgramArguments") or []
want = os.environ["WORKER_CONFIG"]
for index, argument in enumerate(arguments):
    if argument == "--":
        break
    if argument == "--worker-config" and index + 1 < len(arguments) and arguments[index + 1] == want:
        sys.exit(0)
    if argument == "--worker-config=" + want:
        sys.exit(0)
sys.exit(1)
PY
}

# The deploy tests run these scripts on Linux CI, where stat is GNU: -f there
# means "file system" and takes the format as a file operand.
file_owner() { if stat --version >/dev/null 2>&1; then stat -c '%u:%g' "$1"; else stat -f '%u:%g' "$1"; fi; }
file_mode() { if stat --version >/dev/null 2>&1; then stat -c '%a' "$1"; else stat -f '%Lp' "$1"; fi; }

install_worker_config() { # install_worker_config <source> <owner:group> <mode>
  local tmp="${WORKER_CONFIG}.new"
  install -m "$3" "$1" "$tmp"
  chown "$2" "$tmp" 2>/dev/null || true
  mv -f "$tmp" "$WORKER_CONFIG"
}

cmd_reconfigure() {
  local candidate="${1:-}"
  [ -n "$candidate" ] || { usage; exit 2; }
  [ -f "$candidate" ] || die "$candidate does not exist"
  validate_worker_config "$candidate" || die "refusing an invalid worker config"
  worker_config_wired || die "$PLIST does not pass --worker-config $WORKER_CONFIG; see DEPLOY.md for the one-time adoption"
  if [ -f "$WORKER_CONFIG" ] && cmp -s "$candidate" "$WORKER_CONFIG"; then
    log "worker config is already installed"
    exit 0
  fi
  health_ok || die "public health is down right now; fix the outage before reconfiguring"
  local socket
  socket="$(control_socket)"
  [ -S "$socket" ] || die "control socket $socket is not a socket; is ${LABEL} running?"

  take_lock
  inhibit_autoupdate

  # The live file carries secrets-by-reference and must stay readable by the
  # service user only, so a new file inherits the live owner and mode.
  local owner mode backup=""
  mkdir -p "$(dirname "$WORKER_CONFIG")"
  if [ -f "$WORKER_CONFIG" ]; then
    owner="$(file_owner "$WORKER_CONFIG")"
    mode="$(file_mode "$WORKER_CONFIG")"
    backup="${WORKER_CONFIG}.backup-$(date +%Y%m%d-%H%M%S)"
    cp -p "$WORKER_CONFIG" "$backup"
    log "current worker config saved to $backup"
  else
    owner="$(file_owner "$(dirname "$WORKER_CONFIG")")"
    mode=0600
  fi
  install_worker_config "$candidate" "$owner" "$mode"

  restore_worker_config() {
    if [ -n "$backup" ]; then
      install_worker_config "$backup" "$owner" "$mode"
    else
      rm -f "$WORKER_CONFIG"
    fi
    request_upgrade "$socket" >/dev/null 2>&1 || true
  }

  if ! request_upgrade "$socket" >/dev/null; then
    log "the new worker config never produced a ready worker; the old generation is still serving"
    restore_worker_config
    die "reconfigure failed and the previous worker config was restored; the listener never dropped"
  fi
  if ! wait_health; then
    log "the new worker config switched generations but public health failed; restoring"
    restore_worker_config
    wait_health || log "health is still down after restoring; check subrouter-guard.log"
    die "reconfigure failed and the previous worker config was restored"
  fi
  log "worker config installed; old connections are draining"
}

cmd_rollback() {
  local to=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --to) to="${2:-}"; shift 2 ;;
      *) die "unknown option $1" ;;
    esac
  done
  local source description
  if [ -n "$to" ]; then
    to="$(normalize_tag "$to")" || die "invalid version $to; expected vX.Y.Z"
    source="$(find_backup "$to")" || die "no kept backup of $to in $BACKUP_DIR; 'subrouter-deploy.sh list' shows what is kept, 'subrouter-deploy.sh install-release $to' downloads it"
    description="kept $to"
  else
    [ -f "$LAST_GOOD" ] || die "no recorded last-good worker at $LAST_GOOD"
    source="$LAST_GOOD"
    description="last-good"
  fi
  local good_sha current_sha
  good_sha="$(sha_of "$source")"
  current_sha="$(sha_of "$BIN")"
  [ "$good_sha" != "$current_sha" ] || { log "$description is already installed ($good_sha)"; exit 0; }
  take_lock
  inhibit_autoupdate
  # Copy the source first: pruning must not delete the file the swap reads,
  # and $LAST_GOOD is shared with the guard.
  RELEASE_TMP="$(mktemp -d)"
  local staged="${RELEASE_TMP}/rollback-source"
  cp -p "$source" "$staged"
  local rollback_from
  rollback_from="$(keep_backup "$BIN")"
  if ! swap_and_verify "$staged" "$rollback_from" "$description ${good_sha:0:12}"; then
    die "rollback could not take effect through the control socket; the service may be restart-looping, see subrouter-guard.sh"
  fi
  if [ -n "$to" ]; then
    printf '%s\n' "$to" >"${VERSION_FILE}.new"
  else
    printf '%s\n' "rollback:${good_sha:0:12}" >"${VERSION_FILE}.new"
  fi
  mv -f "${VERSION_FILE}.new" "$VERSION_FILE"
  prune_backups
  log "rolled back to $description ${good_sha:0:12}; the replaced worker is kept at $rollback_from"
  if [ "$HAD_INHIBIT" -eq 0 ]; then
    log "autoupdate is not pinned and installs the latest release on its next run; 'subrouter-deploy.sh pin' keeps this worker"
  fi
}

# fetch_release downloads and verifies a release worker the way
# subrouter-autoupdate.sh does (exactly one SHA256SUMS line, matching digest)
# and prints the verified file.
fetch_release() { # fetch_release <vX.Y.Z>
  local tag="$1" arch
  case "${SUBROUTER_RELEASE_ARCH:-$(uname -m)}" in
    x86_64|amd64) arch="amd64" ;;
    arm64|aarch64) arch="arm64" ;;
    *) die "unsupported arch ${SUBROUTER_RELEASE_ARCH:-$(uname -m)}" ;;
  esac
  local asset="subrouter_${tag#v}_darwin_${arch}"
  local base="${RELEASE_DOWNLOAD_URL}/${tag}"
  curl -fsSL -o "${RELEASE_TMP}/${asset}" "${base}/${asset}" || die "could not download ${base}/${asset}"
  curl -fsSL -o "${RELEASE_TMP}/SHA256SUMS" "${base}/SHA256SUMS" || die "could not download ${base}/SHA256SUMS"
  local sums
  sums="$(awk -v asset="$asset" '$2 == asset || $2 == "*" asset' "${RELEASE_TMP}/SHA256SUMS")"
  [ "$(printf '%s\n' "$sums" | awk 'NF {n++} END {print n + 0}')" = "1" ] \
    || die "expected exactly one checksum for $asset in ${base}/SHA256SUMS"
  (cd "$RELEASE_TMP" && printf '%s\n' "$sums" | shasum -a 256 -c - >/dev/null 2>&1) \
    || die "checksum mismatch for $asset; nothing was installed"
  chmod 0755 "${RELEASE_TMP}/${asset}"
  printf '%s\n' "${RELEASE_TMP}/${asset}"
}

cmd_install_release() {
  local tag
  [ -n "${1:-}" ] || { usage; exit 2; }
  tag="$(normalize_tag "$1")" || die "invalid version $1; expected vX.Y.Z"
  RELEASE_TMP="$(mktemp -d)"
  trap 'rm -rf "$RELEASE_TMP"' EXIT
  local candidate
  candidate="$(fetch_release "$tag")" || exit 1
  log "verified ${candidate##*/} against the ${tag} SHA256SUMS"
  local revision
  revision="$(release_revision "$tag")" \
    || die "cannot resolve the commit of release tag $tag in $REPO_URL, so its lineage cannot be checked"
  cmd_install "$candidate" --label "$tag" --revision "$revision"
}

# release_revision prints the commit a release tag points at, so a release
# install goes through the same lineage check as any other install.
release_revision() { # release_revision <vX.Y.Z>
  refresh_repo_cache || return 1
  GIT_TERMINAL_PROMPT=0 git --git-dir="$REPO_CACHE" fetch --quiet --no-tags \
    "$REPO_URL" "+refs/tags/$1:refs/tags/$1" || return 1
  git --git-dir="$REPO_CACHE" rev-parse --verify --quiet "refs/tags/$1^{commit}"
}

write_pin() { # write_pin <label>; callers hold the deploy lock
  mkdir -p "$(dirname "$UPGRADE_INHIBIT_FILE")"
  printf 'pinned at %s by subrouter-deploy.sh pin (%s) at %s; clear with subrouter-deploy.sh unpin\n' \
    "$1" "${SUDO_USER:-$(id -un 2>/dev/null || echo unknown)}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    >"${UPGRADE_INHIBIT_FILE}.new"
  chmod 0600 "${UPGRADE_INHIBIT_FILE}.new"
  mv -f "${UPGRADE_INHIBIT_FILE}.new" "$UPGRADE_INHIBIT_FILE"
}

# pin writes the same sentinel subrouter-autoupdate.sh already honours (and
# the guard writes after a rollback), so autoupdate prints the pin as its
# reason for deferring.
cmd_pin() {
  local tag="" installed
  if [ -n "${1:-}" ]; then
    tag="$(normalize_tag "$1")" || die "invalid version $1; expected vX.Y.Z"
  fi
  installed="$(sed -n '1p' "$VERSION_FILE" 2>/dev/null || true)"
  # Pin before installing: the install borrows the pin and puts it back, so
  # there is no moment in which autoupdate could replace the pinned release.
  ( take_lock; write_pin "${tag:-${installed:-unknown}}" ) || exit 1
  if [ -n "$tag" ] && [ "$tag" != "$installed" ]; then
    if ! ( cmd_install_release "$tag" ); then
      ( take_lock; write_pin "${installed:-unknown}" ) || true
      die "install of $tag failed; autoupdate stays pinned at ${installed:-unknown} (subrouter-deploy.sh unpin resumes it)"
    fi
  fi
  log "autoupdate pinned at ${tag:-${installed:-unknown}}; subrouter-deploy.sh unpin resumes it"
}

cmd_unpin() {
  take_lock
  if [ ! -e "$UPGRADE_INHIBIT_FILE" ]; then
    log "autoupdate is not pinned"
    return 0
  fi
  log "removing: $(sed -n '1p' "$UPGRADE_INHIBIT_FILE" 2>/dev/null || echo "$UPGRADE_INHIBIT_FILE")"
  rm -f "$UPGRADE_INHIBIT_FILE"
  log "autoupdate resumes on its next run"
}

backup_label() { # backup_label <path> -> "<version> <UTC time>"
  local name="${1##*/}" seconds when
  seconds="${name%%_*}"
  seconds="${seconds:0:10}"
  when="$(date -u -r "$seconds" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "@$seconds" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)"
  printf '%-24s %s' "${name#*_}" "$when"
}

cmd_list() {
  printf 'installed %s (%s)\n' "$(sed -n '1p' "$VERSION_FILE" 2>/dev/null || echo unknown)" "$(sha_of "$BIN" | cut -c1-12)"
  if [ -e "$UPGRADE_INHIBIT_FILE" ]; then
    printf 'pinned    yes: %s\n' "$(sed -n '1p' "$UPGRADE_INHIBIT_FILE" 2>/dev/null || echo "$UPGRADE_INHIBIT_FILE exists")"
  else
    printf 'pinned    no (subrouter-autoupdate.sh installs new releases)\n'
  fi
  printf 'last-good %s\n' "$(sha_of "$LAST_GOOD" | cut -c1-12)"
  printf 'backups   %s (newest first, %s kept)\n' "$BACKUP_DIR" "$KEEP_BACKUPS"
  local path found=0
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    found=1
    printf '  %s  %s\n' "$(backup_label "$path")" "$(sha_of "$path" | cut -c1-12)"
  done < <(backup_files)
  [ "$found" -eq 1 ] || printf '  (none)\n'
}

cmd_status() {
  local live_sha
  live_sha="$(sha_of "$BIN")"
  printf 'live      %s %s\n' "$BIN" "$live_sha"
  printf 'revision  %s\n' "$(revision_of_binary "$live_sha" || echo unrecorded)"
  printf 'last-good %s %s\n' "$LAST_GOOD" "$(sha_of "$LAST_GOOD")"
  printf 'version   %s\n' "$(cat "$VERSION_FILE" 2>/dev/null || echo unknown)"
  if health_ok; then printf 'health    ok\n'; else printf 'health    DOWN\n'; fi
}

# restart_daemon_body performs the whole stop/start sequence. It is written to
# a script and run detached, so an interrupted caller cannot leave the service
# booted out with the port closed: the sequence keeps running to the bootstrap.
restart_daemon_body() {
  cat <<'BODY'
set -u
LABEL="__LABEL__"
PLIST="__PLIST__"
SUPERVISOR_BIN="__SUPERVISOR_BIN__"
BIN="__BIN__"
LAUNCHCTL="__LAUNCHCTL__"
RESTART_WAIT_SECS="__RESTART_WAIT_SECS__"
MAINTENANCE="__MAINTENANCE__"
# This script, not the caller, clears the sentinel. A caller killed with
# SIGKILL runs no trap, and a sentinel left behind is what stopped the guard
# from healing an interrupted restart.
trap 'rm -f "$MAINTENANCE" 2>/dev/null || true' EXIT
"$LAUNCHCTL" bootout "system/${LABEL}" >/dev/null 2>&1 || true
deadline=$((SECONDS + RESTART_WAIT_SECS))
while [ "$SECONDS" -lt "$deadline" ]; do
  pids="$(pgrep -f "^${SUPERVISOR_BIN} supervise" 2>/dev/null; pgrep -f "^${BIN} serve " 2>/dev/null)"
  [ -z "$pids" ] && break
  sleep 1
done
pids="$(pgrep -f "^${SUPERVISOR_BIN} supervise" 2>/dev/null; pgrep -f "^${BIN} serve " 2>/dev/null)"
[ -n "$pids" ] && kill -KILL $pids 2>/dev/null
sleep 1
# launchd answers "Input/output error" while the old job is still draining
# under its ExitTimeOut, even after every process is gone. A single bootstrap
# therefore loses the race and leaves the service out of the domain with the
# port closed, which is how a restart turned into an outage. Retry until the
# job is really in the domain.
bootstrap_deadline=$((SECONDS + 120))
while [ "$SECONDS" -lt "$bootstrap_deadline" ]; do
  "$LAUNCHCTL" bootstrap system "$PLIST" >/dev/null 2>&1
  "$LAUNCHCTL" print "system/${LABEL}" >/dev/null 2>&1 && break
  sleep 2
done
BODY
}

restart_daemon() {
  local script
  script="$(mktemp)"
  restart_daemon_body \
    | sed -e "s#__LABEL__#${LABEL}#" -e "s#__PLIST__#${PLIST}#" \
          -e "s#__SUPERVISOR_BIN__#${SUPERVISOR_BIN}#" -e "s#__BIN__#${BIN}#" \
          -e "s#__LAUNCHCTL__#${LAUNCHCTL}#" -e "s#__RESTART_WAIT_SECS__#${RESTART_WAIT_SECS}#" \
          -e "s#__MAINTENANCE__#${MAINTENANCE}#" \
    >"$script"
  # Detach: a killed ssh session or Ctrl-C must not strand the service between
  # bootout and bootstrap. That is exactly how the router was left down once.
  # macOS ships no setsid, and a backgrounded subshell always reports success,
  # so the fallback has to be chosen before the fork, not after it.
  if command -v setsid >/dev/null 2>&1; then
    setsid bash "$script" >/dev/null 2>&1 </dev/null &
  else
    nohup bash "$script" >/dev/null 2>&1 </dev/null &
  fi
  disown 2>/dev/null || true
  local deadline=$((SECONDS + HEALTH_TIMEOUT_SECS))
  while [ "$SECONDS" -lt "$deadline" ]; do
    health_ok && { rm -f "$script"; return 0; }
    sleep 2
  done
  rm -f "$script"
  return 1
}

cmd_restart_daemon() {
  take_lock
  : >"$MAINTENANCE"
  # The sentinel is removed on every exit path, including an interrupt, so a
  # killed restart cannot also leave the watchdog muzzled.
  trap 'rm -f "$MAINTENANCE" 2>/dev/null || true; release_lock' EXIT
  log "restarting ${LABEL}"
  if restart_daemon; then
    log "health is answering again"
  else
    die "the service did not come back within ${HEALTH_TIMEOUT_SECS}s; check /var/log/subrouter-guard.log"
  fi
}

cmd_install_supervisor() {
  local candidate="${1:-}"
  [ -n "$candidate" ] || { usage; exit 2; }
  [ -f "$candidate" ] || die "$candidate does not exist"
  [ -x "$candidate" ] || die "$candidate is not executable"
  "$candidate" --help >/dev/null 2>&1 || die "$candidate does not answer --help; wrong arch or a corrupt download"
  local candidate_sha current_sha
  candidate_sha="$(sha_of "$candidate")"
  current_sha="$(sha_of "$SUPERVISOR_BIN")"
  [ "$candidate_sha" != "$current_sha" ] || { log "supervisor is already installed ($candidate_sha)"; exit 0; }
  health_ok || die "public health is down right now; fix the outage before replacing the supervisor"

  take_lock
  : >"$MAINTENANCE"
  trap 'rm -f "$MAINTENANCE" 2>/dev/null || true; release_lock' EXIT

  local backup="${SUPERVISOR_BIN}.backup-$(date +%Y%m%d-%H%M%S)"
  cp -p "$SUPERVISOR_BIN" "$backup"
  log "current supervisor ${current_sha:0:12} saved to $backup"
  install -m 0755 "$candidate" "${SUPERVISOR_BIN}.new"
  mv -f "${SUPERVISOR_BIN}.new" "$SUPERVISOR_BIN"

  if restart_daemon; then
    log "installed supervisor ${candidate_sha:0:12}; health is answering"
    return 0
  fi
  log "supervisor ${candidate_sha:0:12} did not bring the service back; restoring ${current_sha:0:12}"
  install -m 0755 "$backup" "${SUPERVISOR_BIN}.rb"
  mv -f "${SUPERVISOR_BIN}.rb" "$SUPERVISOR_BIN"
  restart_daemon || log "the restored supervisor is not answering either; this needs a human"
  die "supervisor install failed and the previous binary was restored"
}

case "${1:-}" in
  install) shift; cmd_install "$@" ;;
  record-revision) shift; cmd_record_revision "$@" ;;
  reconfigure) shift; cmd_reconfigure "$@" ;;
  install-release) shift; cmd_install_release "$@" ;;
  pin) shift; cmd_pin "$@" ;;
  unpin) shift; cmd_unpin "$@" ;;
  list) shift; cmd_list "$@" ;;
  install-supervisor) shift; cmd_install_supervisor "$@" ;;
  restart-daemon) shift; cmd_restart_daemon "$@" ;;
  rollback) shift; cmd_rollback "$@" ;;
  status) shift; cmd_status "$@" ;;
  -h|--help|help|"") usage ;;
  *) usage; exit 2 ;;
esac
