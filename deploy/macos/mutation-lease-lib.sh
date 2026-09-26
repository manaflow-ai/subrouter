#!/usr/bin/env bash

subrouter_mutation_process_start_identity() {
  python3 - "$1" <<'PY'
import ctypes
import struct
import sys

pid = int(sys.argv[1])
if sys.platform == "darwin":
    libproc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
    proc_pidinfo = libproc.proc_pidinfo
    proc_pidinfo.argtypes = [
        ctypes.c_int, ctypes.c_int, ctypes.c_uint64,
        ctypes.c_void_p, ctypes.c_int,
    ]
    proc_pidinfo.restype = ctypes.c_int
    raw = ctypes.create_string_buffer(136)
    if proc_pidinfo(pid, 3, 0, raw, len(raw)) != len(raw):
        raise SystemExit(1)
    seconds, microseconds = struct.unpack_from("<QQ", raw.raw, 120)
    if seconds == 0:
        raise SystemExit(1)
    print(f"darwin:{seconds}:{microseconds}")
elif sys.platform.startswith("linux"):
    fields = open(f"/proc/{pid}/stat", encoding="ascii").read().rsplit(")", 1)[1].split()
    print(f"linux:{fields[19]}")
else:
    raise SystemExit(1)
PY
}

# Acquire a crash-released BSD file lock without passing the lock descriptor to
# commands started by the caller. The helper owns the descriptor and exits as
# soon as its shell parent and any explicitly adopted rollback child are gone,
# so ordinary callback descendants cannot accidentally keep the lease alive.
acquire_subrouter_mutation_lease() {
  local lock_file="$1" control_dir ready_file helper_pid attempts result
  control_dir="$(mktemp -d "${TMPDIR:-/tmp}/subrouter-mutation-lease.XXXXXX")" || return 73
  ready_file="$control_dir/ready"
  python3 - "$lock_file" "$control_dir" "$$" <<'PY' &
import ctypes
import fcntl
import os
import struct
import sys
import time

lock_file, control_dir, parent_text = sys.argv[1:]
parent_pid = int(parent_text)
ready_file = os.path.join(control_dir, "ready")
adopt_request = os.path.join(control_dir, "adopt")
adopt_ack = os.path.join(control_dir, "adopted")


def write_state(path, value):
    next_path = f"{path}.next.{os.getpid()}"
    descriptor = os.open(next_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.write(descriptor, value.encode("ascii"))
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    os.replace(next_path, path)
    directory = os.open(control_dir, os.O_RDONLY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def process_identity(pid):
    if sys.platform == "darwin":
        libproc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
        proc_pidinfo = libproc.proc_pidinfo
        proc_pidinfo.argtypes = [
            ctypes.c_int, ctypes.c_int, ctypes.c_uint64,
            ctypes.c_void_p, ctypes.c_int,
        ]
        proc_pidinfo.restype = ctypes.c_int
        raw = ctypes.create_string_buffer(136)
        if proc_pidinfo(pid, 3, 0, raw, len(raw)) != len(raw):
            return None
        parent = struct.unpack_from("<I", raw.raw, 16)[0]
        seconds, microseconds = struct.unpack_from("<QQ", raw.raw, 120)
        if seconds == 0:
            return None
        return parent, f"darwin:{seconds}:{microseconds}"
    if sys.platform.startswith("linux"):
        try:
            fields = open(f"/proc/{pid}/stat", encoding="ascii").read().rsplit(")", 1)[1].split()
            return int(fields[1]), f"linux:{fields[19]}"
        except (IndexError, OSError, ValueError):
            return None
    return None


descriptor = os.open(lock_file, os.O_WRONLY | os.O_CREAT, 0o600)
try:
    fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
except BlockingIOError:
    write_state(ready_file, "busy\n")
    raise SystemExit(75)
write_state(ready_file, "acquired\n")
adopted = {}
try:
    while True:
        try:
            with open(adopt_request, encoding="ascii") as request:
                candidate_text, request_nonce, requested_start = request.read().split()
                candidate_pid = int(candidate_text)
                if len(request_nonce) != 32 or any(
                    char not in "0123456789abcdef" for char in request_nonce
                ):
                    raise ValueError
        except FileNotFoundError:
            candidate_pid = 0
        except (OSError, ValueError):
            candidate_pid = -1
        if candidate_pid > 1:
            # ppid and start time come from one kernel record and the child
            # supplied the same start identity before publication. Re-read it
            # before ACK so neither stale requests nor PID reuse can splice an
            # old direct-child relationship onto a new process.
            before = process_identity(candidate_pid)
            after = process_identity(candidate_pid)
            if (
                before is not None
                and before[0] == parent_pid
                and before[1] == requested_start
                and after == before
            ):
                adopted[candidate_pid] = before[1]
                write_state(adopt_ack, f"{candidate_pid} {request_nonce} {before[1]}\n")
            try:
                os.unlink(adopt_request)
            except FileNotFoundError:
                pass
        still_adopted = {}
        for pid, expected in adopted.items():
            current = process_identity(pid)
            if current is not None and current[1] == expected:
                still_adopted[pid] = expected
                continue
            if current is None:
                try:
                    os.kill(pid, 0)
                    still_adopted[pid] = expected
                except PermissionError:
                    still_adopted[pid] = expected
                except ProcessLookupError:
                    pass
        adopted = still_adopted
        if os.getppid() != parent_pid and not adopted:
            break
        time.sleep(0.01)
finally:
    for path in (ready_file, adopt_request, adopt_ack):
        try:
            os.unlink(path)
        except FileNotFoundError:
            pass
    try:
        os.rmdir(control_dir)
    except OSError:
        pass
PY
  helper_pid=$!
  attempts=0
  while [ "$attempts" -lt 1000 ]; do
    if [ -s "$ready_file" ]; then
      result="$(tr -d '[:space:]' <"$ready_file")"
      rm -f "$ready_file"
      if [ "$result" = acquired ]; then
        SUBROUTER_MUTATION_LEASE_PID="$helper_pid"
        SUBROUTER_MUTATION_LEASE_CONTROL_DIR="$control_dir"
        return 0
      fi
      wait "$helper_pid" 2>/dev/null || true
      rmdir "$control_dir" 2>/dev/null || true
      return 75
    fi
    if ! kill -0 "$helper_pid" 2>/dev/null; then
      wait "$helper_pid" 2>/dev/null || true
      rm -f "$ready_file"
      rmdir "$control_dir" 2>/dev/null || true
      return 73
    fi
    attempts=$((attempts + 1))
    sleep 0.01
  done
  kill "$helper_pid" 2>/dev/null || true
  wait "$helper_pid" 2>/dev/null || true
  rm -f "$ready_file"
  rmdir "$control_dir" 2>/dev/null || true
  return 73
}

release_subrouter_mutation_lease() {
  local helper_pid="${SUBROUTER_MUTATION_LEASE_PID:-}"
  local control_dir="${SUBROUTER_MUTATION_LEASE_CONTROL_DIR:-}"
  case "$helper_pid" in ''|*[!0-9]*) return 0 ;; esac
  kill "$helper_pid" 2>/dev/null || true
  wait "$helper_pid" 2>/dev/null || true
  if [ -n "$control_dir" ] && [ -d "$control_dir" ]; then
    rm -f "$control_dir/ready" "$control_dir/adopt" "$control_dir/adopted"
    rmdir "$control_dir" 2>/dev/null || true
  fi
  SUBROUTER_MUTATION_LEASE_PID=""
  SUBROUTER_MUTATION_LEASE_CONTROL_DIR=""
}

subrouter_mutation_lease_is_held_by_parent() {
  local lock_file="$1"
  local owner_pid="${SUBROUTER_MUTATION_LEASE_OWNER_PID:-}"
  local helper_pid="${SUBROUTER_MUTATION_LEASE_HELPER_PID:-}"
  local control_dir="${SUBROUTER_MUTATION_LEASE_CONTROL_DIR:-}"
  local helper_parent request_next request_nonce request_identity
  local ack_pid ack_nonce ack_identity attempts=0
  case "$owner_pid:$helper_pid" in
    *[!0-9:]*|:|*:|0:*|1:*) return 1 ;;
  esac
  [ -n "$control_dir" ] && [ -d "$control_dir" ] && [ ! -L "$control_dir" ] || return 1
  [ "$owner_pid" -eq "$PPID" ] || return 1
  kill -0 "$helper_pid" 2>/dev/null || return 1
  helper_parent="$(/bin/ps -p "$helper_pid" -o ppid= 2>/dev/null | tr -d '[:space:]')"
  [ "$helper_parent" = "$owner_pid" ] || return 1
  # The helper relationship is necessary but not sufficient: prove the shared
  # kernel lease itself remains unavailable before trusting the parent handoff.
  ! /usr/bin/lockf -s -k -t 0 "$lock_file" /usr/bin/true 2>/dev/null || return 1

  # Transfer crash continuity without passing the descriptor to callbacks. The
  # helper accepts only a direct child of its original owner, captures that
  # process generation, and acknowledges before rollback performs any mutation.
  request_next="$control_dir/adopt.next.$$"
  request_nonce="$(python3 -c 'import secrets; print(secrets.token_hex(16))')" || return 1
  request_identity="$(subrouter_mutation_process_start_identity "$$")" || return 1
  (umask 077; printf '%s %s %s\n' "$$" "$request_nonce" "$request_identity" >"$request_next") || return 1
  mv -f "$request_next" "$control_dir/adopt" || { rm -f "$request_next"; return 1; }
  while [ "$attempts" -lt 1000 ]; do
    if [ -s "$control_dir/adopted" ]; then
      read -r ack_pid ack_nonce ack_identity <"$control_dir/adopted" || true
      if [ "$ack_pid" = "$$" ] && [ "$ack_nonce" = "$request_nonce" ] \
        && [ "$ack_identity" = "$request_identity" ]; then
        kill -0 "$helper_pid" 2>/dev/null || return 1
        ! /usr/bin/lockf -s -k -t 0 "$lock_file" /usr/bin/true 2>/dev/null || return 1
        return 0
      fi
    fi
    kill -0 "$helper_pid" 2>/dev/null || return 1
    attempts=$((attempts + 1))
    sleep 0.01
  done
  return 1
}
