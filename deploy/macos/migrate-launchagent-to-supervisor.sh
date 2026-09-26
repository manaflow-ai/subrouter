#!/usr/bin/env bash
# Put a per-user Subrouter LaunchAgent behind the supervisor, so upgrading the
# binary stops cutting connections that coding agents are actively streaming on.
#
# The existing migrate-launchdaemon-to-supervisor.sh handles the system-wide
# LaunchDaemon (/Library/LaunchDaemons, `system` domain, root). A developer
# machine runs a per-user LaunchAgent instead (~/Library/LaunchAgents,
# `gui/<uid>` domain, no sudo), which that script cannot touch.
#
# Without the supervisor, replacing ~/bin/subrouter and restarting the agent
# closes every established connection: an agent mid-turn loses its response.
# With it, the supervisor owns the listener, health-checks the replacement, and
# lets the old worker finish the connections it already accepted.
#
# The one-time transition below still drops in-flight connections, because the
# unsupervised process owns those file descriptors. Run it when no agent is
# mid-turn. Every upgrade after it is non-disruptive.
set -euo pipefail

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
# shellcheck disable=SC1091
. "$SCRIPT_DIR/launchagent-transition-lib.sh"
# shellcheck disable=SC1091
. "$SCRIPT_DIR/mutation-lease-lib.sh"

LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter}"
PLIST="${SUBROUTER_PLIST:-$HOME/Library/LaunchAgents/${LABEL}.plist}"
WORKER_BIN="${SUBROUTER_BIN:-$HOME/bin/subrouter}"
SUPERVISOR_BIN="${SUBROUTER_SUPERVISOR_BIN:-$HOME/bin/subrouter-supervisor}"
STATE_DIR="${SUBROUTER_STATE_DIR:-$HOME/.subrouter}"
CONTROL_SOCKET="${SUBROUTER_CONTROL_SOCKET:-${STATE_DIR}/supervisor.sock}"
LOCAL_DATA_SOCKET="${SUBROUTER_LOCAL_DATA_SOCKET:-${STATE_DIR}/local-data.sock}"
DOMAIN="gui/$(id -u)"

PREFLIGHT_CALLBACK="${SUBROUTER_PREFLIGHT_CALLBACK:-}"
CANARY_CALLBACK="${SUBROUTER_CANARY_CALLBACK:-}"
RETIRING_STATE_DIR="${SUBROUTER_RETIRING_STATE_DIR:-}"
PUBLIC_ADDR_OVERRIDE="${SUBROUTER_PUBLIC_ADDR:-}"
WORKER_SERVE_ARGS_JSON="${SUBROUTER_WORKER_SERVE_ARGS_JSON:-}"
CANDIDATE_ENV_JSON="${SUBROUTER_CANDIDATE_ENV_JSON:-}"
PREFLIGHT_TIMEOUT="${SUBROUTER_PREFLIGHT_TIMEOUT:-120}"
CANARY_TIMEOUT="${SUBROUTER_CANARY_TIMEOUT:-300}"
MUTATION_LOCK_FILE="${SUBROUTER_MUTATION_LOCK_FILE:-${PLIST}.supervisor-mutation.lock}"
SERVING_STORE_BINDING="$HOME/.subrouter/codex/.local-serving-store.json"

# Serialize every worker-path mutation and activation with the routine updater.
# A dedicated helper owns the kernel descriptor and releases it on parent exit
# or crash; callback descendants never inherit the lease.
acquire_subrouter_mutation_lease "$MUTATION_LOCK_FILE" \
  || { status=$?; echo "migrate-launchagent-to-supervisor: another deployment or worker update holds $MUTATION_LOCK_FILE" >&2; exit "$status"; }
export SUBROUTER_MUTATION_LEASE_OWNER_PID="$$"
export SUBROUTER_MUTATION_LEASE_HELPER_PID="$SUBROUTER_MUTATION_LEASE_PID"
export SUBROUTER_MUTATION_LEASE_CONTROL_DIR
trap release_subrouter_mutation_lease EXIT

die() { echo "migrate-launchagent-to-supervisor: $*" >&2; exit 1; }
private_serving_store_binding_mode() {
  python3 - "$1" <<'PY'
import os
import stat
import sys

path = sys.argv[1]
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
        raise SystemExit("serving-store binding must be a current-user-owned private regular file")
    directory = os.stat(os.path.realpath(os.path.dirname(path)))
    if not stat.S_ISDIR(directory.st_mode) or directory.st_uid != os.getuid() or directory.st_mode & 0o022:
        raise SystemExit("serving-store binding directory must be current-user-owned and not writable by group/other")
    print(format(stat.S_IMODE(opened.st_mode), "03o"))
finally:
    os.close(descriptor)
PY
}
verify_candidate_serving_store_binding() {
  python3 - "$1" "$2" "$3" <<'PY'
import json
import os
import stat
import sys

path, state_dir, local_data_socket = sys.argv[1:]
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
        raise SystemExit("published serving-store binding is not a private regular file")
    body = os.read(descriptor, 4097)
finally:
    os.close(descriptor)

def reject_duplicates(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate field")
        result[key] = value
    return result

try:
    binding = json.loads(body, object_pairs_hook=reject_duplicates)
except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
    raise SystemExit(f"published serving-store binding is invalid JSON: {error}") from error
expected_accounts = os.path.realpath(os.path.join(state_dir, "codex", "accounts"))
socket_info = os.stat(local_data_socket, follow_symlinks=False)
parent_info = os.stat(os.path.dirname(local_data_socket), follow_symlinks=False)
if (
    not isinstance(binding, dict)
    or set(binding) != {"schema", "accounts_dir", "local_data_socket", "local_data_socket_identity"}
    or binding.get("schema") != "subrouter.local-serving-store/v2"
    or binding.get("accounts_dir") != expected_accounts
    or binding.get("local_data_socket") != local_data_socket
    or not stat.S_ISSOCK(socket_info.st_mode)
    or socket_info.st_uid != os.getuid()
    or stat.S_IMODE(socket_info.st_mode) != 0o600
    or not stat.S_ISDIR(parent_info.st_mode)
    or parent_info.st_uid != os.getuid()
    or parent_info.st_mode & 0o022
    or binding.get("local_data_socket_identity") != f"unix:{socket_info.st_dev}:{socket_info.st_ino}"
):
    raise SystemExit("published serving-store binding does not select the candidate state")
PY
}

stage_candidate_serving_store_binding() {
  local destination="$1" state_dir="$2" local_data_socket="$3"
  python3 - "$destination" "$state_dir" "$local_data_socket" <<'PY'
import json
import os
import stat
import sys

destination, state_dir, local_data_socket = sys.argv[1:]
socket_stat = os.stat(local_data_socket, follow_symlinks=False)
parent_stat = os.stat(os.path.dirname(local_data_socket), follow_symlinks=False)
if (
    not stat.S_ISSOCK(socket_stat.st_mode)
    or socket_stat.st_uid != os.getuid()
    or stat.S_IMODE(socket_stat.st_mode) != 0o600
    or not stat.S_ISDIR(parent_stat.st_mode)
    or parent_stat.st_uid != os.getuid()
    or parent_stat.st_mode & 0o022
):
    raise SystemExit("candidate local data socket is not private and current-user-owned")
accounts_dir = os.path.realpath(os.path.join(state_dir, "codex", "accounts"))

def go_json(value):
    return (
        json.dumps(value, ensure_ascii=False, separators=(",", ":"))
        .replace("&", "\\u0026")
        .replace("<", "\\u003c")
        .replace(">", "\\u003e")
        .replace("\u2028", "\\u2028")
        .replace("\u2029", "\\u2029")
    )

payload = (
    '{"schema":"subrouter.local-serving-store/v2","accounts_dir":'
    + go_json(accounts_dir)
    + ',"local_data_socket":'
    + go_json(local_data_socket)
    + ',"local_data_socket_identity":'
    + go_json(f"unix:{socket_stat.st_dev}:{socket_stat.st_ino}")
    + "}\n"
).encode("utf-8")
descriptor = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
try:
    os.write(descriptor, payload)
    os.fsync(descriptor)
finally:
    os.close(descriptor)
PY
}

require_sole_unix_listener_owner() {
  local socket="$1" candidate_pid="$2" pid found=0
  while IFS= read -r pid; do
    case "$pid" in ''|*[!0-9]*) continue ;; esac
    found=1
	[ "$pid" = "$candidate_pid" ] || {
      echo "Unix listener $socket is owned by unexpected pid $pid" >&2
      return 1
    }
  done < <(lsof -nP -t -- "$socket" 2>/dev/null | sort -u)
  [ "$found" -eq 1 ] || { echo "no listener owner found for Unix socket $socket" >&2; return 1; }
}
active_worker_fingerprint() {
  local socket="$1" expected_cdhash="$2" status
  status="$(curl -fsS --max-time 2 --unix-socket "$socket" http://localhost/_subrouter/supervisor-status)" || return 1
  STATUS_JSON="$status" python3 - "$expected_cdhash" <<'PY'
import json
import os
import sys

expected = sys.argv[1]
document = json.loads(os.environ["STATUS_JSON"])
active = document.get("active") or {}
worker = document.get("active_worker") or {}
valid = (
    worker.get("id") == active.get("id")
    and isinstance(worker.get("pid"), int)
    and worker["pid"] > 1
    and isinstance(worker.get("process_start_identity"), str)
    and bool(worker["process_start_identity"])
    and worker.get("identity_kind") == "darwin-cdhash-sha256"
    and worker.get("executable_identity") == expected
)
if not valid:
    raise SystemExit("active worker process identity did not match the candidate")
print(json.dumps({
    "id": worker["id"],
    "pid": worker["pid"],
    "process_start_identity": worker["process_start_identity"],
    "identity_kind": worker["identity_kind"],
    "executable_identity": worker["executable_identity"],
}, sort_keys=True, separators=(",", ":")))
PY
}
supervisor_acceptance_fingerprint() {
  local socket="$1" expected_uid="$2" expected_cdhash="$3" status
  [ -e "$socket" ] || return 2
  [ "$(stat -f '%HT' "$socket" 2>/dev/null)" = "Socket" ] || return 1
  [ "$(stat -f '%u' "$socket" 2>/dev/null)" = "$expected_uid" ] || return 1
  status="$(curl -fsS --max-time 2 --unix-socket "$socket" http://localhost/_subrouter/supervisor-status)" || return 2
  [ "$(stat -f '%Lp' "$socket" 2>/dev/null)" = "600" ] || return 1
  STATUS_JSON="$status" python3 - "$expected_cdhash" <<'PY'
import json
import os
import sys

expected = sys.argv[1]
document = json.loads(os.environ["STATUS_JSON"])
active = document.get("active") or {}
worker = document.get("active_worker") or {}
valid = (
    document.get("accepting") is True
    and document.get("retiring") is False
    and len(document.get("backends") or []) == 1
    and worker.get("id") == active.get("id")
    and isinstance(worker.get("pid"), int)
    and worker["pid"] > 1
    and isinstance(worker.get("process_start_identity"), str)
    and bool(worker["process_start_identity"])
    and worker.get("identity_kind") == "darwin-cdhash-sha256"
    and worker.get("executable_identity") == expected
)
if not valid:
    raise SystemExit("supervisor lifecycle or active worker identity did not match the candidate")
print(json.dumps({
    "id": worker["id"],
    "pid": worker["pid"],
    "process_start_identity": worker["process_start_identity"],
    "identity_kind": worker["identity_kind"],
    "executable_identity": worker["executable_identity"],
}, sort_keys=True, separators=(",", ":")))
PY
}
wait_for_supervisor_readiness() {
  local socket="$1" expected_uid="$2" expected_cdhash="$3"
  local attempts="${SUBROUTER_HEALTH_ATTEMPTS:-60}"
  local interval="${SUBROUTER_HEALTH_INTERVAL:-1}"
  local fingerprint status i=0
  while [ "$i" -lt "$attempts" ]; do
    if fingerprint="$(supervisor_acceptance_fingerprint "$socket" "$expected_uid" "$expected_cdhash")"; then
      printf '%s\n' "$fingerprint"
      return 0
    else
      status=$?
      [ "$status" -eq 2 ] || return "$status"
    fi
    i=$((i + 1))
    sleep "$interval"
  done
  return 1
}
wait_for_candidate_process_fingerprint() {
  local service="$1" snapshot="$2" expected_program="$3"
  local attempts="${SUBROUTER_HEALTH_ATTEMPTS:-60}"
  local interval="${SUBROUTER_HEALTH_INTERVAL:-1}"
  local program pid fingerprint i=0
  while [ "$i" -lt "$attempts" ]; do
    if capture_launchctl_snapshot "$service" "$snapshot"; then
      program="$(launchctl_snapshot_field "$snapshot" program)"
      [ -z "$program" ] || [ "$program" = "$expected_program" ] || return 1
      pid="$(launchctl_snapshot_field "$snapshot" pid)"
      case "$pid" in
        ''|*[!0-9]*) ;;
        *)
          fingerprint="$(process_fingerprint "$pid" "$expected_program" || true)"
          if [ -n "$fingerprint" ]; then
            printf '%s\n' "$fingerprint"
            return 0
          fi
          ;;
      esac
    fi
    i=$((i + 1))
    sleep "$interval"
  done
  return 1
}
run_verified_recovery() {
  local recovery recovery_sha_file recovery_sha
  for recovery in "$@"; do
    [ -x "$recovery" ] || continue
    recovery_sha_file="${recovery}.sha256"
    [ -f "$recovery_sha_file" ] && [ ! -L "$recovery_sha_file" ] || continue
    recovery_sha="$(cat "$recovery_sha_file")"
    if verify_file_sha256 "$recovery" "$recovery_sha" && "$recovery"; then
      return 0
    fi
  done
  return 1
}
recovery_generation_suffix() {
  local marker="$TRANSACTION_DIR/recovery-generation" generation
  if [ ! -e "$marker" ]; then
    # Journals created before the marker is durably published can only have the
    # initial generation. This also keeps interrupted older deployments
    # recoverable after upgrading the migration script.
    printf '%s' ""
    return 0
  fi
  [ -f "$marker" ] && [ ! -L "$marker" ] \
    || return 1
  generation="$(cat "$marker")"
  case "$generation" in
    initial) printf '%s' "" ;;
    candidate) printf '%s' "-candidate" ;;
    *) return 1 ;;
  esac
}
activate=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --activate) activate=1; shift ;;
    --preflight-callback)
      [ "$#" -ge 2 ] || die "--preflight-callback requires an executable path"
      PREFLIGHT_CALLBACK="$2"; shift 2 ;;
    --canary-callback)
      [ "$#" -ge 2 ] || die "--canary-callback requires an executable path"
      CANARY_CALLBACK="$2"; shift 2 ;;
    --retiring-state-dir)
      [ "$#" -ge 2 ] || die "--retiring-state-dir requires a path"
      RETIRING_STATE_DIR="$2"; shift 2 ;;
    --public-addr)
      [ "$#" -ge 2 ] || die "--public-addr requires HOST:PORT"
      PUBLIC_ADDR_OVERRIDE="$2"; shift 2 ;;
    --worker-serve-args-json)
      [ "$#" -ge 2 ] || die "--worker-serve-args-json requires a file path"
      WORKER_SERVE_ARGS_JSON="$2"; shift 2 ;;
    --candidate-env-json)
      [ "$#" -ge 2 ] || die "--candidate-env-json requires a file path"
      CANDIDATE_ENV_JSON="$2"; shift 2 ;;
    -h|--help)
      echo "usage: $0 [--activate] [--preflight-callback PATH] [--canary-callback PATH] [--retiring-state-dir PATH] [--public-addr HOST:PORT --worker-serve-args-json FILE] [--candidate-env-json FILE]"
      exit 0 ;;
    *) die "unknown argument $1" ;;
  esac
done

if [ -n "$PUBLIC_ADDR_OVERRIDE" ] || [ -n "$WORKER_SERVE_ARGS_JSON" ]; then
  [ -n "$PUBLIC_ADDR_OVERRIDE" ] && [ -n "$WORKER_SERVE_ARGS_JSON" ] \
    || die "--public-addr and --worker-serve-args-json must be provided together"
fi
for json_input in "$WORKER_SERVE_ARGS_JSON" "$CANDIDATE_ENV_JSON"; do
  [ -z "$json_input" ] || { [ -f "$json_input" ] && [ ! -L "$json_input" ]; } \
    || die "JSON input $json_input must be a regular non-symlink file"
done

export SUBROUTER_TRANSITION_NAME="migrate-launchagent-to-supervisor"
export SUBROUTER_STATE_DIR="$STATE_DIR"

if [ "$activate" -eq 1 ]; then
  positive_integer "$PREFLIGHT_TIMEOUT" \
    || die "preflight timeout must be a positive integer"
  positive_integer "$CANARY_TIMEOUT" \
    || die "functional canary timeout must be a positive integer"
fi

TRANSACTION_DIR="${PLIST}.supervisor-transaction"
UPGRADE_INHIBIT_FILE="${TRANSACTION_DIR}/upgrade-inhibited"
if [ "$activate" -eq 1 ]; then
  if ! mkdir -m 0700 "$TRANSACTION_DIR" 2>/dev/null; then
    interrupted_canary_state_dir="$TRANSACTION_DIR/functional-canary-process-group"
    if [ -d "$interrupted_canary_state_dir" ]; then
      drain_bounded_process_group "interrupted functional canary" \
        "$interrupted_canary_state_dir/process-group" \
        || die "interrupted functional canary termination is unverified; rollback withheld"
      rm -f "$interrupted_canary_state_dir/process-group"
      rmdir "$interrupted_canary_state_dir"
    fi
	phase="$(cat "$TRANSACTION_DIR/phase" 2>/dev/null || true)"
	recovery_suffix="$(recovery_generation_suffix)" \
	  || die "interrupted transaction has an invalid recovery generation"
	case "$phase" in
      ''|prelive)
        rm -rf "$TRANSACTION_DIR"
        die "cleared an incomplete pre-live transaction; rerun activation"
        ;;
	  candidate_plist_installing)
		recovery_candidates=(
		  "$TRANSACTION_DIR/recover-legacy-unchanged${recovery_suffix}"
		  "$TRANSACTION_DIR/recover-legacy-running${recovery_suffix}"
		)
		;;
	  candidate_bootstrap_requested)
		recovery_candidates=(
		  "$TRANSACTION_DIR/recover-legacy-running${recovery_suffix}"
		  "$TRANSACTION_DIR/recover-candidate-running${recovery_suffix}"
		)
		;;
	  candidate_plist_installed|legacy_bootout_requested|bootout_requested|legacy_absent)
		recovery_candidates=("$TRANSACTION_DIR/recover-legacy-running${recovery_suffix}")
		;;
	  rollback_legacy_bootstrap_requested|rollback_legacy_accepted|rollback_binding_restored)
		recovery_candidates=("$TRANSACTION_DIR/recover-rollback-legacy${recovery_suffix}")
		;;
	  *) recovery_candidates=("$TRANSACTION_DIR/recover-candidate-running${recovery_suffix}") ;;
	esac
    run_verified_recovery "${recovery_candidates[@]}" \
      || die "reentry recovery failed for transaction phase $phase"
    rm -rf "$TRANSACTION_DIR"
    die "recovered interrupted transaction phase $phase to legacy; rerun activation"
  fi
  printf 'prelive\n' >"$TRANSACTION_DIR/phase"
  printf 'subrouter supervisor activation in progress\n' >"$UPGRADE_INHIBIT_FILE"
  chmod 0600 "$UPGRADE_INHIBIT_FILE"
  transaction_active=1
  prelive_transaction_exit() {
    local status=$?
    release_subrouter_mutation_lease
    trap - EXIT INT TERM
    [ -d "$TRANSACTION_DIR" ] && rm -rf "$TRANSACTION_DIR"
    [ "$status" -ne 0 ] || status=1
    exit "$status"
  }
  trap prelive_transaction_exit EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
fi

[ -f "$PLIST" ] || die "$PLIST not found"
[ -x "$WORKER_BIN" ] || die "$WORKER_BIN is not executable"
"$WORKER_BIN" help 2>/dev/null | grep -q ' supervise ' \
  || die "$WORKER_BIN does not support supervise; upgrade it first"

# A separate copy, because routine upgrades replace the worker and must never
# replace the supervisor that is holding the listener.
mkdir -p "$(dirname "$SUPERVISOR_BIN")"
cp -f "$WORKER_BIN" "${SUPERVISOR_BIN}.new"
chmod 0755 "${SUPERVISOR_BIN}.new"
mv -f "${SUPERVISOR_BIN}.new" "$SUPERVISOR_BIN"
# An ad-hoc signature keeps macOS from killing the copy with OS_REASON_CODESIGNING.
codesign -s - -f "$SUPERVISOR_BIN" >/dev/null 2>&1 || true

prepared="${PLIST}.supervised"
python3 - "$PLIST" "$prepared" "$SUPERVISOR_BIN" "$WORKER_BIN" "$CONTROL_SOCKET" "$LOCAL_DATA_SOCKET" "$STATE_DIR" \
  "$PUBLIC_ADDR_OVERRIDE" "$WORKER_SERVE_ARGS_JSON" "$CANDIDATE_ENV_JSON" "$UPGRADE_INHIBIT_FILE" <<'PY'
import json
import os
import plistlib
import re
import stat
import sys

(
    source,
    destination,
    supervisor_bin,
    worker_bin,
    control_socket,
    local_data_socket,
    state_dir,
    public_addr_override,
    worker_args_path,
    candidate_env_path,
    upgrade_inhibit_file,
) = sys.argv[1:12]


def load_json_file(path):
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(path, flags)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            raise SystemExit(f"JSON input {path} is not a regular file")
        with os.fdopen(fd, "r", encoding="utf-8", closefd=False) as stream:
            return json.load(stream)
    except (OSError, UnicodeError, json.JSONDecodeError) as error:
        raise SystemExit(f"invalid JSON input {path}: {error}") from error
    finally:
        os.close(fd)


def validate_public_addr(value):
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


with open(source, "rb") as handle:
    plist = plistlib.load(handle)

arguments = list(plist.get("ProgramArguments") or [])
if not arguments:
    raise SystemExit("existing plist has no ProgramArguments")

# The supervisor owns the public address and supplies `serve` plus the worker's
# private socket itself, so strip both from the inherited arguments.
if worker_args_path:
    filtered = load_json_file(worker_args_path)
    if not isinstance(filtered, list) or not all(isinstance(item, str) for item in filtered):
        raise SystemExit("worker serve args JSON must be an array of strings")
    if any("\x00" in item for item in filtered):
        raise SystemExit("worker serve args must not contain NUL bytes")
    for argument in filtered:
        if argument == "serve":
            raise SystemExit("worker serve args must not embed the serve subcommand")
        if argument == "--addr" or argument.startswith("--addr="):
            raise SystemExit("worker serve args must not embed --addr")
    public_addr = public_addr_override
else:
    public_addr = None
    filtered = []
    filtered_source = arguments[1:]
    if filtered_source and filtered_source[0] == "serve":
        filtered_source = filtered_source[1:]
    i = 0
    while i < len(filtered_source):
        argument = filtered_source[i]
        if argument == "--addr":
            if i + 1 >= len(filtered_source):
                raise SystemExit("existing --addr has no value")
            public_addr = filtered_source[i + 1]
            i += 2
            continue
        if argument.startswith("--addr="):
            public_addr = argument.split("=", 1)[1]
            i += 1
            continue
        filtered.append(argument)
        i += 1
    if not public_addr:
        raise SystemExit(
            "could not find --addr in the existing plist; wrapper-backed services require "
            "--public-addr and --worker-serve-args-json"
        )

validate_public_addr(public_addr)

plist["Program"] = supervisor_bin
plist["ProgramArguments"] = [
    supervisor_bin,
    "supervise",
    "--addr", public_addr,
    "--control-socket", control_socket,
    "--local-data-socket", local_data_socket,
    "--worker-bin", worker_bin,
    "--upgrade-inhibit-file", upgrade_inhibit_file,
    "--",
    *filtered,
]
# Pin the candidate to the state root that passed the isolation preflight.
# launchd does not inherit the activating shell's environment, and a legacy
# rollback may intentionally continue using a separate untouched store.
environment = dict(plist.get("EnvironmentVariables") or {})
if candidate_env_path:
    overrides = load_json_file(candidate_env_path)
    if not isinstance(overrides, dict) or not all(
        isinstance(key, str) and isinstance(value, str)
        for key, value in overrides.items()
    ):
        raise SystemExit("candidate environment JSON must be an object of string values")
    for key, value in overrides.items():
        if not re.fullmatch(r"SUBROUTER_[A-Z0-9_]+_FILE", key):
            raise SystemExit(
                "candidate environment keys must match SUBROUTER_*_FILE; "
                "raw secrets and non-file overrides are forbidden"
            )
        if "\x00" in value or not os.path.isabs(value):
            raise SystemExit(f"candidate environment file for {key} must be an absolute path")
        flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
        try:
            file_fd = os.open(value, flags)
        except OSError as error:
            raise SystemExit(f"candidate environment file for {key} is not safely openable: {error}") from error
        try:
            info = os.fstat(file_fd)
            if not stat.S_ISREG(info.st_mode):
                raise SystemExit(f"candidate environment file for {key} is not regular")
            if info.st_uid != os.getuid():
                raise SystemExit(f"candidate environment file for {key} is not owned by the current uid")
            if stat.S_IMODE(info.st_mode) & 0o077:
                raise SystemExit(f"candidate environment file for {key} has group or other permissions")
        finally:
            os.close(file_fd)
    environment.update(overrides)
environment["SUBROUTER_STATE_DIR"] = state_dir
plist["EnvironmentVariables"] = environment
# The supervisor must outlive its draining workers.
plist["ExitTimeOut"] = 600
plist["ThrottleInterval"] = 10

with open(destination, "wb") as handle:
    plistlib.dump(plist, handle, fmt=plistlib.FMT_XML, sort_keys=False)
print(public_addr)
PY

public_addr="$(python3 - "$prepared" <<'PY'
import plistlib, sys
with open(sys.argv[1], "rb") as handle:
    plist = plistlib.load(handle)
arguments = plist["ProgramArguments"]
print(arguments[arguments.index("--addr") + 1])
PY
)"
health_url="http://${public_addr}/_subrouter/health"
case "$public_addr" in
  0.0.0.0:*) health_url="http://127.0.0.1:${public_addr##*:}/_subrouter/health" ;;
  \[::\]:*) health_url="http://127.0.0.1:${public_addr#\[::\]:}/_subrouter/health" ;;
esac

echo "prepared $prepared"
if [ "$activate" -eq 0 ]; then
  cat <<EOF

Review the plist above, then activate with:
  $0 --activate --canary-callback /path/to/real-routed-canary

Activation restarts the agent once, which drops connections currently in
flight. Do it when no coding agent is mid-turn. Every upgrade after it
preserves connections.
EOF
  exit 0
fi

[ -n "$CANARY_CALLBACK" ] \
  || die "activation requires --canary-callback (or SUBROUTER_CANARY_CALLBACK)"
[ -x "$CANARY_CALLBACK" ] || die "canary callback $CANARY_CALLBACK is not executable"
if [ -n "$PREFLIGHT_CALLBACK" ]; then
  [ -x "$PREFLIGHT_CALLBACK" ] \
    || die "preflight callback $PREFLIGHT_CALLBACK is not executable"
fi

if [ -z "$RETIRING_STATE_DIR" ]; then
  RETIRING_STATE_DIR="$(plist_value "$PLIST" EnvironmentVariables:SUBROUTER_STATE_DIR || true)"
fi
[ -n "$RETIRING_STATE_DIR" ] \
  || die "retiring plist has no explicit SUBROUTER_STATE_DIR; pass --retiring-state-dir"
[ "$RETIRING_STATE_DIR" != "$STATE_DIR" ] \
  || die "candidate and retiring state roots must be different"

echo "running bounded preflight"
run_bounded_argv "credential isolation preflight" "$PREFLIGHT_TIMEOUT" \
  "$WORKER_BIN" codex isolation-check --json \
    --retiring-state-dir "$RETIRING_STATE_DIR" \
  || die "Codex isolation preflight failed"
if [ -n "$PREFLIGHT_CALLBACK" ]; then
  run_bounded_argv "deployment preflight" "$PREFLIGHT_TIMEOUT" "$PREFLIGHT_CALLBACK" \
    || die "preflight callback failed"
fi

bundle="${PLIST}.rollback-bundle-$(date +%Y%m%d-%H%M%S)-$$"
mkdir -m 0700 "$bundle"
backup="$bundle/legacy.plist"
copy_file_nofollow "$PLIST" "$backup" 0600
old_program="$(plist_program "$backup")"
require_plist_identity "$PLIST" "$LABEL" "$old_program"
backup_sha256="$(sha256_file "$backup")"
rollback_identity_args=()
identity_manifest="${backup}.identity"
: >"$identity_manifest"
chmod 0600 "$identity_manifest"
printf '%s  %s\n' "$backup_sha256" "$backup" >>"$identity_manifest"
serving_store_rollback_args=(--serving-store-binding "$SERVING_STORE_BINDING")
serving_store_binding_backup=""
serving_store_binding_backup_sha=""
serving_store_binding_mode=""
serving_store_bind_cas_args=()
if [ -e "$SERVING_STORE_BINDING" ] || [ -L "$SERVING_STORE_BINDING" ]; then
  serving_store_binding_mode="$(private_serving_store_binding_mode "$SERVING_STORE_BINDING")" \
    || die "existing serving-store binding is not safe to preserve"
  serving_store_binding_backup="$bundle/local-serving-store.before.json"
  copy_file_nofollow \
    "$SERVING_STORE_BINDING" "$serving_store_binding_backup" "$serving_store_binding_mode" \
    || die "could not preserve the existing serving-store binding"
  serving_store_binding_backup_sha="$(sha256_file "$serving_store_binding_backup")"
  fsync_parent_directory "$serving_store_binding_backup" \
    || die "could not durably preserve the existing serving-store binding"
  serving_store_rollback_args+=(
    --serving-store-binding-backup
    "$serving_store_binding_backup"
    "$serving_store_binding_backup_sha"
    "$serving_store_binding_mode"
  )
  serving_store_bind_cas_args+=(
    --if-current-sha256 "$serving_store_binding_backup_sha"
    --if-current-mode "$serving_store_binding_mode"
  )
  printf 'serving-store-binding-before  %s  %s  %s  %s\n' \
    "$serving_store_binding_backup_sha" "$serving_store_binding_mode" \
    "$SERVING_STORE_BINDING" "$serving_store_binding_backup" >>"$identity_manifest"
else
  serving_store_rollback_args+=(--serving-store-binding-absent)
  serving_store_bind_cas_args+=(--if-current-absent)
  printf 'serving-store-binding-before  absent  %s\n' \
    "$SERVING_STORE_BINDING" >>"$identity_manifest"
fi
candidate_serving_store_binding_artifact="$bundle/local-serving-store.candidate.json"
# The socket inode does not exist until the candidate supervisor is live. Use
# an impossible placeholder for pre-publication rollback; it still accepts the
# preserved prior binding (or prior absence), then is replaced with the exact
# candidate digest before bind-state can publish anything.
candidate_serving_store_binding_sha="0000000000000000000000000000000000000000000000000000000000000000"
serving_store_rollback_args+=(
  --expected-serving-store-binding-sha256
  "$candidate_serving_store_binding_sha"
)
while IFS= read -r dependency; do
  [ -n "$dependency" ] || continue
  dependency_sha="$(sha256_file "$dependency")"
  dependency_mode="$(stat -f '%Lp' "$dependency")"
  dependency_artifact="$bundle/${dependency_sha}-$(basename "$dependency")"
  copy_file_nofollow "$dependency" "$dependency_artifact" "$dependency_mode"
  rollback_identity_args+=(--rollback-artifact "$dependency" "$dependency_artifact" "$dependency_sha" "$dependency_mode")
  printf '%s  %s  %s\n' "$dependency_sha" "$dependency" "$dependency_artifact" >>"$identity_manifest"
done < <(plist_executable_dependencies "$backup")
[ "${#rollback_identity_args[@]}" -gt 0 ] \
  || die "could not discover rollback program identity"
service="$DOMAIN/$LABEL"
legacy_snapshot="$TRANSACTION_DIR/legacy.launchctl"
capture_launchctl_snapshot "$service" "$legacy_snapshot" || die "$service is not loaded"
captured_program="$(launchctl_snapshot_field "$legacy_snapshot" program)"
captured_pid="$(launchctl_snapshot_field "$legacy_snapshot" pid)"
[ "$captured_program" = "$old_program" ] || die "$service legacy program identity mismatch"
case "$captured_pid" in ''|*[!0-9]*) die "$service has no stable legacy pid" ;; esac
legacy_fingerprint="$(process_fingerprint "$captured_pid" "$old_program")" \
  || die "could not capture stable legacy process identity"
candidate_plist_sha="$(sha256_file "$prepared")"
candidate_supervisor_sha="$(sha256_file "$SUPERVISOR_BIN")"
candidate_worker_sha="$(sha256_file "$WORKER_BIN")"
candidate_worker_cdhash="$(codesign -d --verbose=4 "$WORKER_BIN" 2>&1 | sed -n 's/^CDHash=\([0-9A-Fa-f][0-9A-Fa-f]*\)$/\1/p' | tr '[:upper:]' '[:lower:]' | head -n 1)"
case "$candidate_worker_cdhash" in
  ????????????????????????????????????????) ;;
  *) die "could not capture candidate worker code-directory hash" ;;
esac
case "$candidate_worker_cdhash" in *[!0-9a-f]*) die "candidate worker code-directory hash is invalid" ;; esac
{
  printf '%s  %s\n%s  %s\n%s  %s\n' \
    "$candidate_plist_sha" "$prepared" \
    "$candidate_supervisor_sha" "$SUPERVISOR_BIN" \
    "$candidate_worker_sha" "$WORKER_BIN"
  printf 'candidate-worker-cdhash  %s\n' "$candidate_worker_cdhash"
  printf 'legacy-process  %s\n' "$legacy_fingerprint"
} >>"$identity_manifest"

rollback() {
  local expected_running="${1:-$SUPERVISOR_BIN}"
  echo "rolling back through the standalone identity-checked command" >&2
  if SUBROUTER_LABEL="$LABEL" \
  SUBROUTER_PLIST="$PLIST" \
  SUBROUTER_LAUNCHD_DOMAIN="$DOMAIN" \
  SUBROUTER_CONTROL_SOCKET="$CONTROL_SOCKET" \
  SUBROUTER_TRANSACTION_DIR="$TRANSACTION_DIR" \
    "$SCRIPT_DIR/rollback-launchagent-supervisor.sh" \
      --backup "$backup" \
      --backup-sha256 "$backup_sha256" \
      "${rollback_identity_args[@]}" \
      "${serving_store_rollback_args[@]}" \
      --public-addr "$public_addr" \
      --expected-program "$SUPERVISOR_BIN" \
      --expected-running-program "$expected_running"; then
    transaction_active=0
    release_subrouter_mutation_lease
    trap - EXIT INT TERM
    rm -rf "$TRANSACTION_DIR"
    return 0
  fi
  return 1
}

write_recovery_script() {
  local destination="$1" expected_running="$2" expected_installed="${3:-$SUPERVISOR_BIN}"
  {
		printf '#!/usr/bin/env bash\nset -euo pipefail\n'
		printf 'unset SUBROUTER_ROLLBACK_FAULT_INJECT_HARD_PHASE\n'
		printf 'unset SUBROUTER_ROLLBACK_FAULT_INJECT_HARD_OWNER_PHASE\n'
    printf 'export SUBROUTER_LABEL=%q SUBROUTER_PLIST=%q SUBROUTER_LAUNCHD_DOMAIN=%q SUBROUTER_CONTROL_SOCKET=%q SUBROUTER_TRANSACTION_DIR=%q\n' \
      "$LABEL" "$PLIST" "$DOMAIN" "$CONTROL_SOCKET" "$TRANSACTION_DIR"
    printf 'exec %q' "$SCRIPT_DIR/rollback-launchagent-supervisor.sh"
	printf ' %q' --backup "$backup" --backup-sha256 "$backup_sha256" \
	  "${rollback_identity_args[@]}" "${serving_store_rollback_args[@]}"
	printf ' %q' \
      --public-addr "$public_addr" \
      --expected-program "$expected_installed" \
      --expected-running-program "$expected_running"
    printf '\n'
  } >"$destination"
  chmod 0700 "$destination"
}
write_recovery_generation() {
  local suffix="$1" recovery
  write_recovery_script "$TRANSACTION_DIR/recover-legacy-running${suffix}" "$old_program"
  write_recovery_script "$TRANSACTION_DIR/recover-candidate-running${suffix}" "$SUPERVISOR_BIN"
  write_recovery_script "$TRANSACTION_DIR/recover-legacy-unchanged${suffix}" "$old_program" "$old_program"
  write_recovery_script "$TRANSACTION_DIR/recover-rollback-legacy${suffix}" "$old_program" "$old_program"
  for recovery in "$TRANSACTION_DIR/recover-legacy-running${suffix}" "$TRANSACTION_DIR/recover-candidate-running${suffix}" "$TRANSACTION_DIR/recover-legacy-unchanged${suffix}" "$TRANSACTION_DIR/recover-rollback-legacy${suffix}"; do
    sha256_file "$recovery" >"${recovery}.sha256"
    chmod 0600 "${recovery}.sha256"
	python3 - "$recovery" "${recovery}.sha256" <<'PY'
import os
import sys
for path in sys.argv[1:]:
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
PY
  done
	fsync_parent_directory "$TRANSACTION_DIR/recover-legacy-running${suffix}" \
    || die "could not durably publish recovery generation ${suffix:-initial}"
}
set_recovery_generation() {
  local generation="$1"
  case "$generation" in initial|candidate) ;; *) return 1 ;; esac
  python3 - "$TRANSACTION_DIR" "$generation" <<'PY'
import os
import sys

directory, generation = sys.argv[1:]
next_path = os.path.join(directory, "recovery-generation.next")
marker_path = os.path.join(directory, "recovery-generation")
fd = os.open(next_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
try:
    os.write(fd, (generation + "\n").encode())
    os.fsync(fd)
finally:
    os.close(fd)
os.replace(next_path, marker_path)
directory_fd = os.open(directory, os.O_RDONLY)
try:
    os.fsync(directory_fd)
finally:
    os.close(directory_fd)
PY
}
write_recovery_generation ""
set_recovery_generation initial \
  || die "could not durably select the initial recovery generation"

recover_transaction_on_exit() {
  local status=$? phase recovered=0 recovery_suffix=""
  trap - EXIT INT TERM
  if [ "${transaction_active:-0}" -eq 1 ]; then
    phase="$(cat "$TRANSACTION_DIR/phase" 2>/dev/null || true)"
	recovery_suffix="$(recovery_generation_suffix)" || recovery_suffix="__invalid__"
    case "$phase" in
      prelive)
        rm -rf "$TRANSACTION_DIR"
        recovered=1
        ;;
      candidate_plist_installing)
        if run_verified_recovery \
          "$TRANSACTION_DIR/recover-legacy-unchanged${recovery_suffix}" \
          "$TRANSACTION_DIR/recover-legacy-running${recovery_suffix}"; then
          recovered=1
        else
          echo "CRITICAL: automatic legacy recovery failed at phase $phase" >&2
        fi
        ;;
      candidate_bootstrap_requested)
        if run_verified_recovery \
          "$TRANSACTION_DIR/recover-legacy-running${recovery_suffix}" \
          "$TRANSACTION_DIR/recover-candidate-running${recovery_suffix}"; then
          recovered=1
        else
          echo "CRITICAL: automatic legacy recovery failed at phase $phase" >&2
        fi
        ;;
      candidate_plist_installed|legacy_bootout_requested|bootout_requested|legacy_absent)
        if run_verified_recovery "$TRANSACTION_DIR/recover-legacy-running${recovery_suffix}"; then
          recovered=1
        else
          echo "CRITICAL: automatic legacy recovery failed at phase $phase" >&2
        fi
        ;;
      rollback_legacy_bootstrap_requested|rollback_legacy_accepted|rollback_binding_restored)
        if run_verified_recovery "$TRANSACTION_DIR/recover-rollback-legacy${recovery_suffix}"; then
          recovered=1
        else
          echo "CRITICAL: automatic rollback continuation failed at phase $phase" >&2
        fi
        ;;
      *)
        if run_verified_recovery "$TRANSACTION_DIR/recover-candidate-running${recovery_suffix}"; then
          recovered=1
        else
          echo "CRITICAL: automatic legacy recovery failed at phase $phase" >&2
        fi
        ;;
    esac
    [ "$recovered" -eq 0 ] || rm -rf "$TRANSACTION_DIR"
  fi
  release_subrouter_mutation_lease
  [ "$status" -ne 0 ] || status=1
  exit "$status"
}
trap recover_transaction_on_exit EXIT
transition_signal_exit() {
  local status="$1" signal_name="$2"
  trap - INT TERM
  if [ -n "${RUN_BOUNDED_GROUP_STATE_FILE:-}" ] \
    && [ -f "$RUN_BOUNDED_GROUP_STATE_FILE" ]; then
    if ! drain_bounded_process_group "interrupted functional canary" \
      "$RUN_BOUNDED_GROUP_STATE_FILE"; then
      release_subrouter_mutation_lease
      trap - EXIT
      transaction_active=0
      die "$signal_name interrupted functional canary termination is unverified; rollback withheld and transaction journal retained (group state: $RUN_BOUNDED_GROUP_STATE_FILE)"
    fi
    RUN_BOUNDED_CLEANUP_CONFIRMED=1
  fi
  exit "$status"
}
trap 'transition_signal_exit 130 SIGINT' INT
trap 'transition_signal_exit 143 SIGTERM' TERM

set_phase() {
  python3 - "$TRANSACTION_DIR" "$1" <<'PY'
import os
import sys

directory, phase = sys.argv[1:]
next_path = os.path.join(directory, "phase.next")
phase_path = os.path.join(directory, "phase")
fd = os.open(next_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
try:
    os.write(fd, (phase + "\n").encode())
    os.fsync(fd)
finally:
    os.close(fd)
os.replace(next_path, phase_path)
directory_fd = os.open(directory, os.O_RDONLY)
try:
    os.fsync(directory_fd)
finally:
    os.close(directory_fd)
PY
  if [ "${SUBROUTER_FAULT_INJECT_PHASE:-}" = "$1" ]; then
    kill -TERM $$
  fi
  if [ "${SUBROUTER_FAULT_INJECT_HARD_PHASE:-}" = "$1" ]; then
    kill -KILL $$
  fi
}

inject_hard_fault_after_mutation() {
  if [ "${SUBROUTER_FAULT_INJECT_HARD_AFTER_MUTATION:-}" = "$1" ]; then
    kill -KILL $$
  fi
}

verify_file_sha256 "$prepared" "$candidate_plist_sha" || die "candidate plist changed before installation"
verify_file_sha256 "$SUPERVISOR_BIN" "$candidate_supervisor_sha" || die "candidate supervisor changed before installation"
verify_file_sha256 "$WORKER_BIN" "$candidate_worker_sha" || die "candidate worker changed before installation"
set_phase candidate_plist_installing
atomic_restore_nofollow "$prepared" "$PLIST" "$candidate_plist_sha" 0644 \
  || die "candidate plist installation failed"
inject_hard_fault_after_mutation candidate_plist_restore
set_phase candidate_plist_installed
set_phase legacy_bootout_requested
if ! launchctl bootout "$service"; then
  rollback "$old_program"
  die "could not request removal of $service; legacy LaunchAgent restored"
fi
set_phase bootout_requested
if ! wait_for_full_absence "$service" "$captured_pid" "$public_addr"; then
  rollback "$old_program"
  die "$service did not fully disappear; legacy LaunchAgent restored"
fi
set_phase legacy_absent
set_phase candidate_bootstrap_requested
if ! bootstrap_with_retry "$DOMAIN" "$PLIST" "$service" "$public_addr"; then
  rollback
  die "supervised agent failed to bootstrap"
fi
inject_hard_fault_after_mutation candidate_bootstrap
set_phase candidate_bootstrapped

ready_url="${health_url%/_subrouter/health}/_subrouter/ready"
candidate_active_worker_fingerprint="$(wait_for_supervisor_readiness "$CONTROL_SOCKET" "$(id -u)" "$candidate_worker_cdhash" || true)"
candidate_snapshot="$TRANSACTION_DIR/candidate.launchctl"
candidate_fingerprint="$(wait_for_candidate_process_fingerprint "$service" "$candidate_snapshot" "$SUPERVISOR_BIN" || true)"
candidate_pid="${candidate_fingerprint%%|*}"
printf 'candidate-process  %s\n' "$candidate_fingerprint" >>"$identity_manifest"
structural_active_worker_fingerprint=""
structural_failure=""
if [ -z "$candidate_active_worker_fingerprint" ]; then
  structural_failure="supervisor readiness or active worker identity"
elif [ -z "$candidate_fingerprint" ]; then
  structural_failure="launchd supervisor process identity"
elif ! verify_file_sha256 "$PLIST" "$candidate_plist_sha"; then
  structural_failure="installed plist identity"
elif ! verify_file_sha256 "$SUPERVISOR_BIN" "$candidate_supervisor_sha"; then
  structural_failure="supervisor executable identity"
elif ! verify_file_sha256 "$WORKER_BIN" "$candidate_worker_sha"; then
  structural_failure="worker executable identity"
elif ! require_process_fingerprint "$candidate_fingerprint" "$SUPERVISOR_BIN"; then
  structural_failure="supervisor process continuity before listener acceptance"
elif ! require_sole_listener_owner "$public_addr" "$candidate_pid"; then
  structural_failure="listener ownership"
elif ! wait_for_http_acceptance "$health_url" "$ready_url"; then
  structural_failure="HTTP health and readiness"
elif ! require_process_fingerprint "$candidate_fingerprint" "$SUPERVISOR_BIN"; then
  structural_failure="supervisor process continuity after HTTP acceptance"
else
  structural_active_worker_fingerprint="$(supervisor_acceptance_fingerprint "$CONTROL_SOCKET" "$(id -u)" "$candidate_worker_cdhash" || true)"
  [ "$structural_active_worker_fingerprint" = "$candidate_active_worker_fingerprint" ] \
    || structural_failure="supervisor lifecycle or active worker continuity after HTTP acceptance"
fi
if [ -n "$structural_failure" ]; then
  echo "structural acceptance failed: $structural_failure" >&2
  rollback
  die "supervised agent failed structural acceptance"
fi
set_phase structural_accepted

# The supervisor now owns the stable socket, so its kernel identity can be
# frozen into both the v2 binding and the standalone rollback CAS. Do this
# before bind-state; no credential-bearing default-shell request can observe a
# v2 binding whose listener identity was only inferred before launch.
require_sole_unix_listener_owner "$LOCAL_DATA_SOCKET" "$candidate_pid" \
  || { rollback; die "candidate supervisor does not solely own the local data socket"; }
stage_candidate_serving_store_binding \
  "$candidate_serving_store_binding_artifact" "$STATE_DIR" "$LOCAL_DATA_SOCKET" \
  || { rollback; die "could not stage the live candidate serving-store binding"; }
verify_candidate_serving_store_binding \
  "$candidate_serving_store_binding_artifact" "$STATE_DIR" "$LOCAL_DATA_SOCKET" \
  || { rollback; die "could not verify the live candidate serving-store binding"; }
candidate_serving_store_binding_sha="$(sha256_file "$candidate_serving_store_binding_artifact")"
fsync_parent_directory "$candidate_serving_store_binding_artifact" \
  || { rollback; die "could not durably stage the candidate serving-store binding identity"; }
serving_store_rollback_args[${#serving_store_rollback_args[@]}-1]="$candidate_serving_store_binding_sha"
printf 'candidate-serving-store-binding  %s  %s  %s\n' \
  "$candidate_serving_store_binding_sha" "$SERVING_STORE_BINDING" \
  "$candidate_serving_store_binding_artifact" >>"$identity_manifest"
write_recovery_generation "-candidate"
set_recovery_generation candidate \
  || { rollback; die "could not durably select the candidate recovery generation"; }
set_phase recovery_candidate_committed

# Publish the default-shell store selection only after the exact candidate is
# structurally accepted. Intent is durable before the candidate performs its
# own attested atomic write, so a crash at any point is recoverable through the
# same standalone rollback command.
prior_serving_store_binding_matches=0
if [ -n "$serving_store_binding_backup" ]; then
  current_serving_store_binding_mode="$(private_serving_store_binding_mode "$SERVING_STORE_BINDING" 2>/dev/null || true)"
  if [ "$current_serving_store_binding_mode" = "$serving_store_binding_mode" ] \
    && verify_file_sha256 "$SERVING_STORE_BINDING" "$serving_store_binding_backup_sha"; then
    prior_serving_store_binding_matches=1
  fi
elif [ ! -e "$SERVING_STORE_BINDING" ] && [ ! -L "$SERVING_STORE_BINDING" ]; then
  prior_serving_store_binding_matches=1
fi
if [ "$prior_serving_store_binding_matches" -ne 1 ]; then
  release_subrouter_mutation_lease
  trap - EXIT INT TERM
  transaction_active=0
  die "default-shell serving-store binding changed before publication; candidate retained and rollback withheld (journal: $TRANSACTION_DIR)"
fi
set_phase serving_store_binding_requested
candidate_base_url="${health_url%/_subrouter/health}"
if ! /usr/bin/env -u SUBROUTER_STATE_DIR \
  SUBROUTER_LOCAL_BASE_URL="$candidate_base_url" \
  "$WORKER_BIN" daemon bind-state "$STATE_DIR" --local-data-socket "$LOCAL_DATA_SOCKET" "${serving_store_bind_cas_args[@]}"; then
  if rollback; then
    die "candidate serving-store binding failed; legacy LaunchAgent restored"
  fi
  release_subrouter_mutation_lease
  trap - EXIT INT TERM
  transaction_active=0
  die "candidate serving-store binding failed and rollback was withheld (journal: $TRANSACTION_DIR)"
fi
if ! require_sole_unix_listener_owner "$LOCAL_DATA_SOCKET" "$candidate_pid" \
  || ! verify_candidate_serving_store_binding "$SERVING_STORE_BINDING" "$STATE_DIR" "$LOCAL_DATA_SOCKET" \
  || ! verify_file_sha256 "$SERVING_STORE_BINDING" "$candidate_serving_store_binding_sha" \
  || ! fsync_parent_directory "$SERVING_STORE_BINDING"; then
  if rollback; then
    die "candidate serving-store binding was not durably published; legacy LaunchAgent restored"
  fi
  release_subrouter_mutation_lease
  trap - EXIT INT TERM
  transaction_active=0
  die "candidate serving-store binding identity is unexpected; rollback withheld (journal: $TRANSACTION_DIR)"
fi
printf 'candidate-serving-store-binding-published  %s  %s\n' \
  "$candidate_serving_store_binding_sha" "$SERVING_STORE_BINDING" >>"$identity_manifest"
inject_hard_fault_after_mutation serving_store_binding_publish
set_phase serving_store_bound

echo "running bounded functional canary"
if ! SUBROUTER_CANARY_TRANSACTION_WORKER_PATH="$WORKER_BIN" \
  SUBROUTER_CANARY_TRANSACTION_WORKER_SHA256="$candidate_worker_sha" \
  SUBROUTER_BOUNDED_STATE_DIRECTORY="$TRANSACTION_DIR/functional-canary-process-group" \
  run_bounded_argv "functional canary" "$CANARY_TIMEOUT" \
    /usr/bin/env -u SUBROUTER_STATE_DIR "$CANARY_CALLBACK"; then
  if [ "${RUN_BOUNDED_CLEANUP_CONFIRMED:-0}" -ne 1 ]; then
    release_subrouter_mutation_lease
    trap - EXIT INT TERM
    transaction_active=0
    die "functional canary termination is unverified; rollback withheld and transaction journal retained (group state: ${RUN_BOUNDED_GROUP_STATE_FILE:-unavailable})"
  fi
  rollback
  die "functional canary failed; legacy LaunchAgent restored"
fi
set_phase canary_completed
# A prior shadow rehearsal reduces candidate risk; it does not prove this live
# process identity or session continuity. Keep rollback armed through both.
post_canary_failure=""
post_canary_active_worker_fingerprint=""
if ! verify_file_sha256 "$PLIST" "$candidate_plist_sha"; then
  post_canary_failure="installed plist identity"
elif ! verify_file_sha256 "$SUPERVISOR_BIN" "$candidate_supervisor_sha"; then
  post_canary_failure="supervisor executable identity"
elif ! verify_file_sha256 "$WORKER_BIN" "$candidate_worker_sha"; then
  post_canary_failure="worker executable identity"
elif ! verify_file_sha256 "$SERVING_STORE_BINDING" "$candidate_serving_store_binding_sha"; then
  post_canary_failure="default-shell serving-store binding identity"
elif ! require_process_fingerprint "$candidate_fingerprint" "$SUPERVISOR_BIN"; then
  post_canary_failure="supervisor process continuity before final HTTP acceptance"
elif ! require_sole_listener_owner "$public_addr" "$candidate_pid"; then
  post_canary_failure="listener ownership before final HTTP acceptance"
elif ! wait_for_http_acceptance "$health_url" "$ready_url"; then
  post_canary_failure="final HTTP health and readiness"
elif ! require_sole_listener_owner "$public_addr" "$candidate_pid"; then
  post_canary_failure="listener ownership after final HTTP acceptance"
elif ! require_process_fingerprint "$candidate_fingerprint" "$SUPERVISOR_BIN"; then
  post_canary_failure="supervisor process continuity after final HTTP acceptance"
else
  post_canary_active_worker_fingerprint="$(supervisor_acceptance_fingerprint "$CONTROL_SOCKET" "$(id -u)" "$candidate_worker_cdhash" || true)"
  [ "$post_canary_active_worker_fingerprint" = "$candidate_active_worker_fingerprint" ] \
    || post_canary_failure="supervisor lifecycle or active worker continuity after final HTTP acceptance"
fi
if [ -n "$post_canary_failure" ]; then
  echo "post-canary acceptance failed: $post_canary_failure" >&2
  rollback
  die "candidate acceptance changed during canary; legacy LaunchAgent restored"
fi
# Disarm only after the same live candidate remains structurally and
# functionally accepted after the canary transaction.
set_phase accepted
rm -f "$UPGRADE_INHIBIT_FILE"
transaction_active=0
release_subrouter_mutation_lease
trap - EXIT INT TERM
rm -rf "$TRANSACTION_DIR"

echo "supervised Subrouter passed health, readiness, and functional canary acceptance"
echo "control socket: $CONTROL_SOCKET"
echo "backup: $backup"
echo "rollback identity manifest: $identity_manifest"
echo "standalone rollback:"
printf '  %q' "$SCRIPT_DIR/rollback-launchagent-supervisor.sh" \
  --backup "$backup" --backup-sha256 "$backup_sha256" \
  "${rollback_identity_args[@]}" "${serving_store_rollback_args[@]}" \
  --public-addr "$public_addr" \
  --expected-program "$SUPERVISOR_BIN"
printf '\n'
echo
echo "Upgrades are now non-disruptive:"
echo "  curl -fsS --unix-socket $CONTROL_SOCKET -X POST http://localhost/_subrouter/upgrade"
