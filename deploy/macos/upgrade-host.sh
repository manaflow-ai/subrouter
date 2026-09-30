#!/usr/bin/env bash
# upgrade-host.sh moves a supervised macOS team host to a commit of this
# repository (main by default), in one command from an operator's Mac.
#
#   upgrade-host.sh [--ref REF] [--plan] [--wait-mins N] [--enable-rollouts] [--] SSH_ARGS...
#
# SSH_ARGS are passed to ssh as they are, so a jump host and key work:
#
#   upgrade-host.sh -J browser-a -i ~/.ssh/id_ed25519_manaflow_cmux cmux-lawrence@172.20.21.158
#
# The script copies itself to the host over ssh stdin and runs there as the
# login user, taking root through `sudo -n` only for the steps that need it.
# On the host it:
#
# 1. waits while the host is busy: another writer holds deploy.lock, the
#    maintenance sentinel is fresh, public health is down, or the load is above
#    2x the core count. It retries every 60s for --wait-mins (default 30).
# 2. fetches REF into the deploy script's repository cache and builds
#    cmd/subrouter from a `git archive` of that commit, at low priority.
# 3. preflights the candidate: `--help`, then `codex isolation-check` run as
#    the service user against the live state (read-only). A candidate that
#    would refuse to serve this state is never installed.
# 4. backs up the live worker, supervisor, plist, worker config, version file,
#    revision records and the service state (without logs and transcripts) to
#    /var/lib/subrouter-verify/upgrade-backups/<timestamp>, keeping three.
# 5. pins autoupdate if nothing pinned it already (subrouter-autoupdate.sh
#    would put the latest release back over a main build), then hot-swaps the
#    worker with `subrouter-deploy.sh install`. The listener never closes;
#    that script restores the old worker by itself if the candidate never
#    becomes ready or public health drops. Refusals made before the swap
#    (lock held, health down) are retried; a failed candidate is not.
# 6. watches health for SUBROUTER_UPGRADE_WATCH_SECS (default 120) on loopback
#    and, if it answered before the swap, on the tailnet address. Any failure
#    copies the backed-up worker back and asks the supervisor for a new generation directly (deploy.sh
#    refuses to install while health is down), then restores the version file
#    and removes a pin this run wrote.
#
# On the host the work runs under nohup with output in a log file that ssh
# follows, so a dropped ssh session stops only the view, never the upgrade.
# --plan stops after step 3 (plus a dry run of the state backup) and changes
# nothing. The supervisor is not replaced; this script only moves the worker.
#
# --enable-rollouts adopts canary rollouts (RFC #444 step C) instead of moving
# the worker. After the same build, preflight and backup it:
#
# a. installs REF's subrouter-deploy.sh, subrouter-guard.sh, subrouter-verify.sh,
#    subrouter-autoupdate.sh, release-bake-lib.sh, mutation-lease-lib.sh and
#    subrouter-supervisor-handoff.sh into /usr/local/bin (the replaced copies
#    are kept in the backup);
# b. hands the supervisor off to the REF build with `subrouter-deploy.sh
#    handoff-supervisor --adopt-worker-config` (a pf redirect to a bridge
#    supervisor; the port never closes), unless it already runs that build;
# c. adds SUBROUTER_RELEASE_STATE to the worker config's env with
#    `subrouter-deploy.sh reconfigure` (a hot upgrade), unless it is there;
# d. leaves autoupdate off: an existing pin stays, and with none it writes one
#    before step a, so the new autoupdate cannot start a canary that step c's
#    reconfigure would supersede. Step c refuses while a canary is pending.
#    It prints the command that turns autoupdate on once releases exist.
#
# From then on `subrouter-deploy.sh install`, `install-release`, autoupdate and
# the plain path of this script send a new worker out as a supervisor canary.
set -euo pipefail

REF="main"
PLAN=0
WAIT_MINS=30
ON_HOST=0
ENABLE_ROLLOUTS=0

usage() {
  if [ -f "$0" ]; then sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'
  else echo "usage: upgrade-host.sh [--ref REF] [--plan] [--wait-mins N] [--enable-rollouts] [--] SSH_ARGS...  (see deploy/macos/DEPLOY.md)"; fi
  exit "${1:-2}"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --ref) REF="${2:?--ref needs a value}"; shift 2 ;;
    --plan) PLAN=1; shift ;;
    --enable-rollouts) ENABLE_ROLLOUTS=1; shift ;;
    --wait-mins) WAIT_MINS="${2:?--wait-mins needs a value}"; shift 2 ;;
    --on-host) ON_HOST=1; shift ;;
    -h|--help) usage 0 ;;
    --) shift; break ;;
    *) break ;;
  esac
done

if [ "$ON_HOST" -eq 0 ]; then
  [ $# -gt 0 ] || usage 2
  case "$REF" in *[!A-Za-z0-9._/-]*) echo "upgrade-host: bad --ref '$REF'" >&2; exit 2 ;; esac
  case "$WAIT_MINS" in ''|*[!0-9]*) echo "upgrade-host: --wait-mins needs a number" >&2; exit 2 ;; esac
  flags="--on-host --ref $REF --wait-mins $WAIT_MINS"
  [ "$PLAN" -eq 0 ] || flags="$flags --plan"
  [ "$ENABLE_ROLLOUTS" -eq 0 ] || flags="$flags --enable-rollouts"
  src="${BASH_SOURCE[0]:-}"
  if [ -z "$src" ] || [ ! -f "$src" ]; then
    # Run through `curl ... | bash -s --`: the text is gone from the pipe, so
    # send the published copy of this script at the same ref.
    src="$(mktemp "${TMPDIR:-/tmp}/upgrade-host.XXXXXX")"
    trap 'rm -f "$src"' EXIT
    curl -fsSL "https://raw.githubusercontent.com/manaflow-ai/subrouter/${REF}/deploy/macos/upgrade-host.sh" -o "$src" ||
      { echo "upgrade-host: cannot download upgrade-host.sh at $REF" >&2; exit 1; }
  fi
  # The host saves the script to a file before running it, so nothing it runs
  # can read the rest of the script from stdin.
  # shellcheck disable=SC2029  # flags are validated above and meant to expand here
  ssh -o ServerAliveInterval=15 -o ServerAliveCountMax=8 "$@" \
    "f=\$(mktemp /tmp/upgrade-host.XXXXXX) && cat >\"\$f\" && bash \"\$f\" $flags; rc=\$?; rm -f \"\$f\"; exit \$rc" <"$src"
  exit $?
fi

# ---------------------------------------------------------------- on the host
# Detach first: the rest runs under nohup with its output in a file, and this
# process only follows that file. A dropped ssh session then kills the view,
# while the upgrade finishes (and rolls back if it must) on its own.
if [ -z "${UPGRADE_HOST_DETACHED:-}" ]; then
  mkdir -p "$HOME/.cache"
  self="$(mktemp "$HOME/.cache/upgrade-host-run.XXXXXX")"
  cp "$0" "$self"
  runlog="${self}.log"
  : >"$runlog"
  flags=(--on-host --ref "$REF" --wait-mins "$WAIT_MINS")
  [ "$PLAN" -eq 0 ] || flags+=(--plan)
  [ "$ENABLE_ROLLOUTS" -eq 0 ] || flags+=(--enable-rollouts)
  UPGRADE_HOST_DETACHED=1 nohup bash "$self" "${flags[@]}" >>"$runlog" 2>&1 </dev/null &
  pid=$!
  tail -n +1 -f "$runlog" &
  tailpid=$!
  rc=0
  wait "$pid" || rc=$?
  sleep 1
  kill "$tailpid" 2>/dev/null || true
  rm -f "$self" "$runlog"
  exit "$rc"
fi

# The overrides exist for deploy/macos/tests; a real host uses the defaults
# (sudo drops them anyway).
LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter-team}"
PLIST="${SUBROUTER_PLIST:-/Library/LaunchDaemons/${LABEL}.plist}"
BIN="${SUBROUTER_BIN:-/usr/local/bin/subrouter}"
SUPERVISOR="${SUBROUTER_SUPERVISOR_BIN:-/usr/local/libexec/subrouter-supervisor}"
SCRIPTS_DIR="${SUBROUTER_SCRIPTS_DIR:-/usr/local/bin}"
DEPLOY="${SCRIPTS_DIR}/subrouter-deploy.sh"
STATE="${SUBROUTER_DEPLOY_STATE:-/var/lib/subrouter-verify}"
SERVICE_HOME="${SUBROUTER_SERVICE_HOME:-/var/lib/subrouter}"
WORKER_CONFIG="${SUBROUTER_WORKER_CONFIG:-${SERVICE_HOME}/worker-config.json}"
REPO_URL="${SUBROUTER_REPO_URL:-https://github.com/manaflow-ai/subrouter.git}"
REPO_CACHE="${STATE}/subrouter.git"
VERSION_FILE="${SUBROUTER_VERSION_FILE:-/etc/subrouter-version}"
HEALTH_URL="${SUBROUTER_HEALTH_URL:-http://127.0.0.1:31415/_subrouter/health}"
BACKUPS="${STATE}/upgrade-backups"
KEEP=3
WATCH_SECS="${SUBROUTER_UPGRADE_WATCH_SECS:-120}"
LOG="${SUBROUTER_UPGRADE_LOG:-/var/log/subrouter-upgrade.log}"
RELEASE_STATE_PATH="${STATE}/release-state.json"
INHIBIT="${PLIST}.supervisor-transaction/upgrade-inhibited"
# What --enable-rollouts installs into $SCRIPTS_DIR, from deploy/macos at REF.
ROLLOUT_SCRIPTS="subrouter-deploy.sh subrouter-guard.sh subrouter-verify.sh subrouter-autoupdate.sh release-bake-lib.sh mutation-lease-lib.sh subrouter-supervisor-handoff.sh"
GO="$(command -v go || { [ -x /opt/homebrew/bin/go ] && echo /opt/homebrew/bin/go; } || echo /usr/local/go/bin/go)"

say() { printf '%s upgrade-host: %s\n' "$(date -u +%H:%M:%SZ)" "$*" | sudo -n tee -a "$LOG" >&2 || true; }
die() { say "FAILED: $*"; exit 1; }

sudo -n true 2>/dev/null || die "needs passwordless sudo as $(id -un) on $(hostname -s)"
[ -x "$DEPLOY" ] || die "$DEPLOY is missing; this host is not on the supervised deploy path"
[ -x "$GO" ] || die "go is not installed (looked for $GO)"

health_ok() {
  local body
  body="$(curl -fsS --max-time 5 "$HEALTH_URL" 2>/dev/null)" || return 1
  printf '%s' "$body" | grep -q '"ok": *true'
}

tailnet_up() {
  local ip code
  ip="$( (tailscale ip -4 || /Applications/Tailscale.app/Contents/MacOS/Tailscale ip -4) 2>/dev/null | head -n1)"
  [ -n "$ip" ] || return 0 # no tailnet here; loopback health is the check
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://${ip}:31415/healthz" || true)"
  # The team server answers 403 to a caller that is not a tailnet peer it
  # authorizes (itself included); any HTTP answer means the listener is up.
  [ -n "$code" ] && [ "$code" != "000" ]
}

tailnet_up_strict() { # like tailnet_up, but false when there is no tailnet address
  local ip
  ip="$( (tailscale ip -4 || /Applications/Tailscale.app/Contents/MacOS/Tailscale ip -4) 2>/dev/null | head -n1)"
  [ -n "$ip" ] && tailnet_up
}

busy_reason() {
  local owner age
  if sudo -n test -d "$STATE/deploy.lock"; then
    owner="$(sudo -n cat "$STATE/deploy.lock/owner" 2>/dev/null || echo unknown)"
    echo "deploy.lock is held by ${owner}"; return
  fi
  if sudo -n test -f "$STATE/maintenance"; then
    age=$(( $(date +%s) - $(sudo -n stat -f %m "$STATE/maintenance" 2>/dev/null || date +%s) ))
    [ "$age" -ge 5400 ] || { echo "maintenance sentinel set ${age}s ago"; return; }
  fi
  health_ok || { echo "public health is down"; return; }
  local load cores
  load="$(sysctl -n vm.loadavg | awk '{print int($2)}')"
  cores="$(sysctl -n hw.ncpu)"
  [ "$load" -le $((cores * 2)) ] || { echo "load ${load} on ${cores} cores"; return; }
  echo ""
}

wait_until_idle() {
  local deadline=$((SECONDS + WAIT_MINS * 60)) reason
  while :; do
    reason="$(busy_reason)"
    [ -n "$reason" ] || return 0
    [ "$SECONDS" -lt "$deadline" ] || die "host still busy after ${WAIT_MINS} min: $reason"
    say "busy ($reason); retrying in 60s"
    sleep 60
  done
}

control_socket() {
  if [ -n "${SUBROUTER_CONTROL_SOCKET:-}" ]; then printf '%s\n' "$SUBROUTER_CONTROL_SOCKET"; return; fi
  sudo -n python3 -c '
import plistlib, sys
args = plistlib.load(open(sys.argv[1], "rb")).get("ProgramArguments") or []
for i, a in enumerate(args):
    if a == "--control-socket" and i + 1 < len(args):
        print(args[i + 1]); break
    if a.startswith("--control-socket="):
        print(a.split("=", 1)[1]); break
' "$PLIST"
}

backup_state() { # backup_state <archive>
  sudo -n tar -C "$SERVICE_HOME" --exclude ./logs --exclude ./transcripts --exclude '*.sock' -czf "$1" .
}

wait_health() { # wait_health <seconds>
  local deadline=$((SECONDS + $1))
  while [ "$SECONDS" -lt "$deadline" ]; do health_ok && return 0; sleep 1; done
  return 1
}

say "host $(hostname -s), ref $REF$([ "$ENABLE_ROLLOUTS" -eq 0 ] || echo ', enable rollouts')$([ "$PLAN" -eq 0 ] || echo ', plan only')"
wait_until_idle

# 2. fetch and build ------------------------------------------------------
if ! sudo -n test -d "$REPO_CACHE"; then
  sudo -n git init --quiet --bare "$REPO_CACHE"
fi
GIT_TERMINAL_PROMPT=0 sudo -n git --git-dir="$REPO_CACHE" fetch --quiet --prune --no-tags \
  "$REPO_URL" '+refs/heads/*:refs/heads/*' || die "cannot fetch $REPO_URL"
if printf '%s' "$REF" | grep -Eq '^[0-9a-f]{40}$'; then
  SHA="$REF"
else
  SHA="$(sudo -n git --git-dir="$REPO_CACHE" rev-parse --verify --quiet "refs/heads/${REF}^{commit}" ||
    sudo -n git --git-dir="$REPO_CACHE" rev-parse --verify --quiet "${REF}^{commit}")" || die "unknown ref $REF"
fi
SUBJECT="$(sudo -n git --git-dir="$REPO_CACHE" log -1 --format=%s "$SHA" 2>/dev/null)" ||
  die "commit $SHA is not on any branch of $REPO_URL; push it first"
say "target ${SHA:0:12} ${SUBJECT}"

LIVE_SHA="$(shasum -a 256 "$BIN" | awk '{print $1}')"
LIVE_VERSION="$(cat "$VERSION_FILE" 2>/dev/null || true)"
LIVE_REV="$(sudo -n cat "$STATE/revisions/$LIVE_SHA" 2>/dev/null | head -n1 || true)"
[ -n "$LIVE_VERSION" ] || LIVE_VERSION="rollback:${LIVE_SHA:0:12}"
say "live ${LIVE_SHA:0:12} version '${LIVE_VERSION}' revision ${LIVE_REV:-unrecorded}"

stage="${STATE}/upgrade/${SHA}"
# A stage directory appears only complete (binary and label), by rename.
if ! sudo -n test -f "$stage/label"; then
  mkdir -p "$HOME/.cache"
  work="$(mktemp -d "$HOME/.cache/subrouter-upgrade.XXXXXX")"
  trap 'rm -rf "$work"' EXIT
  sudo -n git --git-dir="$REPO_CACHE" archive "$SHA" | tar -x -C "$work"
  pkg="github.com/manaflow-ai/subrouter/internal/buildversion"
  case "$REF" in
    main) label="main-${SHA:0:12}" ;;
    "$SHA") label="rev-${SHA:0:12}" ;;
    *) label="${REF##*/}-${SHA:0:12}" ;;
  esac
  say "building ${label} (nice 15, 4 procs)"
  (cd "$work" && CGO_ENABLED=0 GOMAXPROCS=4 nice -n 15 "$GO" build -p 4 -trimpath \
    -ldflags "-s -w -X ${pkg}.version=${label} -X ${pkg}.commit=${SHA:0:12} -X ${pkg}.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ) -X ${pkg}.mainline=local-build" \
    -o "$work/subrouter" ./cmd/subrouter) >&2 || die "build failed"
  sudo -n rm -rf "${stage}.partial"
  sudo -n install -d -m 0755 "${stage}.partial"
  sudo -n install -m 0755 "$work/subrouter" "${stage}.partial/subrouter"
  printf '%s\n' "$label" | sudo -n tee "${stage}.partial/label" >/dev/null
  sudo -n rm -rf "$stage"
  sudo -n mv "${stage}.partial" "$stage"
fi
# Keep the three newest staged builds besides this one.
{ sudo -n ls -1t "${STATE}/upgrade" | grep -vx -- "$SHA" | awk 'NR > 3' || true; } | while read -r old; do
  case "$old" in *[!0-9a-f.partil]*|'') ;; *) sudo -n rm -rf "${STATE:?}/upgrade/$old" ;; esac
done
CANDIDATE="$stage/subrouter"
LABEL_TEXT="$(sudo -n cat "$stage/label")"
CAND_SHA="$(shasum -a 256 "$CANDIDATE" | awk '{print $1}')"
say "candidate ${CAND_SHA:0:12} at $CANDIDATE"

# 3. preflight ------------------------------------------------------------
SOCKET="$(control_socket)"
sudo -n test -S "$SOCKET" || die "control socket '${SOCKET}' is not a socket; is ${LABEL} running?"
"$CANDIDATE" --help >/dev/null 2>&1 || die "candidate does not answer --help"
# Run it with the environment the worker gets: the plist's, overlaid with the
# worker config's env, as the plist's user.
SERVICE_USER="$(sudo -n python3 -c 'import plistlib,sys; print(plistlib.load(open(sys.argv[1],"rb")).get("UserName") or "root")' "$PLIST")"
worker_env=()
while IFS= read -r kv; do [ -n "$kv" ] && worker_env+=("$kv"); done < <(sudo -n python3 -c '
import json, plistlib, sys
env = dict(plistlib.load(open(sys.argv[1], "rb")).get("EnvironmentVariables") or {})
try:
    env.update((json.load(open(sys.argv[2])) or {}).get("env") or {})
except (OSError, ValueError):
    pass
for k, v in env.items():
    if "\n" not in str(v):
        print("%s=%s" % (k, v))
' "$PLIST" "$WORKER_CONFIG")
iso="$(cd / && sudo -n -H -u "$SERVICE_USER" env ${worker_env[@]+"${worker_env[@]}"} \
  "$CANDIDATE" codex isolation-check --json 2>&1)" || die "codex isolation-check refused the live state: $iso"
say "preflight ok: $iso"

take_backup() { # sets bk
  local ts
  ts="$(date -u +%Y%m%dT%H%M%SZ)"
  bk="${BACKUPS}/${ts}"
  sudo -n install -d -m 0700 "$BACKUPS" "$bk"
  sudo -n cp -p "$BIN" "$bk/subrouter"
  sudo -n cp -p "$SUPERVISOR" "$bk/subrouter-supervisor"
  sudo -n cp -p "$PLIST" "$bk/"
  sudo -n cp -p "$WORKER_CONFIG" "$bk/worker-config.json" 2>/dev/null || true
  sudo -n cp -p "$VERSION_FILE" "$bk/subrouter-version" 2>/dev/null || true
  sudo -n cp -pR "$STATE/revisions" "$bk/revisions" 2>/dev/null || true
  backup_state "$bk/state.tgz" || die "state backup failed; nothing was changed"
  printf 'live_sha=%s\nlive_version=%s\nlive_rev=%s\ntarget=%s\n' \
    "$LIVE_SHA" "$LIVE_VERSION" "${LIVE_REV:-}" "$SHA" | sudo -n tee "$bk/receipt" >/dev/null
  say "backed up to $bk ($(sudo -n du -sh "$bk" | awk '{print $1}'))"
  # Keep the newest $KEEP backups (names sort by time).
  sudo -n ls -1 "$BACKUPS" | sort -r | awk -v keep="$KEEP" 'NR > keep' | while read -r old; do
    case "$old" in 20[0-9][0-9]*Z) sudo -n rm -rf "${BACKUPS:?}/$old" ;; esac
  done
}

canary_endpoint_ok() { # the running supervisor answers GET /_subrouter/canary
  sudo -n curl -fsS --max-time 10 --unix-socket "$(control_socket)" http://localhost/_subrouter/canary 2>/dev/null | grep -q '"state"'
}

worker_config_wired() {
  sudo -n env WANT="$WORKER_CONFIG" python3 -c '
import os, plistlib, sys
args = plistlib.load(open(sys.argv[1], "rb")).get("ProgramArguments") or []
want = os.environ["WANT"]
for i, a in enumerate(args):
    if a == "--":
        break
    if (a == "--worker-config" and i + 1 < len(args) and args[i + 1] == want) or a == "--worker-config=" + want:
        sys.exit(0)
sys.exit(1)
' "$PLIST"
}

# release_state_wired: the worker config env already names the release state.
release_state_wired() {
  sudo -n env WANT="$RELEASE_STATE_PATH" python3 -c '
import json, os, sys
try:
    env = (json.load(open(sys.argv[1])) or {}).get("env") or {}
except (OSError, ValueError):
    sys.exit(1)
sys.exit(0 if env.get("SUBROUTER_RELEASE_STATE") == os.environ["WANT"] else 1)
' "$WORKER_CONFIG"
}

canary_pending() { # canary-rollout.json names a rollout that is not resolved
  sudo -n python3 -c '
import json, sys
try:
    doc = json.load(open(sys.argv[1]))
except (OSError, ValueError):
    sys.exit(1)
sys.exit(0 if not doc.get("resolved") else 1)
' "$STATE/canary-rollout.json" 2>/dev/null
}

# enable_rollouts is --enable-rollouts: steps a-d in the header.
enable_rollouts() {
  local src f changed="" sup_sha
  src="$(mktemp -d "$HOME/.cache/subrouter-rollouts.XXXXXX")"
  sudo -n git --git-dir="$REPO_CACHE" archive "$SHA" deploy/macos | tar -x -C "$src" ||
    die "cannot read deploy/macos at ${SHA:0:12}"
  for f in $ROLLOUT_SCRIPTS; do
    [ -f "$src/deploy/macos/$f" ] || die "${SHA:0:12} has no deploy/macos/$f; --enable-rollouts needs a ref with RFC #444 step C"
    sudo -n cmp -s "$src/deploy/macos/$f" "$SCRIPTS_DIR/$f" 2>/dev/null || changed="$changed $f"
  done
  sup_sha="$(sudo -n shasum -a 256 "$SUPERVISOR" | awk '{print $1}')"
  local handoff=0
  if [ "$sup_sha" != "$CAND_SHA" ] || ! worker_config_wired; then handoff=1; fi
  local reconfigure=1
  release_state_wired && reconfigure=0
  say "a. scripts to install:${changed:- none, all current}"
  say "b. supervisor ${sup_sha:0:12} -> ${CAND_SHA:0:12}: $([ "$handoff" -eq 1 ] && echo 'handoff-supervisor --adopt-worker-config' || echo 'already this build with --worker-config, skipped')"
  say "c. worker config env SUBROUTER_RELEASE_STATE=${RELEASE_STATE_PATH}: $([ "$reconfigure" -eq 1 ] && echo 'reconfigure' || echo 'already set, skipped')"
  if [ "$PLAN" -eq 1 ]; then
    rm -rf "$src"
    say "plan only: nothing was changed"
    return 0
  fi

  wait_until_idle
  take_backup

  # d, first. Autoupdate stays off until releases are cut on green main
  # (#444 B). The pin goes in before the new scripts, so the new autoupdate
  # never starts a canary that step c's reconfigure would then supersede.
  if sudo -n test -e "$INHIBIT"; then
    say "d. autoupdate stays pinned: $(sudo -n sed -n 1p "$INHIBIT")"
  else
    sudo -n install -d -m 0755 "$(dirname "$INHIBIT")"
    printf 'pinned by upgrade-host.sh --enable-rollouts on %s: autoupdate stays off until releases are cut on green main (RFC #444 step B); subrouter-deploy.sh unpin resumes it\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%SZ)" | sudo -n tee "${INHIBIT}.new" >/dev/null
    sudo -n chmod 0600 "${INHIBIT}.new"
    sudo -n mv -f "${INHIBIT}.new" "$INHIBIT"
    say "d. pinned autoupdate so it stays off"
  fi

  # a. The scripts go in together under deploy.lock, so a guard or autoupdate
  # tick never runs a half-updated set. Each file is replaced by rename.
  if [ -n "$changed" ]; then
    sudo -n mkdir "$STATE/deploy.lock" 2>/dev/null || die "deploy.lock is held by $(sudo -n cat "$STATE/deploy.lock/owner" 2>/dev/null || echo unknown); nothing was installed"
    # From here any exit, including an errexit in tee or install, drops it.
    trap 'rm -rf "${work:-}" "${src:-}"; sudo -n rm -rf "$STATE/deploy.lock"' EXIT
    printf 'upgrade-host.sh --enable-rollouts (pid %s)\n' "$$" | sudo -n tee "$STATE/deploy.lock/owner" >/dev/null
    sudo -n install -d -m 0700 "$bk/scripts"
    for f in $changed; do
      sudo -n cp -p "$SCRIPTS_DIR/$f" "$bk/scripts/$f" 2>/dev/null || true
      sudo -n install -m 0755 "$src/deploy/macos/$f" "$SCRIPTS_DIR/$f.new" && sudo -n mv -f "$SCRIPTS_DIR/$f.new" "$SCRIPTS_DIR/$f" || {
        sudo -n rm -rf "$STATE/deploy.lock"
        die "could not install $SCRIPTS_DIR/$f; the replaced scripts are in $bk/scripts"
      }
    done
    sudo -n rm -rf "$STATE/deploy.lock"
    trap 'rm -rf "${work:-}" "${src:-}"' EXIT
    say "a. installed${changed} into $SCRIPTS_DIR (the replaced copies are in $bk/scripts)"
  fi
  rm -rf "$src"

  # b. The supervisor owns the listener; a restart would close it, so it is
  # handed off behind a pf redirect instead.
  if [ "$handoff" -eq 1 ]; then
    wait_until_idle
    say "b. handing the supervisor off to ${CAND_SHA:0:12}; the port stays open (log: /var/log/subrouter-handoff.log)"
    sudo -n "$DEPLOY" handoff-supervisor "$CANDIDATE" --adopt-worker-config >&2 ||
      die "the supervisor handoff failed; it restores the previous supervisor itself (see above). Scripts from ${SHA:0:12} stay installed and fall back to plain upgrades; the old ones are in $bk/scripts"
  fi
  canary_endpoint_ok || die "the supervisor does not answer GET /_subrouter/canary after the handoff"
  say "b. supervisor ${CAND_SHA:0:12} serves and answers /_subrouter/canary"

  # c. Worker env is a hot reconfigure, never a plist edit. A reconfigure is a
  # plain upgrade and would supersede a pending canary with a generation
  # started from the candidate binary, so it waits for none to be pending
  # (subrouter-deploy.sh reconfigure refuses too).
  if [ "$reconfigure" -eq 1 ] && canary_pending; then
    die "a canary rollout is pending ($(sudo -n cat "$STATE/canary-rollout.json" 2>/dev/null | tr -d '\n' | cut -c1-200)); let it finish or run 'sudo $DEPLOY abort', then run --enable-rollouts again"
  fi
  if [ "$reconfigure" -eq 1 ]; then
    local next="$STATE/worker-config.enable-rollouts.json"
    sudo -n env WANT="$RELEASE_STATE_PATH" python3 -c '
import json, os, sys
source, target = sys.argv[1:3]
try:
    doc = json.load(open(source))
except (OSError, ValueError) as error:
    sys.exit(f"cannot read {source}: {error}")
env = doc.get("env") or {}
env["SUBROUTER_RELEASE_STATE"] = os.environ["WANT"]
doc["env"] = env
fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w") as stream:
    json.dump(doc, stream, indent=2)
    stream.write("\n")
' "$WORKER_CONFIG" "$next" || die "cannot prepare the worker config"
    wait_until_idle
    if ! sudo -n "$DEPLOY" reconfigure "$next" >&2; then
      sudo -n rm -f "$next"
      die "reconfigure failed and restored the previous worker config (see above)"
    fi
    sudo -n rm -f "$next"
    say "c. worker env now has SUBROUTER_RELEASE_STATE=${RELEASE_STATE_PATH}"
  fi

  sudo -n "$DEPLOY" status >&2 || true
  say "OK: rollouts enabled on $(hostname -s) from ${SHA:0:12}; installs now go out as supervisor canaries (5% -> 25% -> 100%)"
  say "autoupdate is OFF. Once releases exist, turn it on with: sudo $DEPLOY unpin"
  if ! grep -ls 'subrouter-autoupdate.sh' /Library/LaunchDaemons/*.plist >/dev/null 2>&1; then
    say "note: no LaunchDaemon runs $SCRIPTS_DIR/subrouter-autoupdate.sh on this host yet; unpinning alone will not schedule it"
  fi
  return 0
}

if [ "$ENABLE_ROLLOUTS" -eq 1 ]; then
  enable_rollouts
  exit 0
fi

if [ "$CAND_SHA" = "$LIVE_SHA" ]; then
  say "candidate is already live; nothing to do"
  exit 0
fi
if [ "$PLAN" -eq 1 ]; then
  backup_state /dev/null || die "the state backup would fail (see above)"
  say "plan only: would back up and hot-swap ${LIVE_SHA:0:12} -> ${CAND_SHA:0:12} (${LABEL_TEXT})"
  exit 0
fi

# The tailnet listener is checked after the swap only if it answers now; a
# host that listens on loopback only, or behind tailscale serve, never does.
TAILNET_CHECK=0
if tailnet_up_strict 2>/dev/null; then TAILNET_CHECK=1; fi

# 4. backup ---------------------------------------------------------------
wait_until_idle
take_backup

# 5. hot swap -------------------------------------------------------------
# The live worker may come from a deploy branch that the target does not
# contain, so the lineage check cannot pass; the reason is logged, and the
# target's commit is recorded afterwards so the next install is checked.
# A deploy script without the lineage guard takes neither flag nor the record.
reason="upgrade-host to ${REF}@${SHA:0:12} from ${LIVE_REV:-unrecorded}"
lineage=()
if grep -q -- '--allow-unrelated' "$DEPLOY"; then lineage=(--allow-unrelated "$reason"); fi
# Pin first. /etc/subrouter-version is about to name a main build, which
# subrouter-autoupdate.sh would replace with the latest release. deploy.sh
# borrows an existing pin and puts it back when it exits, so the pin holds
# across the install; an existing pin is kept as it is.
WROTE_PIN=0
unpin_ours() { [ "$WROTE_PIN" -eq 0 ] || sudo -n rm -f "$INHIBIT" || true; WROTE_PIN=0; }
# Until the candidate is installed, any exit (a die, errexit, an interrupt)
# drops a pin this run wrote, so a host is never left pinned to a build it
# does not run.
trap 'rm -rf "${work:-}"; unpin_ours' EXIT
if ! sudo -n test -e "$INHIBIT"; then
  sudo -n install -d -m 0755 "$(dirname "$INHIBIT")"
  printf 'pinned at %s by upgrade-host.sh on %s; subrouter-deploy.sh unpin resumes release autoupdate\n' \
    "$LABEL_TEXT" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" | sudo -n tee "${INHIBIT}.new" >/dev/null
  sudo -n chmod 0600 "${INHIBIT}.new"
  sudo -n mv -f "${INHIBIT}.new" "$INHIBIT"
  WROTE_PIN=1
  say "pinned autoupdate at ${LABEL_TEXT}"
fi

deploy_out="$(mktemp "$HOME/.cache/upgrade-host-deploy.XXXXXX")"
attempt=1
while :; do
  wait_until_idle
  if sudo -n "$DEPLOY" install "$CANDIDATE" ${lineage[@]+"${lineage[@]}"} --label "$LABEL_TEXT" 2>&1 |
    tee "$deploy_out" >&2; then
    break
  fi
  # Retry only a refusal made before anything was touched: another writer
  # held the lock, or health was down when it looked. A candidate that was
  # swapped in and failed is never tried again.
  if [ "$(shasum -a 256 "$BIN" | awk '{print $1}')" = "$LIVE_SHA" ] && [ "$attempt" -lt 3 ] &&
    grep -Eq 'holds .*deploy\.lock|another deploy holds|cannot take .*deploy\.lock|public health is down right now' "$deploy_out"; then
    attempt=$((attempt + 1))
    say "deploy refused before the swap; retry ${attempt}/3 after the host settles"
    continue
  fi
  rm -f "$deploy_out"
  unpin_ours
  die "subrouter-deploy.sh install failed; it restored the previous worker (see above)"
done
rm -f "$deploy_out"
trap 'rm -rf "${work:-}"' EXIT
live_now="$(shasum -a 256 "$BIN" | awk '{print $1}')"
[ "$live_now" = "$CAND_SHA" ] || die "the worker changed right after the install (${live_now:0:12}); not recording or watching it"
if [ "${#lineage[@]}" -gt 0 ]; then
  sudo -n "$DEPLOY" record-revision "$SHA" || say "warning: could not record revision $SHA"
fi

# 7. watch and roll back ----------------------------------------------------
# subrouter-deploy.sh install refuses to run while health is down, which is
# exactly when this rollback runs, so it does the restore itself: the old
# worker goes back over the binary and the supervisor starts a new generation
# behind the bound listener. deploy.lock keeps the guard out meanwhile.
rollback() {
  # Every step runs even if one fails: the lock and the pin must not be left
  # behind, or the guard and every later deploy stand down.
  set +e
  say "rolling back to ${LIVE_SHA:0:12} ($1)"
  local locked=0
  local waited=0
  # Wait up to 60s for another writer, as deploy.sh's own lock does.
  while :; do
    if sudo -n mkdir "$STATE/deploy.lock" 2>/dev/null; then locked=1; break; fi
    [ "$waited" -lt 60 ] || break
    sleep 2; waited=$((waited + 2))
  done
  if [ "$locked" -eq 1 ]; then
    printf 'upgrade-host.sh rollback (pid %s)\n' "$$" | sudo -n tee "$STATE/deploy.lock/owner" >/dev/null
  else
    say "deploy.lock is held by $(sudo -n cat "$STATE/deploy.lock/owner" 2>/dev/null || echo unknown); restoring anyway"
  fi
  sudo -n install -m 0755 "$bk/subrouter" "${BIN}.rollback"
  sudo -n mv -f "${BIN}.rollback" "$BIN"
  sudo -n curl -fsS --max-time 120 --unix-socket "$SOCKET" -X POST "http://localhost/_subrouter/upgrade" >/dev/null ||
    say "the supervisor refused the rollback generation"
  printf '%s\n' "$LIVE_VERSION" | sudo -n tee "$VERSION_FILE" >/dev/null
  # With the bake gate installed, the candidate's bake must not be judged
  # against the worker that is serving again.
  if sudo -n test -f "$STATE/release-state.json"; then
    sudo -n env REASON="upgrade-host rollback: $1" python3 -c '
import datetime, json, os, sys
path = sys.argv[1]
state = json.load(open(path))
if state.get("state") == "baking":
    state["state"] = "rolled_back"
    state["reason"] = os.environ["REASON"]
    state["since"] = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    tmp = path + ".new"
    json.dump(state, open(tmp, "w"), indent=2)
    os.replace(tmp, path)
' "$STATE/release-state.json" || say "could not mark the bake rolled back"
  fi
  unpin_ours
  [ "$locked" -eq 0 ] || sudo -n rm -rf "$STATE/deploy.lock"
  if wait_health 60; then
    say "rolled back to ${LIVE_SHA:0:12}; the listener stayed up. Backup kept at $bk"
  else
    say "health is still down after the rollback; subrouter-guard.sh restores last-good within ~2 min. Backup at $bk"
  fi
  exit 1
}

deadline=$((SECONDS + WATCH_SECS))
while [ "$SECONDS" -lt "$deadline" ]; do
  sleep 5
  if [ "$(shasum -a 256 "$BIN" | awk '{print $1}')" != "$CAND_SHA" ]; then
    # With canary rollouts the supervisor's gate may abort the candidate
    # inside the watch; the guard then puts last-good back and pins.
    aborted="$(sudo -n python3 -c '
import json, sys
try:
    doc = json.load(open(sys.argv[1]))
except (OSError, ValueError):
    raise SystemExit(0)
if doc.get("resolved") == "aborted":
    print(doc.get("reason") or "no reason recorded")
' "$STATE/canary-rollout.json" 2>/dev/null || true)"
    [ -z "$aborted" ] || die "the canary of ${LABEL_TEXT} was aborted ($aborted); last-good is back and autoupdate is pinned"
    die "the worker binary changed under the watch; not touching it"
  fi
  health_ok || { sleep 5; health_ok || rollback "loopback health failed after the swap"; }
  [ "$TAILNET_CHECK" -eq 0 ] || tailnet_up || { sleep 5; tailnet_up || rollback "tailnet listener did not answer after the swap"; }
done

sudo -n "$DEPLOY" status >&2 || true
say "OK: ${LIVE_VERSION} -> ${LABEL_TEXT} (${SHA:0:12}); backup at $bk"
