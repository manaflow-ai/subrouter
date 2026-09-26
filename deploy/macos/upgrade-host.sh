#!/usr/bin/env bash
# upgrade-host.sh moves a supervised macOS team host to a commit of this
# repository (main by default), in one command from an operator's Mac.
#
#   upgrade-host.sh [--ref REF] [--plan] [--wait-mins N] [--] SSH_ARGS...
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
set -euo pipefail

REF="main"
PLAN=0
WAIT_MINS=30
ON_HOST=0

usage() {
  if [ -f "$0" ]; then sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'
  else echo "usage: upgrade-host.sh [--ref REF] [--plan] [--wait-mins N] [--] SSH_ARGS...  (see deploy/macos/DEPLOY.md)"; fi
  exit "${1:-2}"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --ref) REF="${2:?--ref needs a value}"; shift 2 ;;
    --plan) PLAN=1; shift ;;
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

LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter-team}"
PLIST="/Library/LaunchDaemons/${LABEL}.plist"
BIN="/usr/local/bin/subrouter"
SUPERVISOR="/usr/local/libexec/subrouter-supervisor"
DEPLOY="/usr/local/bin/subrouter-deploy.sh"
STATE="/var/lib/subrouter-verify"
SERVICE_HOME="/var/lib/subrouter"
WORKER_CONFIG="${SERVICE_HOME}/worker-config.json"
REPO_URL="https://github.com/manaflow-ai/subrouter.git"
REPO_CACHE="${STATE}/subrouter.git"
VERSION_FILE="/etc/subrouter-version"
HEALTH_URL="http://127.0.0.1:31415/_subrouter/health"
BACKUPS="${STATE}/upgrade-backups"
KEEP=3
WATCH_SECS="${SUBROUTER_UPGRADE_WATCH_SECS:-120}"
LOG="/var/log/subrouter-upgrade.log"
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

say "host $(hostname -s), ref $REF$([ "$PLAN" -eq 0 ] || echo ', plan only')"
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
    -ldflags "-s -w -X ${pkg}.version=${label} -X ${pkg}.commit=${SHA:0:12} -X ${pkg}.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o "$work/subrouter" ./cmd/subrouter) >&2 || die "build failed"
  sudo -n rm -rf "${stage}.partial"
  sudo -n install -d -m 0755 "${stage}.partial"
  sudo -n install -m 0755 "$work/subrouter" "${stage}.partial/subrouter"
  printf '%s\n' "$label" | sudo -n tee "${stage}.partial/label" >/dev/null
  sudo -n rm -rf "$stage"
  sudo -n mv "${stage}.partial" "$stage"
fi
# Keep the three newest staged builds besides this one.
sudo -n ls -1t "${STATE}/upgrade" | grep -v "^${SHA}\$" | awk 'NR > 3' | while read -r old; do
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
iso="$(cd / && sudo -n -u "$SERVICE_USER" env ${worker_env[@]+"${worker_env[@]}"} \
  "$CANDIDATE" codex isolation-check --json 2>&1)" || die "codex isolation-check refused the live state: $iso"
say "preflight ok: $iso"

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
ts="$(date -u +%Y%m%dT%H%M%SZ)"
bk="${BACKUPS}/${ts}"
sudo -n install -d -m 0700 "$BACKUPS" "$bk"
sudo -n cp -p "$BIN" "$bk/subrouter"
sudo -n cp -p "$SUPERVISOR" "$bk/subrouter-supervisor"
sudo -n cp -p "$PLIST" "$bk/"
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
INHIBIT="${PLIST}.supervisor-transaction/upgrade-inhibited"
WROTE_PIN=0
unpin_ours() { [ "$WROTE_PIN" -eq 0 ] || sudo -n rm -f "$INHIBIT" || true; }
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
  if sudo -n mkdir "$STATE/deploy.lock" 2>/dev/null; then
    locked=1
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
  [ "$(shasum -a 256 "$BIN" | awk '{print $1}')" = "$CAND_SHA" ] || die "the worker binary changed under the watch; not touching it"
  health_ok || { sleep 5; health_ok || rollback "loopback health failed after the swap"; }
  [ "$TAILNET_CHECK" -eq 0 ] || tailnet_up || { sleep 5; tailnet_up || rollback "tailnet listener did not answer after the swap"; }
done

sudo -n "$DEPLOY" status >&2 || true
say "OK: ${LIVE_VERSION} -> ${LABEL_TEXT} (${SHA:0:12}); backup at $bk"
