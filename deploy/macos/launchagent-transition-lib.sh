#!/usr/bin/env bash
# Shared fail-closed launchd transition helpers. This file is sourced by the
# LaunchAgent migration and rollback commands; it is not an entry point.

launchagent_die() {
  echo "${SUBROUTER_TRANSITION_NAME:-subrouter-launchagent-transition}: $*" >&2
  exit 1
}

positive_integer() {
  case "${1:-}" in
    ''|*[!0-9]*|0) return 1 ;;
    *) return 0 ;;
  esac
}

plist_value() {
  local plist="$1" key="$2"
  /usr/libexec/PlistBuddy -c "Print :${key}" "$plist" 2>/dev/null
}

plist_program() {
  local plist="$1" program
  program="$(plist_value "$plist" Program || true)"
  if [ -z "$program" ]; then
    program="$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$plist" 2>/dev/null)"
  fi
  printf '%s\n' "$program"
}

sha256_file() {
  shasum -a 256 "$1" | awk '{print $1}'
}

verify_file_sha256() {
  local path="$1" expected="$2" actual
  [ -f "$path" ] && [ ! -L "$path" ] \
    || { echo "identity file $path is missing, non-regular, or a symlink" >&2; return 1; }
  actual="$(sha256_file "$path")"
  [ "$actual" = "$expected" ] || {
    echo "identity mismatch for $path: expected $expected, got $actual" >&2
    return 1
  }
}

copy_file_nofollow() {
  python3 - "$1" "$2" "$3" <<'PY'
import os, shutil, stat, sys
source, destination, mode = sys.argv[1:]
flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
source_fd = os.open(source, flags)
try:
    info = os.fstat(source_fd)
    if not stat.S_ISREG(info.st_mode):
        raise SystemExit("source is not a regular file")
    destination_fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, int(mode, 8))
    try:
        with os.fdopen(source_fd, "rb", closefd=False) as src, os.fdopen(destination_fd, "wb", closefd=False) as dst:
            shutil.copyfileobj(src, dst)
            dst.flush(); os.fsync(dst.fileno())
    finally:
        os.close(destination_fd)
finally:
    os.close(source_fd)
PY
}

atomic_restore_nofollow() {
  local artifact="$1" destination="$2" expected="$3" mode="$4" next
  verify_file_sha256 "$artifact" "$expected" || return 1
  next="${destination}.rollback-next.$$"
  rm -f "$next"
  copy_file_nofollow "$artifact" "$next" "$mode" || return 1
  verify_file_sha256 "$artifact" "$expected" || { rm -f "$next"; return 1; }
  verify_file_sha256 "$next" "$expected" || { rm -f "$next"; return 1; }
  mv -f "$next" "$destination"
}

fsync_parent_directory() {
  python3 - "$1" <<'PY'
import os
import sys

parent = os.path.dirname(sys.argv[1]) or "."
flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0)
descriptor = os.open(parent, flags)
try:
    os.fsync(descriptor)
finally:
    os.close(descriptor)
PY
}

plist_executable_dependencies() {
  python3 - "$1" <<'PY'
import os
import plistlib
import shlex
import sys

with open(sys.argv[1], "rb") as stream:
    plist = plistlib.load(stream)

paths = []
program = plist.get("Program")
arguments = list(plist.get("ProgramArguments") or [])
for candidate in [program, *arguments]:
    if isinstance(candidate, str) and os.path.isabs(candidate):
        paths.append(candidate)

# A legacy LaunchAgent commonly starts a small shell wrapper. Discover literal
# absolute executable paths in that wrapper (worker and helper binaries) while
# ignoring comments, shebangs, variables, and non-executable secret files.
wrapper = program or (arguments[0] if arguments else None)
if isinstance(wrapper, str) and os.path.isfile(wrapper):
    try:
        with open(wrapper, "r", encoding="utf-8") as stream:
            content = stream.read().replace("\\\n", " ")
            for line in content.splitlines():
                if line.startswith("#!") or line.lstrip().startswith("#"):
                    continue
                try:
                    words = shlex.split(line, comments=True, posix=True)
                except ValueError:
                    continue
                paths.extend(word for word in words if os.path.isabs(word))
    except (OSError, UnicodeError):
        pass

for path in dict.fromkeys(paths):
    if os.path.isfile(path) and os.access(path, os.X_OK):
        print(path)
PY
}

launchagent_job_field() {
  local service="$1" field="$2"
  launchctl print "$service" 2>/dev/null \
    | awk -v field="$field" '$1 == field && $2 == "=" {
        sub(/^[^=]*=[[:space:]]*/, "", $0); print; exit
      }'
}

launchagent_job_loaded() {
  launchctl print "$1" >/dev/null 2>&1
}

capture_launchctl_snapshot() {
  local service="$1" destination="$2"
  launchctl print "$service" >"$destination" 2>/dev/null
}

launchctl_snapshot_field() {
  local snapshot="$1" field="$2"
  awk -v field="$field" '$1 == field && $2 == "=" {
    sub(/^[^=]*=[[:space:]]*/, "", $0); print; exit
  }' "$snapshot"
}

require_plist_identity() {
  local plist="$1" expected_label="$2" expected_program="$3"
  [ -f "$plist" ] || launchagent_die "$plist not found"
  [ "$(plist_value "$plist" Label)" = "$expected_label" ] \
    || launchagent_die "$plist does not declare label $expected_label"
  [ "$(plist_program "$plist")" = "$expected_program" ] \
    || launchagent_die "$plist does not declare expected program $expected_program"
}

capture_loaded_identity() {
  local service="$1" expected_program="$2"
  local program pid
  launchagent_job_loaded "$service" || return 1
  program="$(launchagent_job_field "$service" program)"
  pid="$(launchagent_job_field "$service" pid)"
  [ "$program" = "$expected_program" ] \
    || launchagent_die "$service runs unexpected program ${program:-unknown}"
  case "$pid" in
    ''|*[!0-9]*) launchagent_die "$service has no stable numeric pid" ;;
  esac
  printf '%s\n' "$pid"
}

process_fingerprint() {
  local pid="$1" program="$2" program_sha command start
  program_sha="$(sha256_file "$program")"
  command="$(ps -p "$pid" -o command= | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]][[:space:]]*/ /g')"
  start="$(ps -p "$pid" -o lstart= | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]][[:space:]]*/ /g')"
  [ -n "$command" ] && [ -n "$start" ] || return 1
  printf '%s|%s|%s|%s\n' "$pid" "$program_sha" "$start" "$command"
}

require_process_fingerprint() {
  local expected="$1" program="$2" pid
  pid="${expected%%|*}"
  [ "$(process_fingerprint "$pid" "$program")" = "$expected" ]
}

plist_public_addr() {
  python3 - "$1" <<'PY'
import plistlib
import sys

with open(sys.argv[1], "rb") as stream:
    arguments = list(plistlib.load(stream).get("ProgramArguments") or [])

for index, argument in enumerate(arguments):
    if argument == "--addr" and index + 1 < len(arguments):
        print(arguments[index + 1])
        break
    if argument.startswith("--addr="):
        print(argument.split("=", 1)[1])
        break
else:
    raise SystemExit("plist has no --addr")
PY
}

listener_present() {
  local public_addr="$1" port
  port="${public_addr##*:}"
  case "$port" in
    ''|*[!0-9]*) launchagent_die "cannot determine listener port from $public_addr" ;;
  esac
  lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1
}

wait_for_full_absence() {
  local service="$1" captured_pid="$2" public_addr="$3"
  local attempts="${SUBROUTER_ABSENCE_ATTEMPTS:-120}"
  local interval="${SUBROUTER_ABSENCE_INTERVAL:-1}"
  local stable=0 current_pid i

  i=0
  while [ "$i" -lt "$attempts" ]; do
    i=$((i + 1))
    if launchagent_job_loaded "$service"; then
      current_pid="$(launchagent_job_field "$service" pid)"
      if [ -n "$captured_pid" ] && [ "$current_pid" != "$captured_pid" ]; then
        echo "$service changed pid during removal ($captured_pid -> ${current_pid:-unknown})" >&2
        return 1
      fi
      stable=0
    elif { [ -z "$captured_pid" ] || ! kill -0 "$captured_pid" 2>/dev/null; } \
      && ! listener_present "$public_addr"; then
      stable=$((stable + 1))
      [ "$stable" -ge 2 ] && return 0
    else
      stable=0
    fi
    sleep "$interval"
  done
  echo "$service did not reach full label, pid, and listener absence" >&2
  return 1
}

bootstrap_with_retry() {
  local domain="$1" plist="$2" service="$3" public_addr="$4"
  local attempts="${SUBROUTER_BOOTSTRAP_ATTEMPTS:-10}"
  local interval="${SUBROUTER_BOOTSTRAP_INTERVAL:-2}"
  local i=0
  while [ "$i" -lt "$attempts" ]; do
    if launchctl bootstrap "$domain" "$plist"; then
      return 0
    fi
    i=$((i + 1))
    wait_for_full_absence "$service" "" "$public_addr" || return 1
    sleep "$interval"
  done
  return 1
}

pid_is_candidate_descendant() {
  local pid="$1" candidate="$2" parent steps=0
  while [ "$pid" -gt 1 ] 2>/dev/null && [ "$steps" -lt 64 ]; do
    [ "$pid" = "$candidate" ] && return 0
    parent="$(ps -p "$pid" -o ppid= | tr -d '[:space:]')"
    case "$parent" in ''|*[!0-9]*) return 1 ;; esac
    pid="$parent"; steps=$((steps + 1))
  done
  return 1
}

require_sole_listener_owner() {
  local public_addr="$1" candidate_pid="$2" port pid found=0
  port="${public_addr##*:}"
  while IFS= read -r pid; do
    case "$pid" in ''|*[!0-9]*) continue ;; esac
    found=1
    pid_is_candidate_descendant "$pid" "$candidate_pid" || {
      echo "listener on port $port is owned by unexpected pid $pid" >&2
      return 1
    }
  done < <(lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | sort -u)
  [ "$found" -eq 1 ] || { echo "no listener owner found on port $port" >&2; return 1; }
}

require_control_socket_status() {
  local socket="$1" expected_uid="$2" status
  [ "$(stat -f '%HT' "$socket" 2>/dev/null)" = "Socket" ] || return 1
  [ "$(stat -f '%Lp' "$socket" 2>/dev/null)" = "600" ] || return 1
  [ "$(stat -f '%u' "$socket" 2>/dev/null)" = "$expected_uid" ] || return 1
  status="$(curl -fsS --max-time 2 --unix-socket "$socket" http://localhost/_subrouter/supervisor-status)" || return 1
  printf '%s' "$status" | python3 -c 'import json,sys; d=json.load(sys.stdin); valid=d.get("accepting") is True and d.get("retiring") is False and bool(d.get("active",{}).get("id")) and len(d.get("backends",[])) == 1; sys.exit(0 if valid else 1)'
}

wait_for_http_acceptance() {
  local health_url="$1" ready_url="$2"
  local attempts="${SUBROUTER_HEALTH_ATTEMPTS:-60}"
  local interval="${SUBROUTER_HEALTH_INTERVAL:-1}"
  local i=0
  while [ "$i" -lt "$attempts" ]; do
    if curl -fsS --max-time 2 "$health_url" >/dev/null 2>&1 \
      && curl -fsS --max-time 2 "$ready_url" >/dev/null 2>&1; then
      return 0
    fi
    i=$((i + 1))
    sleep "$interval"
  done
  return 1
}

drain_bounded_process_group() {
  local name="$1" state_file="$2" status
  local saved_int_trap saved_term_trap saved_hup_trap
  saved_int_trap="$(trap -p INT)"
  saved_term_trap="$(trap -p TERM)"
  saved_hup_trap="$(trap -p HUP)"
  trap '' INT TERM HUP
  if python3 - "$name" "$state_file" <<'PY'
import ctypes
import errno
import json
import os
import secrets
import signal
import subprocess
import sys
import time

name, state_file = sys.argv[1:]
for caught_signal in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
    signal.signal(caught_signal, signal.SIG_IGN)
try:
    with open(state_file, "r", encoding="ascii") as source:
        state = json.load(source)
    if set(state) != {"pgid", "leader_start", "callback_token"}:
        raise ValueError
    pgid = state["pgid"]
    expected_leader_start = state["leader_start"]
    callback_token = state["callback_token"]
    if (
        not isinstance(pgid, int)
        or not isinstance(expected_leader_start, str)
        or not isinstance(callback_token, str)
        or len(callback_token) != 64
        or any(char not in "0123456789abcdef" for char in callback_token)
    ):
        raise ValueError
except FileNotFoundError:
    raise SystemExit(0)
except (OSError, ValueError):
    print(f"{name} watchdog process-group identity is invalid", file=sys.stderr)
    raise SystemExit(1)

if pgid <= 1:
    print(f"{name} watchdog process-group identity is unsafe", file=sys.stderr)
    raise SystemExit(1)


class DarwinBSDInfo(ctypes.Structure):
    _fields_ = [
        ("pbi_flags", ctypes.c_uint32), ("pbi_status", ctypes.c_uint32),
        ("pbi_xstatus", ctypes.c_uint32), ("pbi_pid", ctypes.c_uint32),
        ("pbi_ppid", ctypes.c_uint32), ("pbi_uid", ctypes.c_uint32),
        ("pbi_gid", ctypes.c_uint32), ("pbi_ruid", ctypes.c_uint32),
        ("pbi_rgid", ctypes.c_uint32), ("pbi_svuid", ctypes.c_uint32),
        ("pbi_svgid", ctypes.c_uint32), ("rfu_1", ctypes.c_uint32),
        ("pbi_comm", ctypes.c_char * 16), ("pbi_name", ctypes.c_char * 32),
        ("pbi_nfiles", ctypes.c_uint32), ("pbi_pgid", ctypes.c_uint32),
        ("pbi_pjobc", ctypes.c_uint32), ("e_tdev", ctypes.c_uint32),
        ("e_tpgid", ctypes.c_uint32), ("pbi_nice", ctypes.c_int32),
        ("pbi_start_tvsec", ctypes.c_uint64),
        ("pbi_start_tvusec", ctypes.c_uint64),
    ]


class DarwinAuditToken(ctypes.Structure):
    _fields_ = [("val", ctypes.c_uint32 * 8)]


def process_start_identity(pid):
    if sys.platform == "darwin":
        try:
            libproc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
            proc_pidinfo = libproc.proc_pidinfo
            proc_pidinfo.argtypes = [
                ctypes.c_int, ctypes.c_int, ctypes.c_uint64,
                ctypes.c_void_p, ctypes.c_int,
            ]
            proc_pidinfo.restype = ctypes.c_int
            info = DarwinBSDInfo()
            size = ctypes.sizeof(info)
            if proc_pidinfo(pid, 3, 0, ctypes.byref(info), size) != size:
                return None
            return f"darwin:{info.pbi_start_tvsec}:{info.pbi_start_tvusec}"
        except (AttributeError, OSError):
            return None
    if sys.platform.startswith("linux"):
        try:
            fields = open(f"/proc/{pid}/stat", encoding="ascii").read().rsplit(")", 1)[1].split()
            return f"linux:{fields[19]}"
        except (IndexError, OSError):
            return None
    return None


def signal_process_identity(pid, expected_start, sent_signal):
    if process_start_identity(pid) != expected_start:
        return False
    if sys.platform == "darwin":
        libsystem = ctypes.CDLL("/usr/lib/libSystem.B.dylib", use_errno=True)
        libproc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
        self_task = ctypes.c_uint.in_dll(libsystem, "mach_task_self_").value
        task = ctypes.c_uint()
        task_name_for_pid = libsystem.task_name_for_pid
        task_name_for_pid.argtypes = [ctypes.c_uint, ctypes.c_int, ctypes.POINTER(ctypes.c_uint)]
        task_name_for_pid.restype = ctypes.c_int
        if task_name_for_pid(self_task, pid, ctypes.byref(task)) != 0:
            if process_start_identity(pid) != expected_start:
                return False
            raise RuntimeError("could not bind callback process identity")
        try:
            token = DarwinAuditToken()
            count = ctypes.c_uint32(8)
            task_info = libsystem.task_info
            task_info.argtypes = [
                ctypes.c_uint, ctypes.c_int,
                ctypes.POINTER(ctypes.c_uint32), ctypes.POINTER(ctypes.c_uint32),
            ]
            task_info.restype = ctypes.c_int
            if task_info(
                task.value,
                15,  # TASK_AUDIT_TOKEN
                ctypes.cast(ctypes.byref(token), ctypes.POINTER(ctypes.c_uint32)),
                ctypes.byref(count),
            ) != 0 or count.value != 8:
                raise RuntimeError("could not read callback process identity token")
        finally:
            libsystem.mach_port_deallocate(self_task, task.value)
        if token.val[5] != pid or process_start_identity(pid) != expected_start:
            return False
        proc_signal = libproc.proc_signal_with_audittoken
        proc_signal.argtypes = [ctypes.POINTER(DarwinAuditToken), ctypes.c_int]
        proc_signal.restype = ctypes.c_int
        if proc_signal(ctypes.byref(token), int(sent_signal)) != 0:
            error = ctypes.get_errno()
            if error == errno.ESRCH:
                return False
            raise OSError(error, "identity-bound callback signal failed")
        return True
    try:
        os.kill(pid, sent_signal)
        return True
    except ProcessLookupError:
        return False


group_signaling_allowed = True
leader_identity_checks = 0
fault_reuse_after_sample = (
    os.environ.get(
        "SUBROUTER_FAULT_INJECT_BOUNDED_DRAIN_PGID_REUSE_AFTER_SAMPLE",
        "",
    )
    == name
)


def leader_identity_matches():
    global group_signaling_allowed, leader_identity_checks
    if not group_signaling_allowed:
        return False
    current_leader_start = process_start_identity(pgid)
    if fault_reuse_after_sample and leader_identity_checks > 0:
        current_leader_start = None
    leader_identity_checks += 1
    if current_leader_start != expected_leader_start:
        # Once the recorded leader is absent or has a different kernel start
        # identity, this numeric PGID may already belong to unrelated work. It
        # must never become eligible for group inspection or signaling again.
        group_signaling_allowed = False
        return False
    return True


def candidate_identities(user_only=False):
    command = ["/bin/ps"]
    if user_only:
        command.extend(["-U", str(os.getuid())])
    else:
        command.append("-ax")
    command.extend(["-o", "pid="])
    result = subprocess.run(
        command,
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        timeout=3,
        start_new_session=True,
        env={"PATH": "/usr/bin:/bin"},
    )
    if result.returncode != 0:
        raise RuntimeError("process identity inspection failed")
    identities = {}
    for raw_pid in result.stdout.splitlines():
        try:
            pid = int(raw_pid.strip())
        except ValueError:
            continue
        identity = process_start_identity(pid)
        if identity is not None:
            identities[pid] = identity
    return identities


def live_members():
    if not leader_identity_matches():
        return {}
    identities_before = candidate_identities()
    result = subprocess.run(
        ["/bin/ps", "-axo", "pid=", "-o", "pgid=", "-o", "state="],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        timeout=3,
    )
    if result.returncode != 0:
        raise RuntimeError("process inspection failed")
    if not leader_identity_matches():
        return {}
    members = {}
    for line in result.stdout.splitlines():
        fields = line.split()
        if len(fields) != 3:
            continue
        try:
            candidate = int(fields[0])
            candidate_pgid = int(fields[1])
        except ValueError:
            continue
        # Zombies cannot execute or generate traffic and are waiting only for
        # their new parent to reap them.
        if candidate_pgid == pgid and not fields[2].startswith(b"Z"):
            identity = identities_before.get(candidate)
            if identity is not None and process_start_identity(candidate) == identity:
                members[candidate] = identity
    return members


def marked_identities():
    marker = b"SUBROUTER_BOUNDED_CALLBACK_TOKEN=" + callback_token.encode("ascii")
    identities_before = candidate_identities(user_only=True)
    result = subprocess.run(
        [
            "/bin/ps", "eww", "-U", str(os.getuid()),
            "-o", "pid=", "-o", "command=",
        ],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        timeout=3,
        start_new_session=True,
        env={"PATH": "/usr/bin:/bin"},
    )
    if result.returncode != 0:
        raise RuntimeError("marked process inspection failed")
    identities = {}
    for line in result.stdout.splitlines():
        fields = line.lstrip().split(None, 1)
        if len(fields) != 2 or marker not in fields[1]:
            continue
        try:
            pid = int(fields[0])
        except ValueError:
            continue
        identity = identities_before.get(pid)
        if identity is not None and process_start_identity(pid) == identity:
            identities[pid] = identity
    return identities


def signal_identities(identities, sent_signal):
    for pid, expected_start in identities.items():
        signal_process_identity(pid, expected_start, sent_signal)


term_sent = False
term_deadline = 0.0
last_inspection_warning = 0.0
tracked_marked = {}
while True:
    try:
        members = live_members()
        tracked_marked.update(marked_identities())
    except (OSError, RuntimeError, subprocess.SubprocessError):
        now = time.monotonic()
        if now - last_inspection_warning >= 5:
            print(
                f"{name} callback process inspection unavailable; rollback remains withheld",
                file=sys.stderr,
            )
            last_inspection_warning = now
        time.sleep(0.1)
        continue
    tracked_marked = {
        pid: started
        for pid, started in tracked_marked.items()
        if process_start_identity(pid) == started
    }
    if not members and not tracked_marked:
        break
    if not term_sent:
        signal_identities(members, signal.SIGTERM)
        signal_identities(tracked_marked, signal.SIGTERM)
        term_sent = True
        term_deadline = time.monotonic() + 5
    elif time.monotonic() >= term_deadline:
        signal_identities(members, signal.SIGKILL)
        signal_identities(tracked_marked, signal.SIGKILL)
    time.sleep(0.05)
PY
  then
    status=0
  else
    status=$?
  fi
  if [ -n "$saved_int_trap" ]; then eval "$saved_int_trap"; else trap - INT; fi
  if [ -n "$saved_term_trap" ]; then eval "$saved_term_trap"; else trap - TERM; fi
  if [ -n "$saved_hup_trap" ]; then eval "$saved_hup_trap"; else trap - HUP; fi
  return "$status"
}

run_bounded_argv() {
  local name="$1" timeout="$2"
  local state_dir state_file status cleanup_status
  RUN_BOUNDED_CLEANUP_CONFIRMED=0
  RUN_BOUNDED_GROUP_STATE_FILE=""
  shift 2
  positive_integer "$timeout" \
    || launchagent_die "$name timeout must be a positive integer"
  [ "$#" -gt 0 ] || launchagent_die "$name command is required"

  if [ -n "${SUBROUTER_BOUNDED_STATE_DIRECTORY:-}" ]; then
    state_dir="$SUBROUTER_BOUNDED_STATE_DIRECTORY"
    mkdir -m 0700 "$state_dir" \
      || launchagent_die "could not create private $name watchdog state"
  else
    state_dir="$(mktemp -d "${TMPDIR:-/tmp}/subrouter-bounded.XXXXXX")" \
      || launchagent_die "could not create private $name watchdog state"
  fi
  chmod 0700 "$state_dir"
  state_file="$state_dir/process-group"
  RUN_BOUNDED_GROUP_STATE_FILE="$state_file"

  if python3 - "$name" "$timeout" "$state_file" "$@" <<'PY'
import os
import ctypes
import json
import secrets
import signal
import subprocess
import sys
import time

name, timeout, state_file, *command = sys.argv[1:]
parent_pid = os.getppid()
caught_signals = (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)
previous_mask = signal.pthread_sigmask(signal.SIG_BLOCK, caught_signals)
pending_signal = None
process = None


class DarwinBSDInfo(ctypes.Structure):
    _fields_ = [
        ("pbi_flags", ctypes.c_uint32), ("pbi_status", ctypes.c_uint32),
        ("pbi_xstatus", ctypes.c_uint32), ("pbi_pid", ctypes.c_uint32),
        ("pbi_ppid", ctypes.c_uint32), ("pbi_uid", ctypes.c_uint32),
        ("pbi_gid", ctypes.c_uint32), ("pbi_ruid", ctypes.c_uint32),
        ("pbi_rgid", ctypes.c_uint32), ("pbi_svuid", ctypes.c_uint32),
        ("pbi_svgid", ctypes.c_uint32), ("rfu_1", ctypes.c_uint32),
        ("pbi_comm", ctypes.c_char * 16), ("pbi_name", ctypes.c_char * 32),
        ("pbi_nfiles", ctypes.c_uint32), ("pbi_pgid", ctypes.c_uint32),
        ("pbi_pjobc", ctypes.c_uint32), ("e_tdev", ctypes.c_uint32),
        ("e_tpgid", ctypes.c_uint32), ("pbi_nice", ctypes.c_int32),
        ("pbi_start_tvsec", ctypes.c_uint64),
        ("pbi_start_tvusec", ctypes.c_uint64),
    ]


def process_start_identity(pid):
    if sys.platform == "darwin":
        libproc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
        proc_pidinfo = libproc.proc_pidinfo
        proc_pidinfo.argtypes = [
            ctypes.c_int, ctypes.c_int, ctypes.c_uint64,
            ctypes.c_void_p, ctypes.c_int,
        ]
        proc_pidinfo.restype = ctypes.c_int
        info = DarwinBSDInfo()
        size = ctypes.sizeof(info)
        if proc_pidinfo(pid, 3, 0, ctypes.byref(info), size) != size:
            raise RuntimeError("callback leader start identity is unavailable")
        return f"darwin:{info.pbi_start_tvsec}:{info.pbi_start_tvusec}"
    if sys.platform.startswith("linux"):
        fields = open(f"/proc/{pid}/stat", encoding="ascii").read().rsplit(")", 1)[1].split()
        return f"linux:{fields[19]}"
    raise RuntimeError("callback leader start identity is unsupported")


class ForkedProcess:
    def __init__(self, pid):
        self.pid = pid
        self.returncode = None
        self.leader_start = None
        self.group_signaling_allowed = False

    def _record(self, status):
        # waitpid has reaped the leader, so its numeric PID/PGID can be reused.
        # The shell-owned drain will still find token-bound descendants by
        # their individual kernel start identities.
        self.group_signaling_allowed = False
        if os.WIFEXITED(status):
            self.returncode = os.WEXITSTATUS(status)
        elif os.WIFSIGNALED(status):
            self.returncode = -os.WTERMSIG(status)
        else:
            self.returncode = 125
        return self.returncode

    def poll(self):
        if self.returncode is not None:
            return self.returncode
        waited, status = os.waitpid(self.pid, os.WNOHANG)
        return None if waited == 0 else self._record(status)

    def wait(self):
        if self.returncode is None:
            _waited, status = os.waitpid(self.pid, 0)
            self._record(status)
        return self.returncode


def interrupted(signum, _frame):
    global pending_signal
    if pending_signal is None:
        pending_signal = signum


for caught_signal in caught_signals:
    signal.signal(caught_signal, interrupted)


def terminate_owned_group():
    # The anchor is this watchdog's direct child and is never waited elsewhere.
    # While poll() returns None it is live; if it exits before killpg it remains
    # our unreaped zombie, so its numeric PID/PGID still cannot be reused.
    if process is None:
        return
    if process.poll() is None:
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    deadline = time.monotonic() + 5
    while process.poll() is None and time.monotonic() < deadline:
        time.sleep(0.05)
    if process.poll() is None:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    if process.poll() is None:
        process.wait()


def spawn_after_group_publication():
    """Create the callback group, publish it durably, then permit exec."""
    ready_read, ready_write = os.pipe()
    release_read, release_write = os.pipe()
    callback_token = secrets.token_hex(32)
    child_pid = os.fork()
    if child_pid == 0:
        try:
            os.close(ready_read)
            os.close(release_write)
            os.setsid()
            signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask)
            os.write(ready_write, b"1")
            os.close(ready_write)
            released = os.read(release_read, 1)
            os.close(release_read)
            if released != b"1":
                os._exit(125)
            anchor_pid = os.getpid()
            os.environ["SUBROUTER_BOUNDED_GROUP_ANCHOR_PID"] = str(anchor_pid)
            os.environ["SUBROUTER_BOUNDED_CALLBACK_TOKEN"] = callback_token
            for caught_signal in caught_signals:
                signal.signal(caught_signal, signal.SIG_IGN)
            callback_pid = os.fork()
            if callback_pid == 0:
                for caught_signal in caught_signals:
                    signal.signal(caught_signal, signal.SIG_DFL)
                os.execvpe(command[0], command, os.environ)
            _waited, callback_status = os.waitpid(callback_pid, 0)

            # Keep the original process-group leader (and its kernel start
            # identity) alive until no callback descendant remains. This makes
            # the durable group record safe across watchdog death and delayed
            # transaction reentry.
            while True:
                try:
                    result = subprocess.run(
                        [
                            "/bin/ps", "eww", "-U", str(os.getuid()),
                            "-o", "pid=", "-o", "pgid=", "-o", "state=",
                            "-o", "command=",
                        ],
                        check=False,
                        stdout=subprocess.PIPE,
                        stderr=subprocess.DEVNULL,
                        timeout=3,
                        start_new_session=True,
                        env={"PATH": "/usr/bin:/bin"},
                    )
                    if result.returncode != 0:
                        raise RuntimeError
                    members = []
                    for line in result.stdout.splitlines():
                        fields = line.lstrip().split(None, 3)
                        if len(fields) < 3:
                            continue
                        pid, pgid = int(fields[0]), int(fields[1])
                        command_text = fields[3] if len(fields) == 4 else b""
                        marked = (
                            b"SUBROUTER_BOUNDED_CALLBACK_TOKEN="
                            + callback_token.encode("ascii")
                        ) in command_text
                        if (
                            pid != anchor_pid
                            and not fields[2].startswith(b"Z")
                            and (pgid == anchor_pid or marked)
                        ):
                            members.append(pid)
                    if not members:
                        break
                except (OSError, RuntimeError, ValueError, subprocess.SubprocessError):
                    pass
                time.sleep(0.05)
            if os.WIFEXITED(callback_status):
                os._exit(os.WEXITSTATUS(callback_status))
            if os.WIFSIGNALED(callback_status):
                os._exit(128 + os.WTERMSIG(callback_status))
            os._exit(125)
        except BaseException:
            os._exit(126)

    child = ForkedProcess(child_pid)
    os.close(ready_write)
    os.close(release_read)
    try:
        ready = os.read(ready_read, 1)
        if ready != b"1":
            child.wait()
            raise RuntimeError("callback process group did not become ready")
        if (
            os.environ.get(
                "SUBROUTER_FAULT_INJECT_BOUNDED_WATCHDOG_SIGKILL_BEFORE_PUBLISH",
                "",
            ) == name
        ):
            os.kill(os.getpid(), signal.SIGKILL)
        leader_start = process_start_identity(child.pid)
        child.leader_start = leader_start
        child.group_signaling_allowed = True
        descriptor = os.open(
            state_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600
        )
        try:
            state = {
                "pgid": child.pid,
                "leader_start": leader_start,
                "callback_token": callback_token,
            }
            os.write(
                descriptor,
                (json.dumps(state, sort_keys=True, separators=(",", ":")) + "\n").encode(),
            )
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
        state_directory = os.open(os.path.dirname(state_file), os.O_RDONLY)
        try:
            os.fsync(state_directory)
        finally:
            os.close(state_directory)
        os.write(release_write, b"1")
        return child
    finally:
        os.close(ready_read)
        os.close(release_write)


try:
    process = spawn_after_group_publication()
    fault_name = os.environ.get("SUBROUTER_FAULT_INJECT_BOUNDED_NAME", "")
    inject_fault = fault_name == name
    fault_pid_file = (
        os.environ.get("SUBROUTER_FAULT_INJECT_BOUNDED_CHILD_PID_FILE", "")
        if inject_fault
        else ""
    )
    if fault_pid_file:
        descriptor = os.open(
            fault_pid_file, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600
        )
        try:
            os.write(descriptor, f"{process.pid}\n".encode())
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    fault_signal = (
        os.environ.get("SUBROUTER_FAULT_INJECT_BOUNDED_SIGNAL", "")
        if inject_fault
        else ""
    )
    if fault_signal:
        injected_signals = {
            "INT": signal.SIGINT,
            "TERM": signal.SIGTERM,
            "HUP": signal.SIGHUP,
        }
        if fault_signal not in injected_signals:
            raise SystemExit("invalid bounded callback fault signal")
        os.kill(os.getpid(), injected_signals[fault_signal])
except BaseException:
    terminate_owned_group()
    raise
finally:
    signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask)


deadline = time.monotonic() + int(timeout)
while True:
    if pending_signal is not None:
        terminate_owned_group()
        raise SystemExit(128 + pending_signal)
    result = process.poll()
    if result is not None:
        # poll() reaped the anchor, so its numeric PID/PGID is no longer a safe
        # inspection key. The anchor normally exits only after its descendants;
        # the shell-owned token/start-identity drain handles abnormal leftovers.
        raise SystemExit(result)
    if os.getppid() != parent_pid:
        terminate_owned_group()
        raise SystemExit(125)
    if time.monotonic() >= deadline:
        print(f"{name} timed out after {timeout}s", file=sys.stderr)
        terminate_owned_group()
        raise SystemExit(124)
    time.sleep(0.05)
PY
  then
    status=0
  else
    status=$?
  fi

  # This shell, not the replaceable Python watchdog, owns the final callback
  # process-group drain.  A watchdog SIGKILL therefore cannot return control to
  # migration rollback while the callback or any same-session descendant can
  # still generate traffic.
  if drain_bounded_process_group "$name" "$state_file"; then
    cleanup_status=0
  else
    cleanup_status=$?
  fi
  if [ "$cleanup_status" -ne 0 ]; then
    echo "$name callback termination is unverified; retained $state_file" >&2
    return 125
  fi
  # shellcheck disable=SC2034 # consumed by the migration caller after return
  RUN_BOUNDED_CLEANUP_CONFIRMED=1
  rm -f "$state_file"
  rmdir "$state_dir"
  # shellcheck disable=SC2034 # consumed by the migration caller after return
  RUN_BOUNDED_GROUP_STATE_FILE=""
  return "$status"
}
