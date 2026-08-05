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
FRONT_ENV="${SUBROUTER_FRONT_ENV:-/etc/default/subrouter-front}"
FRESH_MARKER="${SUBROUTER_FRESH_TOPOLOGY_MARKER:-${STATE_DIR}/front-topology-prepared}"
DEFAULTS_FILE="${SUBROUTER_DEFAULTS_FILE:-/etc/default/subrouter}"
LEGACY_SERVICE="${SUBROUTER_LEGACY_SERVICE:-subrouter.service}"
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
  local port
  validate_slot "${slot}"
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
  } >"${temporary}"
  chmod 0644 "${temporary}"
  mv -f -- "${temporary}" "${FRONT_ENV}"
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
  if [[ -f "${VERIFY_UNIT}" ]]; then
    install -d -m 0755 "${VERIFY_DROPIN_DIR}"
    cat >"${VERIFY_DROPIN_DIR}/front.conf" <<'UNIT'
[Service]
Environment=SUBROUTER_VERIFY_HEALTH_URL=http://127.0.0.1:31416/_subrouter/health
Environment=SUBROUTER_VERIFY_USAGE_URL=http://127.0.0.1:31416/_subrouter/usage-status
Environment=SUBROUTER_VERIFY_PROXY_URL=http://127.0.0.1:31416/v1/messages
UNIT
    chmod 0644 "${VERIFY_DROPIN_DIR}/front.conf"
  fi

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
  --addr 0.0.0.0:31416 \\
  --control-socket ${FRONT_SOCKET} \\
  --backend-id \${SUBROUTER_FRONT_BACKEND_ID} \\
  --backend-network \${SUBROUTER_FRONT_BACKEND_NETWORK} \\
  --backend-address \${SUBROUTER_FRONT_BACKEND_ADDRESS}
Restart=always
RestartSec=2
TimeoutStopSec=infinity
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

same_release_bytes() {
  local live="$1" desired="$2"
  [[ -x "${live}" && -x "${desired}" ]] || return 1
  [[ "$(sha256sum "${live}" | awk '{print $1}')" == "$(sha256sum "${desired}" | awk '{print $1}')" ]]
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

    if same_release_bytes "${FRONT_ROOT}/subrouter" "${control_binary}" \
        && same_release_bytes "${CONTROL_ROOT}/subrouter" "${control_binary}" \
        && same_release_bytes "${SLOT_ROOT}/${active_slot}/worker" "${worker_binary}" \
        && systemctl is-active --quiet "subrouter-slot@${active_slot}.service" \
        && systemctl is-enabled --quiet subrouter-front.service \
        && systemctl is-enabled --quiet "subrouter-slot@${active_slot}.service" \
        && curl -fsS http://127.0.0.1:31416/_subrouter/health >/dev/null \
        && curl -fsS http://127.0.0.1:31416/_subrouter/ready >/dev/null
    then
      log "migration topology already matches control ${control_tag} and worker ${worker_tag}"
      return
    fi
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
    write_front_default "$2"
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
  *)
    die "usage: $0 {install-release|install-topology|ensure-migration-topology|prepare-fresh-topology|activate-fresh-topology|prepare-slot|set-front-default|retire-slot|stop-drained-slot|sample-service-rss|enable-slot|disable-slot} ..."
    ;;
esac
