#!/usr/bin/env bash
# Install immutable release bytes and manage the stable front plus two private
# supervisor slots. This script runs as root on the VM. LB changes stay in the
# separate operator-side migration script.
set -euo pipefail

RELEASE_ROOT="${SUBROUTER_RELEASE_ROOT:-/opt/subrouter/releases}"
SLOT_ROOT="${SUBROUTER_SLOT_ROOT:-/opt/subrouter/slots}"
FRONT_ROOT="${SUBROUTER_FRONT_ROOT:-/opt/subrouter/front}"
CONTROL_ROOT="${SUBROUTER_CONTROL_ROOT:-/opt/subrouter/control}"
STATE_DIR="${SUBROUTER_STATE_DIR:-/var/lib/subrouter}"
FRONT_SOCKET="${SUBROUTER_FRONT_CONTROL_SOCKET:-${STATE_DIR}/front.sock}"
FRONT_TRANSFER_SOCKET="${SUBROUTER_FRONT_TRANSFER_SOCKET:-${STATE_DIR}/front-listener.sock}"
FRONT_ENV="${SUBROUTER_FRONT_ENV:-/etc/default/subrouter-front}"
FRESH_MARKER="${SUBROUTER_FRESH_TOPOLOGY_MARKER:-${STATE_DIR}/front-topology-prepared}"
DEFAULTS_FILE="${SUBROUTER_DEFAULTS_FILE:-/etc/default/subrouter}"
LEGACY_SERVICE="${SUBROUTER_LEGACY_SERVICE:-subrouter.service}"
LEGACY_SOCKET_SERVICE="${SUBROUTER_LEGACY_SOCKET_SERVICE:-subrouter.socket}"
LEGACY_CONTROL_SOCKET="${SUBROUTER_LEGACY_CONTROL_SOCKET:-${STATE_DIR}/supervisor.sock}"
SLOT_UNIT="${SUBROUTER_SLOT_UNIT:-/etc/systemd/system/subrouter-slot@.service}"
FRONT_UNIT="${SUBROUTER_FRONT_UNIT:-/etc/systemd/system/subrouter-front.service}"
SLOT_ENV_DIR="${SUBROUTER_SLOT_ENV_DIR:-/etc/subrouter/slots}"
LOG_DIR="${SUBROUTER_LOG_DIR:-/var/log/subrouter}"
VERIFY_UNIT="${SUBROUTER_VERIFY_UNIT:-/etc/systemd/system/subrouter-verify.service}"
VERIFY_DROPIN_DIR="${SUBROUTER_VERIFY_DROPIN_DIR:-${VERIFY_UNIT}.d}"
DEPLOYMENT_CONTRACT="${SUBROUTER_DEPLOYMENT_CONTRACT:-/usr/local/libexec/subrouter-deployment-contract}"

log() { printf 'install-front-slots: %s\n' "$*"; }
die() { log "$*" >&2; exit 1; }

[[ "$(id -u)" == "0" ]] || die "run as root"
for required_command in curl jq python3 sha256sum systemctl; do
  command -v "${required_command}" >/dev/null 2>&1 \
    || die "${required_command} is required"
done
[[ -f "${DEPLOYMENT_CONTRACT}" && ! -L "${DEPLOYMENT_CONTRACT}" ]] \
  || die "deployment contract must be a regular non-symlink file: ${DEPLOYMENT_CONTRACT}"

validate_tag() {
  [[ "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] \
    || die "release tag must look like v0.1.52"
}

validate_slot() {
  [[ "$1" == "slot-a" || "$1" == "slot-b" ]] \
    || die "slot must be slot-a or slot-b"
}

slot_port() {
  case "$1" in
    slot-a) printf '31417\n' ;;
    slot-b) printf '31418\n' ;;
    *) return 1 ;;
  esac
}

release_binary() {
  printf '%s/%s/subrouter\n' "${RELEASE_ROOT}" "$1"
}

atomic_symlink() {
  local target="$1"
  local destination="$2"
  local temporary="${destination}.new.$$"
  install -d -m 0755 "$(dirname "${destination}")"
  rm -f -- "${temporary}"
  ln -s -- "${target}" "${temporary}"
  mv -Tf -- "${temporary}" "${destination}"
}

wait_endpoint() {
  local url="$1"
  local service="$2"
  for _ in $(seq 1 120); do
    if curl -fsS --max-time 2 "${url}" >/dev/null 2>&1; then
      return 0
    fi
    systemctl is-active --quiet "${service}" \
      || die "${service} stopped before ${url} became ready"
    sleep 0.25
  done
  die "${service} did not become ready at ${url}"
}

wait_slot_endpoint() {
  local port="$1"
  local path="$2"
  local service="$3"
  for _ in $(seq 1 120); do
    if python3 "${DEPLOYMENT_CONTRACT}" probe-slot-endpoint "${port}" "${path}" \
        >/dev/null 2>&1
    then
      return 0
    fi
    systemctl is-active --quiet "${service}" \
      || die "${service} stopped before ${path} became ready"
    sleep 0.25
  done
  die "${service} did not become ready at ${path}"
}

write_front_default() {
  local slot="$1"
  local address="${2:-0.0.0.0:31416}"
  local port
  validate_slot "${slot}"
  [[ "${address}" == "0.0.0.0:31415" || "${address}" == "0.0.0.0:31416" ]] \
    || die "front address must be 0.0.0.0:31415 or 0.0.0.0:31416"
  port="$(slot_port "${slot}")"
  local temporary
  install -d -m 0755 "$(dirname "${FRONT_ENV}")"
  if [[ -e "${FRONT_ENV}" || -L "${FRONT_ENV}" ]]; then
    [[ -f "${FRONT_ENV}" ]] || die "front default target is not a regular file: ${FRONT_ENV}"
  fi
  temporary="$(mktemp "${FRONT_ENV}.tmp.XXXXXX")"
  {
    printf 'SUBROUTER_FRONT_BACKEND_ID=%s\n' "${slot}"
    printf 'SUBROUTER_FRONT_BACKEND_NETWORK=tcp\n'
    printf 'SUBROUTER_FRONT_BACKEND_ADDRESS=127.0.0.1:%s\n' "${port}"
    printf 'SUBROUTER_FRONT_ADDR=%s\n' "${address}"
  } >"${temporary}"
  chmod 0644 "${temporary}"
  mv -f -- "${temporary}" "${FRONT_ENV}"
}

front_default_value() {
  local key="$1" fallback="$2" value=""
  if [[ -f "${FRONT_ENV}" && ! -L "${FRONT_ENV}" ]]; then
    value="$(sed -n "s/^${key}=//p" "${FRONT_ENV}" | tail -n 1)"
  fi
  printf '%s\n' "${value:-${fallback}}"
}

set_front_default() {
  local slot="$1"
  write_front_default "${slot}" \
    "$(front_default_value SUBROUTER_FRONT_ADDR 0.0.0.0:31416)"
}

write_verify_front_address() {
  local address="$1" temporary
  [[ "${address}" == "127.0.0.1:31415" || "${address}" == "127.0.0.1:31416" ]] \
    || die "verify front address must use the managed public port"
  [[ -f "${VERIFY_UNIT}" ]] || return 0
  temporary="$(mktemp "${VERIFY_UNIT}.tmp.XXXXXX")"
  {
    printf '[Unit]\n'
    printf 'Description=Subrouter Claude rate-limit reroute self-verification\n'
    printf 'After=subrouter-front.service\n'
    printf '\n'
    printf '[Service]\n'
    printf 'Type=oneshot\n'
    printf 'Environment=SUBROUTER_VERIFY_HEALTH_URL=http://%s/_subrouter/health\n' "${address}"
    printf 'Environment=SUBROUTER_VERIFY_USAGE_URL=http://%s/_subrouter/usage-status\n' "${address}"
    printf 'Environment=SUBROUTER_VERIFY_PROXY_URL=http://%s/v1/messages\n' "${address}"
    printf 'ExecStart=/usr/local/bin/subrouter-verify.sh\n'
    printf 'User=root\n'
    printf 'Nice=10\n'
  } >"${temporary}"
  chmod 0644 "${temporary}"
  mv -f -- "${temporary}" "${VERIFY_UNIT}"
  rm -f -- "${VERIFY_DROPIN_DIR}/front.conf"
}

install_release() {
  local tag="$1"
  local candidate="$2"
  local expected="$3"
  validate_tag "${tag}"
  [[ "${expected}" =~ ^[0-9a-f]{64}$ ]] || die "expected SHA-256 is invalid"
  [[ -f "${candidate}" && ! -L "${candidate}" ]] || die "candidate must be a regular file"
  local actual destination directory incoming
  actual="$(sha256sum "${candidate}" | awk '{print $1}')"
  [[ "${actual}" == "${expected}" ]] || die "candidate checksum mismatch"
  directory="${RELEASE_ROOT}/${tag}"
  destination="${directory}/subrouter"
  install -d -m 0755 "${directory}"
  if [[ -e "${destination}" ]]; then
    [[ -f "${destination}" && ! -L "${destination}" ]] \
      || die "release destination is not a regular file: ${destination}"
    actual="$(sha256sum "${destination}" | awk '{print $1}')"
    [[ "${actual}" == "${expected}" ]] \
      || die "retained ${tag} bytes do not match the release checksum"
    log "release ${tag} already retained (${expected})"
    return
  fi
  incoming="${destination}.incoming.$$"
  install -m 0755 -o root -g root "${candidate}" "${incoming}"
  actual="$(sha256sum "${incoming}" | awk '{print $1}')"
  if [[ "${actual}" != "${expected}" ]]; then
    rm -f -- "${incoming}"
    die "staged release checksum changed"
  fi
  mv -f -- "${incoming}" "${destination}"
  log "retained release ${tag} (${expected})"
}

write_units() {
  local service_user service_group service_home
  service_user="$(systemctl show "${LEGACY_SERVICE}" -p User --value)"
  service_group="$(systemctl show "${LEGACY_SERVICE}" -p Group --value)"
  service_home="$(systemctl show "${LEGACY_SERVICE}" -p Environment --value \
    | tr ' ' '\n' | sed -n 's/^HOME=//p' | head -1)"
  [[ -n "${service_user}" ]] || die "could not read User from ${LEGACY_SERVICE}"
  [[ -n "${service_group}" ]] || service_group="${service_user}"
  [[ -n "${service_home}" ]] || service_home="${STATE_DIR}"

  install -d -m 0750 -o "${service_user}" -g "${service_group}" \
    "${service_home}" "${STATE_DIR}" "${LOG_DIR}"
  install -d -m 0755 "${SLOT_ROOT}/slot-a" "${SLOT_ROOT}/slot-b" "${FRONT_ROOT}" "${CONTROL_ROOT}"
  install -d -m 0755 "${SLOT_ENV_DIR}"
  printf '%s\n' \
    'SUBROUTER_SLOT_ADDR=127.0.0.1:31417' \
    "SUBROUTER_SLOT_CONTROL_SOCKET=${STATE_DIR}/slot-a.sock" \
    >"${SLOT_ENV_DIR}/slot-a"
  printf '%s\n' \
    'SUBROUTER_SLOT_ADDR=127.0.0.1:31418' \
    "SUBROUTER_SLOT_CONTROL_SOCKET=${STATE_DIR}/slot-b.sock" \
    >"${SLOT_ENV_DIR}/slot-b"
  chmod 0644 "${SLOT_ENV_DIR}/slot-a" "${SLOT_ENV_DIR}/slot-b"
  write_verify_front_address "127.0.0.1:31416"

  local slot_tmp front_tmp
  slot_tmp="$(mktemp "${SLOT_UNIT}.tmp.XXXXXX")"
  front_tmp="$(mktemp "${FRONT_UNIT}.tmp.XXXXXX")"
  cat >"${slot_tmp}" <<UNIT
[Unit]
Description=Subrouter supervisor slot %i
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${service_user}
Group=${service_group}
Environment=HOME=${service_home}
Environment=GOMEMLIMIT=160MiB
EnvironmentFile=-${DEFAULTS_FILE}
EnvironmentFile=${SLOT_ENV_DIR}/%i
ExecStart=${CONTROL_ROOT}/subrouter supervise \\
  --expect-proxy-protocol \\
  --addr \${SUBROUTER_SLOT_ADDR} \\
  --control-socket \${SUBROUTER_SLOT_CONTROL_SOCKET} \\
  --worker-bin ${SLOT_ROOT}/%i/worker \\
  -- --sessions \${SUBROUTER_SESSIONS} \$SUBROUTER_TRANSCRIPT_ARGS \\
  --sr-switch-interval \${SUBROUTER_SR_SWITCH_INTERVAL} \$SUBROUTER_EXTRA_ARGS
Restart=on-failure
RestartSec=2
TimeoutStopSec=infinity
WorkingDirectory=${service_home}
MemoryAccounting=true
MemoryMax=192M
MemorySwapMax=0
TasksMax=256
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=read-only
ReadWritePaths=${service_home} ${STATE_DIR} ${LOG_DIR}

[Install]
WantedBy=multi-user.target
UNIT
  cat >"${front_tmp}" <<UNIT
[Unit]
Description=Subrouter stable connection front
After=network-online.target subrouter-slot@slot-a.service subrouter-slot@slot-b.service
Wants=network-online.target

[Service]
Type=simple
User=${service_user}
Group=${service_group}
Environment=HOME=${service_home}
Environment=GOMEMLIMIT=96MiB
EnvironmentFile=${FRONT_ENV}
ExecStart=${FRONT_ROOT}/subrouter front \\
  --addr \${SUBROUTER_FRONT_ADDR} \\
  --control-socket ${FRONT_SOCKET} \\
  --listener-transfer-socket ${FRONT_TRANSFER_SOCKET} \\
  --backend-id \${SUBROUTER_FRONT_BACKEND_ID} \\
  --backend-network \${SUBROUTER_FRONT_BACKEND_NETWORK} \\
  --backend-address \${SUBROUTER_FRONT_BACKEND_ADDRESS}
ExecReload=/bin/kill -HUP \$MAINPID
Restart=always
RestartSec=2
TimeoutStopSec=infinity
# A draining generation can restore MAINPID if the successor ownership commit fails.
NotifyAccess=all
FileDescriptorStoreMax=2
WorkingDirectory=${service_home}
MemoryAccounting=true
MemoryMax=128M
MemorySwapMax=0
TasksMax=256
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=read-only
ReadWritePaths=${service_home} ${STATE_DIR} ${LOG_DIR}

[Install]
WantedBy=multi-user.target
UNIT
  chmod 0644 "${slot_tmp}" "${front_tmp}"
  mv -f -- "${slot_tmp}" "${SLOT_UNIT}"
  mv -f -- "${front_tmp}" "${FRONT_UNIT}"
  systemctl daemon-reload
}

activate_front_takeover() {
  local source_pid="$1" source_fd="$2" slot source_inode front_pid front_pid_after front_fd front_inode status candidate accepted service_user service_group transfer_unit
  [[ "${source_pid}" =~ ^[0-9]+$ && "${source_fd}" =~ ^[0-9]+$ ]] \
    || die "listener takeover PID and FD must be non-negative integers"
  command -v systemd-run >/dev/null 2>&1 || die "systemd-run is required for listener takeover"
  (( source_pid > 1 )) || die "listener takeover source PID must be greater than one"
  [[ -e "/proc/${source_pid}/fd/${source_fd}" ]] \
    || die "listener takeover source descriptor is absent"
  source_inode="$(readlink "/proc/${source_pid}/fd/${source_fd}")"
  [[ "${source_inode}" =~ ^socket:\[[0-9]+\]$ ]] \
    || die "listener takeover source descriptor is not a socket"
  systemctl is-active --quiet "${LEGACY_SERVICE}" \
    || die "legacy ${LEGACY_SERVICE} must remain active until listener takeover is ready"
  slot="$(front_default_value SUBROUTER_FRONT_BACKEND_ID slot-a)"
  validate_slot "${slot}"
  systemctl is-active --quiet "subrouter-slot@${slot}.service" \
    || die "active front slot ${slot} is not running"

  systemctl is-active --quiet subrouter-front.service \
    || die "front service must remain active during listener takeover"
  [[ -S "${FRONT_SOCKET}" ]] || die "active front service has no control socket"
  [[ -S "${FRONT_TRANSFER_SOCKET}" ]] || die "active front service has no listener transfer socket"
  status="$(curl -fsS --unix-socket "${FRONT_SOCKET}" http://localhost/_subrouter/front-status)"
  [[ "$(jq -r '.active.id // empty' <<<"${status}")" == "${slot}" ]] \
    || die "front active slot changed before listener takeover"
  front_pid="$(systemctl show subrouter-front.service -p MainPID --value)"
  if [[ ! "${front_pid}" =~ ^[0-9]+$ ]] || (( front_pid <= 1 )); then
    die "front listener takeover has no live process"
  fi
  service_user="$(systemctl show subrouter-front.service -p User --value)"
  service_group="$(systemctl show subrouter-front.service -p Group --value)"
  [[ -n "${service_user}" ]] || die "front listener transfer has no service user"
  [[ -n "${service_group}" ]] || service_group="${service_user}"
  transfer_unit="subrouter-listener-transfer-${source_pid}-$$"
  systemd-run --quiet --wait --collect --pipe --unit="${transfer_unit}" \
    --property=Type=exec --property="User=${service_user}" --property="Group=${service_group}" \
    --property=NoNewPrivileges=yes --property=PrivateTmp=yes \
    --property=ProtectSystem=full --property=ProtectHome=read-only \
    --property=CapabilityBoundingSet=CAP_SYS_PTRACE \
    --property=AmbientCapabilities=CAP_SYS_PTRACE \
    --property=RuntimeMaxSec=15s \
    "${CONTROL_ROOT}/subrouter" listener-transfer \
      --socket "${FRONT_TRANSFER_SOCKET}" --address 0.0.0.0:31415 \
      --source-pid "${source_pid}" --source-fd "${source_fd}"
  front_pid_after="$(systemctl show subrouter-front.service -p MainPID --value)"
  [[ "${front_pid_after}" == "${front_pid}" ]] \
    || die "front restarted during live listener takeover"
  ! systemctl is-active --quiet "${transfer_unit}" \
    || die "one-shot listener transfer helper remained active"
  front_fd=""
  for candidate in "/proc/${front_pid}/fd"/*; do
    [[ -e "${candidate}" ]] || continue
    if [[ "$(readlink "${candidate}")" == "${source_inode}" ]]; then
      front_fd="${candidate##*/}"
      break
    fi
  done
  [[ "${front_fd}" =~ ^[0-9]+$ ]] \
    || die "front did not inherit the exact source listener socket"
  front_inode="$(readlink "/proc/${front_pid}/fd/${front_fd}")"
  [[ "${front_inode}" == "${source_inode}" ]] \
    || die "front listener socket identity changed during takeover"

  accepted=0
  for _ in $(seq 1 128); do
    python3 -c 'import socket,sys; socket.create_connection(("127.0.0.1", int(sys.argv[1])), 1).close()' 31415 \
      >/dev/null 2>&1 || true
    status="$(curl -fsS --unix-socket "${FRONT_SOCKET}" http://localhost/_subrouter/front-status)"
    accepted="$(jq -r '.listener.accepted_connections // -1' <<<"${status}")"
    [[ "${accepted}" =~ ^[0-9]+$ ]] || die "front returned an invalid listener acceptance count"
    (( accepted > 0 )) && break
    sleep 0.01
  done
  (( accepted > 0 )) \
    || die "front inherited the listener descriptor but did not accept a connection on it"

  # The running front already owns the descriptor. Persist the stable address
  # so a later restart binds 31415 normally after legacy releases its copy.
  write_front_default "${slot}" "0.0.0.0:31415"
  write_verify_front_address "127.0.0.1:31415"
  systemctl daemon-reload
  log "front inherited ${source_inode} from pid ${source_pid} fd ${source_fd} as pid ${front_pid} fd ${front_fd}"
}

restore_front_bootstrap() {
  local slot payload front_pid
  slot="$(front_default_value SUBROUTER_FRONT_BACKEND_ID slot-a)"
  validate_slot "${slot}"
  front_pid="$(systemctl show subrouter-front.service -p MainPID --value)"
  if [[ ! "${front_pid}" =~ ^[0-9]+$ ]] || (( front_pid <= 1 )); then
    die "front bootstrap recovery has no live process"
  fi
  # Commit the recovery address first. If the process stops before replacing
  # its listener, restart recovery discards the inherited public listener and
  # binds this free bootstrap address while legacy still owns the public port.
  write_front_default "${slot}" "0.0.0.0:31416"
  systemctl daemon-reload
  if ss -H -lntp "sport = :31416" | grep -F "pid=${front_pid}," >/dev/null 2>&1; then
    :
  elif ss -H -lntp "sport = :31415" | grep -F "pid=${front_pid}," >/dev/null 2>&1; then
    payload="$(jq -nc --arg address 0.0.0.0:31416 '{address:$address}')"
    curl -fsS --unix-socket "${FRONT_SOCKET}" -H 'Content-Type: application/json' \
      --data-binary "${payload}" -X POST http://localhost/_subrouter/replace-listener >/dev/null
  else
    die "front owns neither its public nor bootstrap listener"
  fi
  write_verify_front_address "127.0.0.1:31416"
  systemctl daemon-reload
  wait_endpoint "http://127.0.0.1:31416/_subrouter/ready" subrouter-front.service
  listener_status subrouter-front.service 31416 >/dev/null
  log "front restored to bootstrap listener 0.0.0.0:31416"
}

ensure_migration_topology() {
  # The operator-side migration calls this only while its deploy lock is held,
  # the URL map is verified on the legacy backend, and legacy health is green.
  local control_tag="$1" worker_tag="$2" preferred_slot="${3:-slot-a}"
  validate_tag "${control_tag}"
  validate_tag "${worker_tag}"
  validate_slot "${preferred_slot}"
  systemctl is-active --quiet "${LEGACY_SERVICE}" \
    || die "legacy ${LEGACY_SERVICE} must remain active while reconciling migration topology"

  local control_binary worker_binary status active_slot connections front_active=0
  control_binary="$(release_binary "${control_tag}")"
  worker_binary="$(release_binary "${worker_tag}")"
  [[ -x "${control_binary}" ]] || die "retained control release is missing: ${control_binary}"
  [[ -x "${worker_binary}" ]] || die "retained worker release is missing: ${worker_binary}"
  active_slot="${preferred_slot}"

  if systemctl is-active --quiet subrouter-front.service; then
    front_active=1
    [[ -S "${FRONT_SOCKET}" ]] \
      || die "active front service has no control socket; repair it before migration"
    status="$(curl -fsS --unix-socket "${FRONT_SOCKET}" http://localhost/_subrouter/front-status)"
    jq -e '. as $status |
      ($status.active.id == "slot-a" or $status.active.id == "slot-b") and
      ($status.backends|type)=="array" and ($status.backends|length)>0 and
      ([$status.backends[].id] | all(type=="string" and (. == "slot-a" or . == "slot-b"))) and
      (([$status.backends[].id] | unique | length) == ($status.backends | length)) and
      ([$status.backends[] | select(.id == $status.active.id)] | length)==1 and
      ([$status.backends[].connections] | all(type=="number" and . >= 0))' \
      <<<"${status}" >/dev/null || die "front returned invalid topology status"
    active_slot="$(jq -r '.active.id' <<<"${status}")"
    connections="$(jq -r '[.backends[].connections] | add // 0' <<<"${status}")"
    [[ "${connections}" =~ ^[0-9]+$ ]] || die "front returned an invalid connection total"

    (( connections == 0 )) \
      || die "refusing to replace stale front topology with ${connections} pinned connection(s)"
  elif [[ -S "${FRONT_SOCKET}" ]]; then
    local unmanaged_probe_status
    set +e
    curl -sS --max-time 2 --output /dev/null --unix-socket "${FRONT_SOCKET}" \
      http://localhost/_subrouter/front-status >/dev/null 2>&1
    unmanaged_probe_status=$?
    set -e
    case "${unmanaged_probe_status}" in
      7) ;;
      *) die "front control socket is live outside subrouter-front.service or cannot be proven stale (curl exit ${unmanaged_probe_status})" ;;
    esac
  fi

  if (( front_active == 0 )); then
    log "installing migration topology from an absent or interrupted state"
  else
    log "reconciling dormant migration topology to control ${control_tag} and worker ${worker_tag}"
  fi
  systemctl disable --now subrouter-front.service >/dev/null 2>&1 || true
  systemctl is-active --quiet subrouter-front.service \
    && die "front service remained active during dormant topology reconciliation"
  systemctl disable --now subrouter-slot@slot-a.service subrouter-slot@slot-b.service >/dev/null 2>&1 || true
  for slot in slot-a slot-b; do
    systemctl is-active --quiet "subrouter-slot@${slot}.service" \
      && die "${slot} remained active during dormant topology reconciliation"
  done
  systemctl is-active --quiet "${LEGACY_SERVICE}" \
    || die "legacy ${LEGACY_SERVICE} stopped during dormant topology reconciliation"

  local socket
  for socket in "${FRONT_SOCKET}" "${STATE_DIR}/slot-a.sock" "${STATE_DIR}/slot-b.sock"; do
    if [[ -e "${socket}" || -S "${socket}" ]]; then
      [[ -S "${socket}" ]] || die "refusing to remove non-socket topology path: ${socket}"
      rm -f -- "${socket}"
    fi
  done
  install_topology "${control_tag}" "${worker_tag}" "${active_slot}"
  systemctl is-active --quiet "${LEGACY_SERVICE}" \
    || die "legacy ${LEGACY_SERVICE} stopped while installing migration topology"
  log "migration topology reconciled through ${active_slot}; legacy service remained active"
}

install_topology() {
  local control_tag="$1"
  local worker_tag="$2"
  local initial_slot="${3:-slot-a}"
  local mode="${4:-migration}"
  validate_tag "${control_tag}"
  validate_tag "${worker_tag}"
  validate_slot "${initial_slot}"
  [[ ! -S "${FRONT_SOCKET}" ]] || die "front topology is already active"
  if [[ "${mode}" == migration ]]; then
    systemctl is-active --quiet "${LEGACY_SERVICE}" \
      || die "legacy ${LEGACY_SERVICE} must remain active during migration"
  elif [[ "${mode}" != fresh ]]; then
    die "topology installation mode must be migration or fresh"
  elif systemctl is-active --quiet subrouter-front.service || [[ -e "${FRESH_MARKER}.active" ]]; then
    die "fresh front topology is already active"
  fi
  local control_binary worker_binary port
  control_binary="$(release_binary "${control_tag}")"
  worker_binary="$(release_binary "${worker_tag}")"
  [[ -x "${control_binary}" ]] || die "retained control release is missing: ${control_binary}"
  [[ -x "${worker_binary}" ]] || die "retained worker release is missing: ${worker_binary}"
  if [[ "${mode}" == migration ]]; then
    [[ "$(sha256sum "${control_binary}" | awk '{print $1}')" != "$(sha256sum "${worker_binary}" | awk '{print $1}')" ]] \
      || die "migration worker must be a different release from the control binary"
  fi
  "${control_binary}" help 2>&1 | grep -Eq '(^|[[:space:]])front([[:space:]]|$)' \
    || die "${control_tag} does not support subrouter front"
  write_units
  atomic_symlink "${control_binary}" "${CONTROL_ROOT}/subrouter"
  atomic_symlink "${control_binary}" "${FRONT_ROOT}/subrouter"
  atomic_symlink "${worker_binary}" "${SLOT_ROOT}/${initial_slot}/worker"
  write_front_default "${initial_slot}"
  if [[ "${mode}" == fresh ]]; then
    systemctl disable --now "${LEGACY_SERVICE}" >/dev/null 2>&1 || true
    systemctl disable --now "${LEGACY_SERVICE%.service}.socket" >/dev/null 2>&1 || true
    systemctl is-active --quiet "${LEGACY_SERVICE}" \
      && die "legacy ${LEGACY_SERVICE} remained active after fresh topology preparation"
    systemctl is-active --quiet "${LEGACY_SERVICE%.service}.socket" \
      && die "legacy ${LEGACY_SERVICE%.service}.socket remained active after fresh topology preparation"
    local marker_tmp
    marker_tmp="$(mktemp "${FRESH_MARKER}.tmp.XXXXXX")"
    printf '%s\n' "${initial_slot}" >"${marker_tmp}"
    chmod 0600 "${marker_tmp}"
    mv -f -- "${marker_tmp}" "${FRESH_MARKER}"
    log "fresh front topology prepared through ${initial_slot}; services remain stopped until authenticated activation"
    return
  fi
  systemctl enable --now "subrouter-slot@${initial_slot}.service"
  port="$(slot_port "${initial_slot}")"
  wait_slot_endpoint "${port}" "/_subrouter/health" "subrouter-slot@${initial_slot}.service"
  wait_slot_endpoint "${port}" "/_subrouter/ready" "subrouter-slot@${initial_slot}.service"
  systemctl enable --now subrouter-front.service
  wait_endpoint "http://127.0.0.1:31416/_subrouter/health" subrouter-front.service
  wait_endpoint "http://127.0.0.1:31416/_subrouter/ready" subrouter-front.service
  systemctl disable --now subrouter-autoupdate.timer >/dev/null 2>&1 || true
  log "front topology ready on 31416 through ${initial_slot} worker ${worker_tag}; control is ${control_tag}; legacy 31415 is still active"
}

activate_fresh_topology() {
  local slot="${1:-slot-a}" port activation_complete=0
  validate_slot "${slot}"
  [[ -f "${FRESH_MARKER}" && "$(cat "${FRESH_MARKER}")" == "${slot}" ]] \
    || die "fresh topology marker does not select ${slot}"
  [[ -f "${DEFAULTS_FILE}" ]] || die "authenticated Subrouter defaults are missing"
  trap 'status=$?; trap - EXIT; if [[ "${activation_complete}" != 1 ]]; then systemctl disable --now subrouter-front.service >/dev/null 2>&1 || true; systemctl disable --now "subrouter-slot@${slot}.service" >/dev/null 2>&1 || true; fi; exit "${status}"' EXIT
  python3 "${DEPLOYMENT_CONTRACT}" validate-auth-defaults "${DEFAULTS_FILE}"
  systemctl disable --now "${LEGACY_SERVICE}" >/dev/null 2>&1 || true
  systemctl disable --now "${LEGACY_SERVICE%.service}.socket" >/dev/null 2>&1 || true
  systemctl is-active --quiet "${LEGACY_SERVICE}" \
    && die "legacy ${LEGACY_SERVICE} remained active during fresh topology activation"
  systemctl is-active --quiet "${LEGACY_SERVICE%.service}.socket" \
    && die "legacy ${LEGACY_SERVICE%.service}.socket remained active during fresh topology activation"
  systemctl enable --now "subrouter-slot@${slot}.service"
  port="$(slot_port "${slot}")"
  wait_slot_endpoint "${port}" "/_subrouter/health" "subrouter-slot@${slot}.service"
  wait_slot_endpoint "${port}" "/_subrouter/ready" "subrouter-slot@${slot}.service"
  systemctl enable --now subrouter-front.service
  wait_endpoint "http://127.0.0.1:31416/_subrouter/health" subrouter-front.service
  wait_endpoint "http://127.0.0.1:31416/_subrouter/ready" subrouter-front.service
  systemctl disable --now subrouter-autoupdate.timer >/dev/null 2>&1 || true
  mv -f -- "${FRESH_MARKER}" "${FRESH_MARKER}.active"
  activation_complete=1
  trap - EXIT
  log "fresh authenticated front topology active on 31416 through ${slot}"
}

prepare_slot() {
  local slot="$1"
  local tag="$2"
  validate_slot "${slot}"
  validate_tag "${tag}"
  [[ -S "${FRONT_SOCKET}" ]] || die "front topology is not installed"
  if systemctl is-active --quiet "subrouter-slot@${slot}.service"; then
    die "refusing to replace active ${slot}; drain and stop it first"
  fi
  local binary port
  binary="$(release_binary "${tag}")"
  [[ -x "${binary}" ]] || die "retained release is missing: ${binary}"
  atomic_symlink "${binary}" "${SLOT_ROOT}/${slot}/worker"
  systemctl reset-failed "subrouter-slot@${slot}.service" >/dev/null 2>&1 || true
  # Enable the candidate before the front EnvironmentFile can point at it. If
  # the VM reboots after a persisted switch, both the selected slot and front
  # come back without falling through to a stopped original slot.
  systemctl enable --now "subrouter-slot@${slot}.service"
  port="$(slot_port "${slot}")"
  wait_slot_endpoint "${port}" "/_subrouter/health" "subrouter-slot@${slot}.service"
  wait_slot_endpoint "${port}" "/_subrouter/ready" "subrouter-slot@${slot}.service"
  log "${slot} is ready on 127.0.0.1:${port} with ${tag}"
}

retire_slot() {
  local slot="$1"
  validate_slot "${slot}"
  curl -fsS --unix-socket "${STATE_DIR}/${slot}.sock" -X POST \
    http://localhost/_subrouter/retire >/dev/null
  log "${slot} worker retirement requested"
}

stop_drained_slot() {
  local slot="$1"
  validate_slot "${slot}"
  local status active connections socket
  status="$(curl -fsS --unix-socket "${FRONT_SOCKET}" http://localhost/_subrouter/front-status)"
  active="$(jq -r '.active.id // empty' <<<"${status}")"
  [[ "${active}" != "${slot}" ]] || die "refusing to stop active ${slot}"
  connections="$(jq -r --arg id "${slot}" '[.backends[]? | select(.id == $id) | .connections][0] // 0' <<<"${status}")"
  [[ "${connections}" =~ ^[0-9]+$ ]] || die "front returned an invalid connection count"
  (( connections == 0 )) || die "refusing to stop ${slot} with ${connections} pinned connection(s)"
  socket="${STATE_DIR}/${slot}.sock"
  ! systemctl is-active --quiet "subrouter-slot@${slot}.service" \
    || die "refusing to force-stop ${slot}; its retired supervisor has not exited"
  [[ ! -S "${socket}" ]] || die "refusing to finalize ${slot}; its control socket is still present"
  systemctl disable "subrouter-slot@${slot}.service"
  log "disabled absent drained ${slot}"
}

sample_service_rss() {
  local target="$1" run_label="$2" service sentinel result oom_result cg peak total pid rss_kib oom current_oom
  [[ "${run_label}" =~ ^[a-zA-Z0-9._-]+$ ]] || die "invalid RSS sampler run label"
  case "${target}" in
    front) service="subrouter-front.service" ;;
    legacy) service="${LEGACY_SERVICE}" ;;
    slot-a|slot-b) service="subrouter-slot@${target}.service" ;;
    *) die "RSS sampler target must be legacy, front, slot-a, or slot-b" ;;
  esac
  sentinel="/tmp/subrouter-rss-${run_label}-${target}.running"
  result="/tmp/subrouter-rss-${run_label}-${target}.peak"
  oom_result="/tmp/subrouter-rss-${run_label}-${target}.oom"
  [[ -e "${sentinel}" ]] || die "RSS sampler sentinel is missing"
  cg="$(systemctl show "${service}" -p ControlGroup --value)"
  [[ -n "${cg}" && -r "/sys/fs/cgroup${cg}/cgroup.procs" ]] \
    || die "cannot read ${service} cgroup"
  peak=0
  oom="$(awk '$1 == "oom_kill" {print $2}' "/sys/fs/cgroup${cg}/memory.events")"
  [[ "${oom}" =~ ^[0-9]+$ ]] || die "cannot read ${service} oom_kill counter"
  while [[ -e "${sentinel}" ]]; do
    [[ -r "/sys/fs/cgroup${cg}/cgroup.procs" ]] || break
    total=0
    while IFS= read -r pid; do
      [[ "${pid}" =~ ^[0-9]+$ && -r "/proc/${pid}/status" ]] || continue
      rss_kib="$(awk '$1 == "VmRSS:" {print $2; exit}' "/proc/${pid}/status")"
      [[ "${rss_kib:-}" =~ ^[0-9]+$ ]] || continue
      total=$((total + rss_kib * 1024))
    done <"/sys/fs/cgroup${cg}/cgroup.procs"
    (( total <= peak )) || peak="${total}"
    printf '%s\n' "${peak}" >"${result}.tmp"
    mv -f -- "${result}.tmp" "${result}"
    if [[ -r "/sys/fs/cgroup${cg}/memory.events" ]]; then
      current_oom="$(awk '$1 == "oom_kill" {print $2}' "/sys/fs/cgroup${cg}/memory.events")"
      [[ "${current_oom}" =~ ^[0-9]+$ ]] && oom="${current_oom}"
    fi
    printf '%s\n' "${oom}" >"${oom_result}.tmp"
    mv -f -- "${oom_result}.tmp" "${oom_result}"
    sleep 0.05
  done
  printf '%s\n' "${peak}" >"${result}.tmp"
  mv -f -- "${result}.tmp" "${result}"
  printf '%s\n' "${oom}" >"${oom_result}.tmp"
  mv -f -- "${oom_result}.tmp" "${oom_result}"
}

enable_slot() {
  local slot="$1"
  validate_slot "${slot}"
  systemctl enable --now "subrouter-slot@${slot}.service"
  local port
  port="$(slot_port "${slot}")"
  wait_slot_endpoint "${port}" "/_subrouter/ready" "subrouter-slot@${slot}.service"
  log "enabled ${slot}"
}

disable_slot() {
  local slot="$1"
  validate_slot "${slot}"
  # Keep the process and every pinned connection alive. This only prevents a
  # stale slot from being selected after reboot once the front default moves.
  systemctl disable "subrouter-slot@${slot}.service"
  log "disabled ${slot} without stopping it"
}

quiesce_legacy_socket() (
  local socket_load_state socket_state newly_masked=0
  socket_load_state="$(systemctl show "${LEGACY_SOCKET_SERVICE}" -p LoadState --value)"
  case "${socket_load_state}" in
    loaded) newly_masked=1 ;;
    masked) ;;
    not-found)
      log "legacy socket unit is absent"
      return
      ;;
    *) die "refusing legacy socket quiescence with ${LEGACY_SOCKET_SERVICE} load state ${socket_load_state:-unknown}" ;;
  esac
  cleanup_runtime_mask() {
    (( newly_masked == 0 )) \
      || systemctl unmask --runtime "${LEGACY_SOCKET_SERVICE}" >/dev/null 2>&1 \
      || true
  }
  systemctl disable "${LEGACY_SOCKET_SERVICE}" >/dev/null
  ! systemctl is-enabled --quiet "${LEGACY_SOCKET_SERVICE}" >/dev/null 2>&1 \
    || die "legacy socket remained enabled before quiescence"
  trap cleanup_runtime_mask EXIT
  if (( newly_masked == 1 )); then
    systemctl mask --runtime "${LEGACY_SOCKET_SERVICE}" >/dev/null
  fi
  socket_state="$(systemctl show "${LEGACY_SOCKET_SERVICE}" -p ActiveState --value)"
  if [[ "${socket_state}" != inactive ]]; then
    systemctl --job-mode=ignore-dependencies stop "${LEGACY_SOCKET_SERVICE}"
  fi
  socket_state="$(systemctl show "${LEGACY_SOCKET_SERVICE}" -p ActiveState --value)"
  [[ "${socket_state}" == inactive ]] \
    || die "legacy socket remained ${socket_state:-unknown} after quiescence"
  systemctl disable "${LEGACY_SOCKET_SERVICE}" >/dev/null
  ! systemctl is-enabled --quiet "${LEGACY_SOCKET_SERVICE}" >/dev/null 2>&1 \
    || die "legacy socket remained enabled after quiescence"
  trap - EXIT
  log "legacy socket is inactive, disabled, and runtime-masked"
)

cleanup_stopped_legacy_control() (
  local legacy_state socket_state legacy_load_state socket_load_state socket_owners
  local socket_unit_present=0
  local -a newly_masked_units
  [[ "${LEGACY_CONTROL_SOCKET}" == /* ]] \
    || die "legacy control socket path must be absolute"
  legacy_load_state="$(systemctl show "${LEGACY_SERVICE}" -p LoadState --value)"
  socket_load_state="$(systemctl show "${LEGACY_SOCKET_SERVICE}" -p LoadState --value)"
  newly_masked_units=()
  case "${legacy_load_state}" in
    loaded) newly_masked_units+=("${LEGACY_SERVICE}") ;;
    masked) ;;
    *) die "refusing legacy control cleanup with ${LEGACY_SERVICE} load state ${legacy_load_state:-unknown}" ;;
  esac
  case "${socket_load_state}" in
    loaded)
      socket_unit_present=1
      newly_masked_units+=("${LEGACY_SOCKET_SERVICE}")
      ;;
    masked) socket_unit_present=1 ;;
    not-found) ;;
    *) die "refusing legacy control cleanup with ${LEGACY_SOCKET_SERVICE} load state ${socket_load_state:-unknown}" ;;
  esac
  disable_legacy_units() {
    systemctl disable "${LEGACY_SERVICE}" >/dev/null
    if (( socket_unit_present == 1 )); then
      systemctl disable "${LEGACY_SOCKET_SERVICE}" >/dev/null
    fi
  }
  disable_legacy_units
  assert_legacy_units_disabled() {
    ! systemctl is-enabled --quiet "${LEGACY_SERVICE}" >/dev/null 2>&1 \
      || die "legacy service remained enabled during control socket cleanup"
    if (( socket_unit_present == 1 )); then
      ! systemctl is-enabled --quiet "${LEGACY_SOCKET_SERVICE}" >/dev/null 2>&1 \
        || die "legacy socket remained enabled during control socket cleanup"
    fi
  }
  assert_legacy_units_disabled
  cleanup_runtime_masks() {
    (( ${#newly_masked_units[@]} == 0 )) \
      || systemctl unmask --runtime "${newly_masked_units[@]}" >/dev/null 2>&1 \
      || true
  }
  assert_legacy_units_inactive() {
    legacy_state="$(systemctl show "${LEGACY_SERVICE}" -p ActiveState --value)"
    [[ "${legacy_state}" == inactive ]] \
      || die "refusing legacy control cleanup while ${LEGACY_SERVICE} is ${legacy_state:-unknown}"
    if (( socket_unit_present == 1 )); then
      socket_state="$(systemctl show "${LEGACY_SOCKET_SERVICE}" -p ActiveState --value)"
      [[ "${socket_state}" == inactive ]] \
        || die "refusing legacy control cleanup while ${LEGACY_SOCKET_SERVICE} is ${socket_state:-unknown}"
    fi
  }
  trap cleanup_runtime_masks EXIT
  if (( ${#newly_masked_units[@]} > 0 )); then
    systemctl mask --runtime "${newly_masked_units[@]}" >/dev/null
  fi
  assert_legacy_units_inactive
  if [[ ! -e "${LEGACY_CONTROL_SOCKET}" && ! -L "${LEGACY_CONTROL_SOCKET}" ]]; then
    assert_legacy_units_inactive
    disable_legacy_units
    assert_legacy_units_disabled
    trap - EXIT
    log "stopped legacy control socket is already absent; legacy units remain runtime-masked"
    return
  fi
  [[ -S "${LEGACY_CONTROL_SOCKET}" && ! -L "${LEGACY_CONTROL_SOCKET}" ]] \
    || die "legacy control socket path is not a non-symlink socket"
  command -v ss >/dev/null 2>&1 || die "ss is required for legacy control socket ownership inspection"
  assert_legacy_units_inactive
  socket_owners="$(ss -H -xlpn src "${LEGACY_CONTROL_SOCKET}")"
  [[ -z "${socket_owners}" ]] \
    || die "refusing to remove a kernel-owned legacy control socket"
  assert_legacy_units_inactive
  rm -f -- "${LEGACY_CONTROL_SOCKET}"
  [[ ! -e "${LEGACY_CONTROL_SOCKET}" && ! -L "${LEGACY_CONTROL_SOCKET}" ]] \
    || die "stopped legacy control socket remained after cleanup"
  assert_legacy_units_inactive
  disable_legacy_units
  assert_legacy_units_disabled
  trap - EXIT
  log "removed stopped legacy control socket; legacy units remain runtime-masked"
)

listener_status() {
  local service="$1" port="$2" pid line fd inode
  command -v ss >/dev/null 2>&1 || die "ss is required for listener ownership inspection"
  [[ "${service}" =~ ^[a-zA-Z0-9@._-]+\.service$ ]] || die "listener service name is invalid"
  [[ "${port}" =~ ^[0-9]+$ ]] || die "listener port must be numeric"
  pid="$(systemctl show "${service}" -p MainPID --value)"
  if [[ ! "${pid}" =~ ^[0-9]+$ ]] || (( pid <= 1 )); then
    die "${service} has no live main process"
  fi
  line="$(ss -H -lntp "sport = :${port}" | grep -F "pid=${pid}," | head -n 1)"
  [[ -n "${line}" ]] || die "${service} does not own a listening socket on port ${port}"
  fd="$(sed -n "s/.*pid=${pid},fd=\([0-9][0-9]*\).*/\1/p" <<<"${line}")"
  [[ "${fd}" =~ ^[0-9]+$ ]] || die "could not identify ${service} listener descriptor"
  inode="$(readlink "/proc/${pid}/fd/${fd}")"
  [[ "${inode}" =~ ^socket:\[[0-9]+\]$ ]] || die "${service} listener descriptor is not a socket"
  jq -nc --arg service "${service}" --argjson pid "${pid}" --argjson fd "${fd}" \
    --arg inode "${inode}" --argjson port "${port}" \
    '{service:$service,pid:$pid,fd:$fd,inode:$inode,port:$port}'
}

case "${1:-}" in
  install-release)
    [[ "$#" == 4 ]] || die "usage: $0 install-release <tag> <candidate> <sha256>"
    install_release "$2" "$3" "$4"
    ;;
  install-topology)
    [[ "$#" == 3 || "$#" == 4 ]] || die "usage: $0 install-topology <control-tag> <worker-tag> [slot-a|slot-b]"
    install_topology "$2" "$3" "${4:-slot-a}"
    ;;
  ensure-migration-topology)
    [[ "$#" == 3 || "$#" == 4 ]] || die "usage: $0 ensure-migration-topology <control-tag> <worker-tag> [slot-a|slot-b]"
    ensure_migration_topology "$2" "$3" "${4:-slot-a}"
    ;;
  prepare-fresh-topology)
    [[ "$#" == 2 || "$#" == 3 ]] || die "usage: $0 prepare-fresh-topology <tag> [slot-a|slot-b]"
    install_topology "$2" "$2" "${3:-slot-a}" fresh
    ;;
  activate-fresh-topology)
    [[ "$#" == 1 || "$#" == 2 ]] || die "usage: $0 activate-fresh-topology [slot-a|slot-b]"
    activate_fresh_topology "${2:-slot-a}"
    ;;
  prepare-slot)
    [[ "$#" == 3 ]] || die "usage: $0 prepare-slot <slot-a|slot-b> <tag>"
    prepare_slot "$2" "$3"
    ;;
  set-front-default)
    [[ "$#" == 2 ]] || die "usage: $0 set-front-default <slot-a|slot-b>"
    set_front_default "$2"
    ;;
  activate-front-takeover)
    [[ "$#" == 3 ]] || die "usage: $0 activate-front-takeover <source-pid> <source-fd>"
    activate_front_takeover "$2" "$3"
    ;;
  restore-front-bootstrap)
    [[ "$#" == 1 ]] || die "usage: $0 restore-front-bootstrap"
    restore_front_bootstrap
    ;;
  configure-verify-front)
    [[ "$#" == 2 ]] || die "usage: $0 configure-verify-front <127.0.0.1:31415|127.0.0.1:31416>"
    write_verify_front_address "$2"
    systemctl daemon-reload
    ;;
  retire-slot)
    [[ "$#" == 2 ]] || die "usage: $0 retire-slot <slot-a|slot-b>"
    retire_slot "$2"
    ;;
  stop-drained-slot)
    [[ "$#" == 2 ]] || die "usage: $0 stop-drained-slot <slot-a|slot-b>"
    stop_drained_slot "$2"
    ;;
  sample-service-rss)
    [[ "$#" == 3 ]] || die "usage: $0 sample-service-rss <legacy|front|slot-a|slot-b> <run-label>"
    sample_service_rss "$2" "$3"
    ;;
  enable-slot)
    [[ "$#" == 2 ]] || die "usage: $0 enable-slot <slot-a|slot-b>"
    enable_slot "$2"
    ;;
  disable-slot)
    [[ "$#" == 2 ]] || die "usage: $0 disable-slot <slot-a|slot-b>"
    disable_slot "$2"
    ;;
  quiesce-legacy-socket)
    [[ "$#" == 1 ]] || die "usage: $0 quiesce-legacy-socket"
    quiesce_legacy_socket
    ;;
  cleanup-stopped-legacy-control)
    [[ "$#" == 1 ]] || die "usage: $0 cleanup-stopped-legacy-control"
    cleanup_stopped_legacy_control
    ;;
  listener-status)
    [[ "$#" == 3 ]] || die "usage: $0 listener-status <service> <port>"
    listener_status "$2" "$3"
    ;;
  *)
    die "usage: $0 {install-release|install-topology|ensure-migration-topology|prepare-fresh-topology|activate-fresh-topology|prepare-slot|set-front-default|activate-front-takeover|restore-front-bootstrap|configure-verify-front|retire-slot|stop-drained-slot|sample-service-rss|enable-slot|disable-slot|quiesce-legacy-socket|cleanup-stopped-legacy-control|listener-status} ..."
    ;;
esac
