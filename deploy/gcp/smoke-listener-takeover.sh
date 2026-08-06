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

[[ -x "${candidate}" ]] || { echo "candidate binary is not executable" >&2; exit 1; }
[[ "${backend_port}" =~ ^[0-9]+$ && "${test_port}" =~ ^[0-9]+$ ]] \
  || { echo "ports must be numeric" >&2; exit 1; }
(( bootstrap_port <= 65535 )) || { echo "test port is too high" >&2; exit 1; }
for command in curl jq readlink ss systemctl systemd-run; do
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

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  systemctl stop "${front_unit}" "${source_unit}" >/dev/null 2>&1 || true
  systemctl reset-failed "${front_unit}" "${source_unit}" >/dev/null 2>&1 || true
  rm -f -- "${control_socket}" "${transfer_socket}"
  exit "${status}"
}
trap cleanup EXIT INT TERM

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

systemd-run --quiet --unit="${front_unit}" --property=Type=simple \
  --property=User=subrouter --property=Group=subrouter \
  --property=NoNewPrivileges=yes --property=PrivateTmp=yes \
  --property=ProtectSystem=full --property=ProtectHome=read-only \
  --property=ReadWritePaths=/var/lib/subrouter \
  "${candidate}" front --addr "0.0.0.0:${bootstrap_port}" \
  --control-socket "${control_socket}" --listener-transfer-socket "${transfer_socket}" \
  --backend-id slot-a \
  --backend-network tcp --backend-address "127.0.0.1:${backend_port}"

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
    "${candidate}" listener-transfer --socket "${transfer_socket}" \
      --address "0.0.0.0:${test_port}" --source-pid "${source_pid}" --source-fd "${source_fd}"
  ! systemctl is-active --quiet "${transfer_unit}" \
    || { echo "one-shot listener helper remained active" >&2; exit 1; }
}
transfer_listener
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

# Exercise the failure-recovery path while the source still owns its copy:
# move the front back to its bootstrap listener, then repeat the exact handoff.
restore_payload="$(jq -nc --arg address "0.0.0.0:${bootstrap_port}" '{address:$address}')"
curl -fsS --unix-socket "${control_socket}" -H 'Content-Type: application/json' \
  --data-binary "${restore_payload}" -X POST http://localhost/_subrouter/replace-listener >/dev/null
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
if has_ptrace_capability "${front_pid}"; then
  echo "restarted front unexpectedly holds CAP_SYS_PTRACE" >&2
  exit 1
fi
transfer_listener
if has_ptrace_capability "${front_pid}"; then
  echo "restarted front gained CAP_SYS_PTRACE during listener takeover" >&2
  exit 1
fi
front_line="$(ss -H -lntp "sport = :${test_port}" | grep -F "pid=${front_pid}," | head -n 1)"
front_fd="$(sed -n "s/.*pid=${front_pid},fd=\([0-9][0-9]*\).*/\1/p" <<<"${front_line}")"
front_inode="$(readlink "/proc/${front_pid}/fd/${front_fd}")"
[[ "${front_inode}" == "${source_inode}" ]] || { echo "repeated handoff inherited a different socket" >&2; exit 1; }

systemctl stop "${source_unit}"
curl -fsS "http://127.0.0.1:${test_port}/_subrouter/ready" >/dev/null
jq -nc --arg inode "${front_inode}" --argjson source_pid "${source_pid}" \
  --argjson source_fd "${source_fd}" --argjson front_pid "${front_pid}" --argjson front_fd "${front_fd}" \
  '{success:true,same_kernel_socket:true,inode:$inode,
    source:{pid:$source_pid,fd:$source_fd},front:{pid:$front_pid,fd:$front_fd}}'
