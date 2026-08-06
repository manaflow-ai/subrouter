#!/usr/bin/env bash
# Exercise pidfd listener takeover under the production systemd sandbox on an unused port.
set -euo pipefail

[[ "$(id -u)" == 0 ]] || { echo "run as root" >&2; exit 1; }
[[ "$#" == 3 ]] || { echo "usage: $0 <candidate-binary> <backend-port> <test-port>" >&2; exit 2; }

candidate="$1"
backend_port="$2"
test_port="$3"
bootstrap_port=$((test_port + 1))
run_id="$$"
source_user="nobody"
source_group="$(id -gn "${source_user}")"
source_unit="subrouter-listener-source-${run_id}.service"
front_unit="subrouter-listener-front-${run_id}.service"
control_socket="/var/lib/subrouter/listener-smoke-${run_id}.sock"
transfer_socket="/var/lib/subrouter/listener-smoke-${run_id}-transfer.sock"
front_address_file="/var/lib/subrouter/listener-smoke-${run_id}.address"
front_candidate="/var/lib/subrouter/listener-smoke-${run_id}.bin"
hold_ready="/tmp/subrouter-listener-smoke-${run_id}.hold-ready"
hold_release="/tmp/subrouter-listener-smoke-${run_id}.hold-release"
hold_pid=""

[[ -x "${candidate}" ]] || { echo "candidate binary is not executable" >&2; exit 1; }
[[ "${backend_port}" =~ ^[0-9]+$ && "${test_port}" =~ ^[0-9]+$ ]] \
  || { echo "ports must be numeric" >&2; exit 1; }
(( bootstrap_port <= 65535 )) || { echo "test port is too high" >&2; exit 1; }
for command in curl jq python3 readlink ss systemctl systemd-run; do
  command -v "${command}" >/dev/null 2>&1 || { echo "${command} is required" >&2; exit 1; }
done

has_ptrace_capability() {
  local pid="$1"
  python3 - "${pid}" <<'PY'
import pathlib
import sys

status = pathlib.Path(f"/proc/{int(sys.argv[1])}/status").read_text(encoding="utf-8")
effective = int(next(line.split()[1] for line in status.splitlines() if line.startswith("CapEff:")), 16)
raise SystemExit(0 if effective & (1 << 19) else 1)
PY
}

assert_single_stored_listener() {
  local stored=""
  for _ in $(seq 1 100); do
    stored="$(systemctl show "${front_unit}" -p NFileDescriptorStore --value)"
    [[ "${stored}" == 1 ]] && return 0
    sleep 0.01
  done
  echo "front descriptor store contains ${stored:-no} listeners, want exactly one" >&2
  return 1
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  touch "${hold_release}" 2>/dev/null || true
  [[ -z "${hold_pid}" ]] || wait "${hold_pid}" >/dev/null 2>&1 || true
  systemctl stop "${front_unit}" "${source_unit}" >/dev/null 2>&1 || true
  systemctl reset-failed "${front_unit}" "${source_unit}" >/dev/null 2>&1 || true
  rm -f -- "${control_socket}" "${transfer_socket}" "${front_address_file}" "${front_candidate}" \
    "${hold_ready}" "${hold_release}"
  exit "${status}"
}
trap cleanup EXIT INT TERM
install -o root -g root -m 0755 "${candidate}" "${front_candidate}"

# A different source UID makes the smoke require the helper's ptrace capability
# even when the host's Yama policy is permissive.
systemd-run --quiet --unit="${source_unit}" --property=Type=simple \
  --property="User=${source_user}" --property="Group=${source_group}" \
  /usr/bin/python3 -c \
  'import socket,sys,time; s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("0.0.0.0",int(sys.argv[1]))); s.listen(128); time.sleep(300)' \
  "${test_port}"

source_pid="$(systemctl show "${source_unit}" -p MainPID --value)"
for _ in $(seq 1 100); do
  ss -H -lntp "sport = :${test_port}" | grep -F "pid=${source_pid}," >/dev/null 2>&1 && break
  systemctl is-active --quiet "${source_unit}" || { echo "source listener stopped" >&2; exit 1; }
  sleep 0.05
done
source_line="$(ss -H -lntp "sport = :${test_port}" | grep -F "pid=${source_pid}," | head -n 1)"
source_fd="$(sed -n "s/.*pid=${source_pid},fd=\([0-9][0-9]*\).*/\1/p" <<<"${source_line}")"
source_inode="$(readlink "/proc/${source_pid}/fd/${source_fd}")"
[[ "${source_inode}" =~ ^socket:\[[0-9]+\]$ ]] || { echo "source listener identity is invalid" >&2; exit 1; }

printf '0.0.0.0:%s\n' "${bootstrap_port}" >"${front_address_file}"
chown subrouter:subrouter "${front_address_file}"
chmod 0600 "${front_address_file}"
# shellcheck disable=SC2016 # Expanded by the service's bash process.
systemd-run --quiet --unit="${front_unit}" --property=Type=simple \
  --property=User=subrouter --property=Group=subrouter \
  --property=NoNewPrivileges=yes --property=PrivateTmp=yes \
  --property=ProtectSystem=full --property=ProtectHome=read-only \
  --property=NotifyAccess=all --property=FileDescriptorStoreMax=2 \
  --property=ReadWritePaths=/var/lib/subrouter \
  /bin/bash -c 'exec "$1" front --addr "$(cat "$2")" --control-socket "$3" --listener-transfer-socket "$4" --backend-id slot-a --backend-network tcp --backend-address "127.0.0.1:$5"' \
  subrouter-listener-smoke "${front_candidate}" "${front_address_file}" "${control_socket}" "${transfer_socket}" "${backend_port}"

for _ in $(seq 1 100); do
  if [[ -S "${control_socket}" && -S "${transfer_socket}" ]] && \
     curl -fsS --unix-socket "${control_socket}" http://localhost/_subrouter/front-status >/dev/null 2>&1
  then
    break
  fi
  systemctl is-active --quiet "${front_unit}" || { journalctl -u "${front_unit}" --no-pager >&2; exit 1; }
  sleep 0.05
done
front_pid="$(systemctl show "${front_unit}" -p MainPID --value)"
assert_single_stored_listener
if has_ptrace_capability "${front_pid}"; then
  echo "front unexpectedly holds CAP_SYS_PTRACE" >&2
  exit 1
fi
transfer_count=0
transfer_listener() {
  local transfer_unit
  transfer_count=$((transfer_count + 1))
  transfer_unit="subrouter-listener-helper-${run_id}-${transfer_count}.service"
  systemd-run --quiet --wait --collect --pipe --unit="${transfer_unit}" \
    --property=Type=exec --property=User=subrouter --property=Group=subrouter \
    --property=NoNewPrivileges=yes --property=PrivateTmp=yes \
    --property=ProtectSystem=full --property=ProtectHome=read-only \
    --property=CapabilityBoundingSet=CAP_SYS_PTRACE \
    --property=AmbientCapabilities=CAP_SYS_PTRACE \
    --property=RuntimeMaxSec=15s \
    "${front_candidate}" listener-transfer --socket "${transfer_socket}" \
      --address "0.0.0.0:${test_port}" --source-pid "${source_pid}" --source-fd "${source_fd}"
  ! systemctl is-active --quiet "${transfer_unit}" \
    || { echo "one-shot listener helper remained active" >&2; exit 1; }
}
transfer_listener
assert_single_stored_listener
[[ "$(systemctl show "${front_unit}" -p MainPID --value)" == "${front_pid}" ]] \
  || { echo "front restarted during listener takeover" >&2; exit 1; }
if has_ptrace_capability "${front_pid}"; then
  echo "front gained CAP_SYS_PTRACE during listener takeover" >&2
  exit 1
fi
front_line="$(ss -H -lntp "sport = :${test_port}" | grep -F "pid=${front_pid}," | head -n 1)"
front_fd="$(sed -n "s/.*pid=${front_pid},fd=\([0-9][0-9]*\).*/\1/p" <<<"${front_line}")"
front_inode="$(readlink "/proc/${front_pid}/fd/${front_fd}")"
[[ "${front_inode}" == "${source_inode}" ]] || { echo "front inherited a different socket" >&2; exit 1; }
if ss -H -lntp "sport = :${bootstrap_port}" | grep -F "pid=${front_pid}," >/dev/null 2>&1; then
  echo "front retained its bootstrap listener after takeover" >&2
  exit 1
fi

accepted=0
for _ in $(seq 1 128); do
  python3 -c 'import socket,sys; socket.create_connection(("127.0.0.1", int(sys.argv[1])), 1).close()' "${test_port}" \
    >/dev/null 2>&1 || true
  accepted="$(curl -fsS --unix-socket "${control_socket}" http://localhost/_subrouter/front-status | \
    jq -r '.listener.accepted_connections // -1')"
  [[ "${accepted}" =~ ^[0-9]+$ ]] || { echo "front returned an invalid listener acceptance count" >&2; exit 1; }
  (( accepted > 0 )) && break
  sleep 0.01
done
(( accepted > 0 )) || { echo "front did not accept a connection on the inherited listener" >&2; exit 1; }

# Restart before the configured address changes. The inherited public listener
# is deliberately mismatched, so startup must discard it and recover on the
# still-configured bootstrap address while the source keeps serving public.
pre_mismatch_restart_pid="${front_pid}"
systemctl restart "${front_unit}"
for _ in $(seq 1 100); do
  [[ -S "${control_socket}" && -S "${transfer_socket}" ]] && \
    curl -fsS --unix-socket "${control_socket}" http://localhost/_subrouter/front-status >/dev/null 2>&1 && break
  sleep 0.05
done
front_pid="$(systemctl show "${front_unit}" -p MainPID --value)"
[[ "${front_pid}" != "${pre_mismatch_restart_pid}" ]] \
  || { echo "front PID did not change across mismatched-listener restart" >&2; exit 1; }
assert_single_stored_listener
ss -H -lntp "sport = :${bootstrap_port}" | grep -F "pid=${front_pid}," >/dev/null \
  || { echo "front did not recover its configured bootstrap listener" >&2; exit 1; }
if ss -H -lntp "sport = :${test_port}" | grep -F "pid=${front_pid}," >/dev/null 2>&1; then
  echo "front retained the mismatched inherited listener" >&2
  exit 1
fi
transfer_listener
assert_single_stored_listener

# Exercise the failure-recovery path while the source still owns its copy:
# move the front back to its bootstrap listener, then repeat the exact handoff.
restore_payload="$(jq -nc --arg address "0.0.0.0:${bootstrap_port}" '{address:$address}')"
curl -fsS --unix-socket "${control_socket}" -H 'Content-Type: application/json' \
  --data-binary "${restore_payload}" -X POST http://localhost/_subrouter/replace-listener >/dev/null
assert_single_stored_listener
if ss -H -lntp "sport = :${test_port}" | grep -F "pid=${front_pid}," >/dev/null 2>&1; then
  echo "front retained the inherited listener after bootstrap restoration" >&2
  exit 1
fi
ss -H -lntp "sport = :${bootstrap_port}" | grep -F "pid=${front_pid}," >/dev/null \
  || { echo "front did not restore its bootstrap listener" >&2; exit 1; }
systemctl restart "${front_unit}"
for _ in $(seq 1 100); do
  [[ -S "${control_socket}" && -S "${transfer_socket}" ]] && \
    curl -fsS --unix-socket "${control_socket}" http://localhost/_subrouter/front-status >/dev/null 2>&1 && break
  sleep 0.05
done
front_pid="$(systemctl show "${front_unit}" -p MainPID --value)"
assert_single_stored_listener
if has_ptrace_capability "${front_pid}"; then
  echo "restarted front unexpectedly holds CAP_SYS_PTRACE" >&2
  exit 1
fi
transfer_listener
assert_single_stored_listener
if has_ptrace_capability "${front_pid}"; then
  echo "restarted front gained CAP_SYS_PTRACE during listener takeover" >&2
  exit 1
fi
front_line="$(ss -H -lntp "sport = :${test_port}" | grep -F "pid=${front_pid}," | head -n 1)"
front_fd="$(sed -n "s/.*pid=${front_pid},fd=\([0-9][0-9]*\).*/\1/p" <<<"${front_line}")"
front_inode="$(readlink "/proc/${front_pid}/fd/${front_fd}")"
[[ "${front_inode}" == "${source_inode}" ]] || { echo "repeated handoff inherited a different socket" >&2; exit 1; }

# Retry while the same public listener is already active. The new stored copy
# must survive cleanup of the previous process-local copy.
transfer_listener
assert_single_stored_listener
front_line="$(ss -H -lntp "sport = :${test_port}" | grep -F "pid=${front_pid}," | head -n 1)"
front_fd="$(sed -n "s/.*pid=${front_pid},fd=\([0-9][0-9]*\).*/\1/p" <<<"${front_line}")"
front_inode="$(readlink "/proc/${front_pid}/fd/${front_fd}")"
[[ "${front_inode}" == "${source_inode}" ]] || { echo "same-address retry inherited a different socket" >&2; exit 1; }

# Persist the production address, hot-reload the front, and prove the successor
# serves the exact same kernel listener before the old process drains.
printf '0.0.0.0:%s\n' "${test_port}" >"${front_address_file}"
pre_restart_front_pid="${front_pid}"
python3 - "${test_port}" "${hold_ready}" "${hold_release}" <<'PY' &
import pathlib
import socket
import sys
import time

connection = socket.create_connection(("127.0.0.1", int(sys.argv[1])), 2)
pathlib.Path(sys.argv[2]).touch()
while not pathlib.Path(sys.argv[3]).exists():
    time.sleep(0.01)
connection.close()
PY
hold_pid=$!
for _ in $(seq 1 100); do
  [[ -e "${hold_ready}" ]] && break
  kill -0 "${hold_pid}" 2>/dev/null || { echo "held front connection exited early" >&2; exit 1; }
  sleep 0.01
done
[[ -e "${hold_ready}" ]] || { echo "held front connection was not established" >&2; exit 1; }
kill -HUP "${pre_restart_front_pid}"
for _ in $(seq 1 100); do
  front_pid="$(systemctl show "${front_unit}" -p MainPID --value)"
  [[ "${front_pid}" != "${pre_restart_front_pid}" ]] && break
  kill -0 "${pre_restart_front_pid}" 2>/dev/null || { echo "front exited before promoting its successor" >&2; exit 1; }
  sleep 0.05
done
[[ "${front_pid}" != "${pre_restart_front_pid}" ]] \
  || { echo "front did not promote a successor while its held connection remained live" >&2; exit 1; }
kill -0 "${pre_restart_front_pid}" 2>/dev/null \
  || { echo "old front did not remain alive to drain its held connection" >&2; exit 1; }
curl -fsS "http://127.0.0.1:${test_port}/_subrouter/ready" >/dev/null \
  || { echo "promoted front did not route a new connection during the old session drain" >&2; exit 1; }
touch "${hold_release}"
wait "${hold_pid}"
hold_pid=""
for _ in $(seq 1 100); do
  ! kill -0 "${pre_restart_front_pid}" 2>/dev/null && break
  sleep 0.05
done
! kill -0 "${pre_restart_front_pid}" 2>/dev/null \
  || { echo "old front did not exit after its held connection drained" >&2; exit 1; }
for _ in $(seq 1 100); do
  [[ -S "${control_socket}" && -S "${transfer_socket}" ]] && \
    curl -fsS --unix-socket "${control_socket}" http://localhost/_subrouter/front-status >/dev/null 2>&1 && break
  sleep 0.05
done
front_pid="$(systemctl show "${front_unit}" -p MainPID --value)"
assert_single_stored_listener
[[ "${front_pid}" != "${pre_restart_front_pid}" ]] \
  || { echo "front PID did not change across descriptor handoff" >&2; exit 1; }
front_line="$(ss -H -lntp "sport = :${test_port}" | grep -F "pid=${front_pid}," | head -n 1)"
front_fd="$(sed -n "s/.*pid=${front_pid},fd=\([0-9][0-9]*\).*/\1/p" <<<"${front_line}")"
front_inode="$(readlink "/proc/${front_pid}/fd/${front_fd}")"
[[ "${front_inode}" == "${source_inode}" ]] \
  || { echo "front reload did not preserve the handed-off kernel socket" >&2; exit 1; }

systemctl stop "${source_unit}"
curl -fsS "http://127.0.0.1:${test_port}/_subrouter/ready" >/dev/null
jq -nc --arg inode "${front_inode}" --argjson source_pid "${source_pid}" \
  --argjson source_fd "${source_fd}" --argjson front_pid "${front_pid}" --argjson front_fd "${front_fd}" \
  '{success:true,same_kernel_socket:true,reload_preserved_socket:true,inode:$inode,
    source:{pid:$source_pid,fd:$source_fd},front:{pid:$front_pid,fd:$front_fd}}'
