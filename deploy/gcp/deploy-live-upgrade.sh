#!/usr/bin/env bash
# Deploy one Linux binary through the stable supervisor while real Codex
# WebSocket and HTTP sessions are in flight. The gate fails and rolls back when
# an old stream is cut, a saved thread cannot resume, systemd restarts, or the
# service records an OOM kill.
set -euo pipefail

PROJECT_ID="${SUBROUTER_GCP_PROJECT:?set SUBROUTER_GCP_PROJECT}"
ZONE="${SUBROUTER_GCP_ZONE:?set SUBROUTER_GCP_ZONE}"
INSTANCE="${SUBROUTER_GCP_INSTANCE:?set SUBROUTER_GCP_INSTANCE}"
DEPLOY_BINARY="${SUBROUTER_DEPLOY_BINARY:?set SUBROUTER_DEPLOY_BINARY}"
CLIENT_BINARY="${SUBROUTER_CLIENT_BINARY:-${DEPLOY_BINARY}}"
TRANSPORT_OBSERVER="${SUBROUTER_TRANSPORT_OBSERVER:?set SUBROUTER_TRANSPORT_OBSERVER}"
CODEX_BINARY="${SUBROUTER_CODEX_BIN:-codex}"
GCLOUD_BINARY="${GCLOUD_BIN:-gcloud}"
SERVICE="${SUBROUTER_SERVICE:-subrouter.service}"
CONTROL_SOCKET="${SUBROUTER_CONTROL_SOCKET:-/var/lib/subrouter/supervisor.sock}"
REMOTE_BINARY="${SUBROUTER_REMOTE_BINARY:-/usr/local/bin/subrouter}"
CLIENT_COUNT="${SUBROUTER_LIVE_CLIENTS:-4}"
MIN_DRAINED_CLIENTS="${SUBROUTER_MIN_DRAINED_CLIENTS:-2}"
CODEX_MODEL="${SUBROUTER_CODEX_MODEL:-gpt-5.6-sol}"
MAX_MEMORY_BYTES="${SUBROUTER_MAX_MEMORY_BYTES:-201326592}"
PUBLIC_HEALTH_URL="${SUBROUTER_PUBLIC_HEALTH_URL:?set SUBROUTER_PUBLIC_HEALTH_URL}"
DEPLOY_CLIENT_BASE_URL="${SUBROUTER_DEPLOY_CLIENT_BASE_URL:?set SUBROUTER_DEPLOY_CLIENT_BASE_URL}"
DEPLOY_TENANT_KEY="${SUBROUTER_DEPLOY_TENANT_KEY:?set SUBROUTER_DEPLOY_TENANT_KEY}"
DEPLOY_REVISION="${SUBROUTER_DEPLOY_REVISION:-}"
DEPLOY_MODE="${SUBROUTER_DEPLOY_MODE:-deploy}"
ARTIFACT_DIR="${SUBROUTER_DEPLOY_ARTIFACT_DIR:-${PWD}/artifacts/gcp-live-upgrade}"
RUN_LABEL="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$"
RUN_LABEL="${RUN_LABEL//[^a-zA-Z0-9._-]/-}"
REMOTE_CANDIDATE="/tmp/subrouter-${RUN_LABEL}"
REMOTE_BACKUP="${REMOTE_BINARY}.rollback-${RUN_LABEL}"

log() { printf 'gcp-live-upgrade: %s\n' "$*"; }
die() { log "$*" >&2; exit 1; }

for command in "${GCLOUD_BINARY}" "${CODEX_BINARY}" jq curl python3 pgrep lsof ps sha256sum; do
  command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done
[[ -x "${DEPLOY_BINARY}" ]] || die "deploy binary is not executable: ${DEPLOY_BINARY}"
[[ -x "${CLIENT_BINARY}" ]] || die "client binary is not executable: ${CLIENT_BINARY}"
[[ -x "${TRANSPORT_OBSERVER}" ]] || die "transport observer is not executable: ${TRANSPORT_OBSERVER}"
[[ "${CLIENT_COUNT}" =~ ^[0-9]+$ ]] || die "SUBROUTER_LIVE_CLIENTS must be an integer"
[[ "${MIN_DRAINED_CLIENTS}" =~ ^[0-9]+$ ]] || die "SUBROUTER_MIN_DRAINED_CLIENTS must be an integer"
[[ "${MAX_MEMORY_BYTES}" =~ ^[0-9]+$ ]] || die "SUBROUTER_MAX_MEMORY_BYTES must be an integer"
(( CLIENT_COUNT >= 2 )) || die "SUBROUTER_LIVE_CLIENTS must be at least 2 to cover WebSocket and HTTP"
(( MIN_DRAINED_CLIENTS > 0 )) || die "SUBROUTER_MIN_DRAINED_CLIENTS must be positive"
(( MAX_MEMORY_BYTES > 0 )) || die "SUBROUTER_MAX_MEMORY_BYTES must be positive"
(( CLIENT_COUNT >= MIN_DRAINED_CLIENTS )) || die "live client count must cover the drain minimum"
[[ "${DEPLOY_MODE}" == "deploy" || "${DEPLOY_MODE}" == "rollback-rehearsal" ]] \
  || die "SUBROUTER_DEPLOY_MODE must be deploy or rollback-rehearsal"
[[ "${DEPLOY_CLIENT_BASE_URL}" =~ ^https://[^/?#]+/?$ ]] \
  || die "SUBROUTER_DEPLOY_CLIENT_BASE_URL must be an HTTPS origin"
[[ "${DEPLOY_TENANT_KEY}" =~ ^srt_[0-9a-f]{32,}$ ]] \
  || die "SUBROUTER_DEPLOY_TENANT_KEY is not a valid tenant key"
CLIENT_BASE_URL="${DEPLOY_CLIENT_BASE_URL%/}/t/${DEPLOY_TENANT_KEY}"

mkdir -p "${ARTIFACT_DIR}"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd)"
WORK_DIR="${ARTIFACT_DIR}/work"
mkdir -p "${WORK_DIR}"
curl -fsS --max-time 10 "${PUBLIC_HEALTH_URL}" >/dev/null \
  || die "public health check failed before ${DEPLOY_MODE}: ${PUBLIC_HEALTH_URL}"

gcloud_ssh() {
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
    --project "${PROJECT_ID}" \
    --zone "${ZONE}" \
    --tunnel-through-iap \
    --quiet \
    --command "$1"
}

gcloud_scp() {
  "${GCLOUD_BINARY}" compute scp "$1" "${INSTANCE}:$2" \
    --project "${PROJECT_ID}" \
    --zone "${ZONE}" \
    --tunnel-through-iap \
    --quiet
}

supervisor_status() {
  gcloud_ssh "sudo curl -fsS --unix-socket '${CONTROL_SOCKET}' http://localhost/_subrouter/supervisor-status"
}

service_restarts() {
  gcloud_ssh "systemctl show '${SERVICE}' -p NRestarts --value" | tail -n 1
}

service_oom_kills() {
  gcloud_ssh "set -eu; cg=\$(systemctl show '${SERVICE}' -p ControlGroup --value); grep '^oom_kill ' /sys/fs/cgroup\${cg}/memory.events | cut -d' ' -f2" | tail -n 1
}

service_memory_peak() {
  gcloud_ssh "set -eu; cg=\$(systemctl show '${SERVICE}' -p ControlGroup --value); cat /sys/fs/cgroup\${cg}/memory.peak" | tail -n 1
}

backend_connections() {
  local status="$1"
  local generation="$2"
  jq -r --arg id "${generation}" \
    '[.backends[] | select(.id == $id) | .connections][0] // 0' <<<"${status}"
}

wait_for_generation_connections() {
  local generation="$1"
  local minimum="$2"
  gcloud_ssh "set -eu; i=0; while [ \$i -lt 300 ]; do status=\$(sudo curl -fsS --unix-socket '${CONTROL_SOCKET}' http://localhost/_subrouter/supervisor-status); count=\$(printf '%s' \"\$status\" | jq -r --arg id '${generation}' '[.backends[] | select(.id == \$id) | .connections][0] // 0'); if [ \"\$count\" -ge '${minimum}' ]; then printf '%s\\n' \"\$count\"; exit 0; fi; i=\$((i + 1)); sleep 0.1; done; echo 'timed out waiting for ${minimum} connections on ${generation}' >&2; exit 1" | tail -n 1
}

wait_for_generation_absent() {
  local generation="$1"
  local label="$2"
  gcloud_ssh "set -eu; i=0; while [ \$i -lt 300 ]; do status=\$(sudo curl -fsS --unix-socket '${CONTROL_SOCKET}' http://localhost/_subrouter/supervisor-status); if ! printf '%s' \"\$status\" | jq -e --arg id '${generation}' '.backends[] | select(.id == \$id)' >/dev/null; then exit 0; fi; i=\$((i + 1)); sleep 0.1; done; echo '${label} did not drain' >&2; exit 1"
}

wait_for_active_generation_change() {
  local previous_generation="$1"
  local label="$2"
  gcloud_ssh "set -eu; i=0; while [ \$i -lt 300 ]; do status=\$(sudo curl -fsS --unix-socket '${CONTROL_SOCKET}' http://localhost/_subrouter/supervisor-status); active=\$(printf '%s' \"\$status\" | jq -r '.active.id // empty'); if [ -n \"\$active\" ] && [ \"\$active\" != '${previous_generation}' ]; then printf '%s\\n' \"\$active\"; exit 0; fi; i=\$((i + 1)); sleep 0.1; done; echo 'timed out waiting for ${label} to replace ${previous_generation}' >&2; exit 1" | tail -n 1
}

wait_for_local_endpoint() {
  local endpoint="$1"
  local label="$2"
  for _ in $(seq 1 60); do
    if curl -fsS --max-time 2 "${TUNNEL_BASE_URL}${endpoint}" >/dev/null 2>&1; then
      return 0
    fi
    if [[ -n "${tunnel_pid:-}" ]]; then
      kill -0 "${tunnel_pid}" 2>/dev/null || die "IAP tunnel exited while waiting for ${label}"
    fi
    sleep 0.5
  done
  die "Subrouter ${label} did not pass through the IAP tunnel"
}

wait_for_client_thread_id() {
  local wrapper_pid="$1"
  local log_path="$2"
  local thread_id=""
  for _ in $(seq 1 200); do
    thread_id="$(jq -Rr 'fromjson? | select(.type == "thread.started") | .thread_id' "${log_path}" 2>/dev/null | head -n 1)"
    [[ -n "${thread_id}" ]] && printf '%s\n' "${thread_id}" && return 0
    kill -0 "${wrapper_pid}" 2>/dev/null || return 1
    sleep 0.05
  done
  return 1
}

wait_for_observed_transport() {
  local event_path="$1"
  local expected="$2"
  local observed=""
  for _ in $(seq 1 200); do
    observed="$(jq -Rr --arg expected "${expected}" \
      'fromjson? | select(.transport == $expected and (.path | endswith("/responses"))) | .transport' \
      "${event_path}" 2>/dev/null | head -n 1)"
    [[ "${observed}" == "${expected}" ]] && printf '%s\n' "${observed}" && return 0
    sleep 0.05
  done
  return 1
}

descendant_pids() {
  local root_pid="$1"
  local queue=("${root_pid}")
  local current child
  while (( ${#queue[@]} > 0 )); do
    current="${queue[0]}"
    queue=("${queue[@]:1}")
    printf '%s\n' "${current}"
    while IFS= read -r child; do
      [[ -n "${child}" ]] && queue+=("${child}")
    done < <(pgrep -P "${current}" 2>/dev/null || true)
  done
}

find_stream_owner() {
  local wrapper_pid="$1"
  local destination_port="$2"
  local candidate
  for _ in $(seq 1 200); do
    while IFS= read -r candidate; do
      if kill -0 "${candidate}" 2>/dev/null &&
        lsof -nP -a -p "${candidate}" -iTCP:"${destination_port}" -sTCP:ESTABLISHED 2>/dev/null \
          | grep -q TCP; then
        printf '%s\n' "${candidate}"
        return 0
      fi
    done < <(descendant_pids "${wrapper_pid}")
    kill -0 "${wrapper_pid}" 2>/dev/null || return 1
    sleep 0.05
  done
  return 1
}

wait_for_stopped_state() {
  local pid="$1"
  local state=""
  for _ in $(seq 1 100); do
    state="$(ps -p "${pid}" -o state= 2>/dev/null | tr -d '[:space:]')"
    [[ "${state}" == T* ]] && printf '%s\n' "${state}" && return 0
    sleep 0.01
  done
  return 1
}

free_local_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

local_port="$(free_local_port)"
TUNNEL_BASE_URL="http://127.0.0.1:${local_port}"

client_wrapper_pids=()
client_child_pids=()
client_modes=()
client_homes=()
client_logs=()
client_last_messages=()
client_thread_ids=()
client_observed_transports=()
client_observer_ports=()
client_observer_events=()
observer_pids=()
stopped_pids=()
tunnel_pid=""
deployment_started=0
deployment_committed=0
rollback_completed=0
rollback_failed=0
previous_sum=""
candidate_generation=""

rollback_deployment() {
  local restored_sum restored_status restored_generation
  log "rolling back candidate worker"
  if ! gcloud_ssh "set -euo pipefail; sudo test -f '${REMOTE_BACKUP}'; sudo install -m 0755 -o root -g root '${REMOTE_BACKUP}' '${REMOTE_BINARY}.incoming'; printf '%s  %s\n' '${previous_sum}' '${REMOTE_BINARY}.incoming' | sudo sha256sum -c - >/dev/null; sudo mv -f '${REMOTE_BINARY}.incoming' '${REMOTE_BINARY}'; sudo curl -fsS --unix-socket '${CONTROL_SOCKET}' -X POST http://localhost/_subrouter/upgrade >/dev/null"; then
    return 1
  fi
  restored_sum="$(gcloud_ssh "sudo sha256sum '${REMOTE_BINARY}' | awk '{print \$1}'" | tail -n 1)"
  [[ "${restored_sum}" == "${previous_sum}" ]] || return 1
  restored_generation="$(wait_for_active_generation_change "${candidate_generation}" "restored generation")" \
    || return 1
  restored_status="$(supervisor_status)" || return 1
  [[ -n "${restored_generation}" ]] || return 1
  if [[ -n "${candidate_generation}" && "${restored_generation}" == "${candidate_generation}" ]]; then
    return 1
  fi
  curl -fsS --max-time 5 "${TUNNEL_BASE_URL}/_subrouter/health" >/dev/null || return 1
  curl -fsS --max-time 5 "${TUNNEL_BASE_URL}/_subrouter/ready" >/dev/null || return 1
  deployment_started=0
  rollback_completed=1
  return 0
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  for pid in "${stopped_pids[@]:-}"; do
    kill -CONT "${pid}" 2>/dev/null || true
  done
  if [[ "${deployment_started}" -eq 1 && "${deployment_committed}" -eq 0 && "${rollback_completed}" -eq 0 ]]; then
    if ! rollback_deployment; then
      rollback_failed=1
      status=1
      log "rollback failed; preserving ${REMOTE_BACKUP} and ${REMOTE_CANDIDATE} for recovery" >&2
    fi
  fi
  for pid in "${client_wrapper_pids[@]:-}"; do
    kill -TERM "${pid}" 2>/dev/null || true
  done
  for pid in "${observer_pids[@]:-}"; do
    kill -TERM "${pid}" 2>/dev/null || true
  done
  if [[ -n "${tunnel_pid}" ]]; then
    kill "${tunnel_pid}" 2>/dev/null || true
    wait "${tunnel_pid}" 2>/dev/null || true
  fi
  if [[ "${rollback_failed}" -eq 0 ]]; then
    gcloud_ssh "rm -f '${REMOTE_CANDIDATE}'; sudo rm -f '${REMOTE_BACKUP}'" >/dev/null 2>&1 || true
  fi
  exit "${status}"
}
trap cleanup EXIT INT TERM

log "opening an IAP tunnel to ${INSTANCE}"
"${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
  --project "${PROJECT_ID}" \
  --zone "${ZONE}" \
  --tunnel-through-iap \
  --quiet \
  -- -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 \
  -L "127.0.0.1:${local_port}:127.0.0.1:31415" &
tunnel_pid=$!
wait_for_local_endpoint "/_subrouter/health" "health check"
wait_for_local_endpoint "/_subrouter/ready" "readiness check"

initial_status="$(supervisor_status)"
old_generation="$(jq -r '.active.id // empty' <<<"${initial_status}")"
[[ -n "${old_generation}" ]] || die "supervisor has no active worker generation"
restarts_before="$(service_restarts)"
oom_before="$(service_oom_kills)"
[[ "${restarts_before}" =~ ^[0-9]+$ ]] || die "invalid baseline restart count: ${restarts_before}"
[[ "${oom_before}" =~ ^[0-9]+$ ]] || die "invalid baseline OOM count: ${oom_before}"
gcloud_ssh "test -S '${CONTROL_SOCKET}' && systemctl is-active --quiet '${SERVICE}'" \
  || die "staging service is not an active supervisor"

candidate_sum="$(sha256sum "${DEPLOY_BINARY}" | awk '{print $1}')"
[[ -n "${DEPLOY_REVISION}" ]] || DEPLOY_REVISION="sha256:${candidate_sum}"
previous_sum="$(gcloud_ssh "sudo sha256sum '${REMOTE_BINARY}' | awk '{print \$1}'" | tail -n 1)"
[[ "${previous_sum}" =~ ^[0-9a-f]{64}$ ]] || die "invalid installed binary checksum: ${previous_sum}"
log "uploading candidate ${candidate_sum}"
gcloud_scp "${DEPLOY_BINARY}" "${REMOTE_CANDIDATE}"
gcloud_ssh "printf '%s  %s\\n' '${candidate_sum}' '${REMOTE_CANDIDATE}' | sha256sum -c - >/dev/null"

start_live_client() {
  local index="$1"
  local mode="$2"
  local home="${WORK_DIR}/codex-${index}"
  local log_path="${ARTIFACT_DIR}/client-${index}-${mode}.jsonl"
  local last_path="${ARTIFACT_DIR}/client-${index}-${mode}.last.txt"
  local observer_log="${ARTIFACT_DIR}/observer-${index}-${mode}.log"
  local observer_events="${ARTIFACT_DIR}/observer-${index}-${mode}.jsonl"
  local observer_port observer_pid
  observer_port="$(free_local_port)"
  local marker="CI_STREAM_${index}_COMPLETE"
  local prompt="Do not use tools. Print 600 numbered lines. Prefix every line with CI_STREAM_${index} and finish with exactly ${marker}."
  local extra=()
  if [[ "${mode}" == "http" ]]; then
    extra=(-c 'model_providers.subrouter.supports_websockets=false')
  fi
  mkdir -p "${home}"
  "${TRANSPORT_OBSERVER}" \
    --listen "127.0.0.1:${observer_port}" \
    --upstream "${CLIENT_BASE_URL}" \
    --events "${observer_events}" \
    >"${observer_log}" 2>&1 &
  observer_pid=$!
  observer_pids+=("${observer_pid}")
  client_observer_ports+=("${observer_port}")
  client_observer_events+=("${observer_events}")
  for _ in $(seq 1 100); do
    if curl -fsS --max-time 1 "http://127.0.0.1:${observer_port}/_observer/health" >/dev/null 2>&1; then
      break
    fi
    kill -0 "${observer_pid}" 2>/dev/null \
      || die "transport observer ${index} exited before becoming ready"
    sleep 0.05
  done
  curl -fsS --max-time 1 "http://127.0.0.1:${observer_port}/_observer/health" >/dev/null \
    || die "transport observer ${index} did not become ready"
  (
    env \
      CODEX_HOME="${home}" \
      SUBROUTER_CODEX_BASE_URL="http://127.0.0.1:${observer_port}/v1" \
      SUBROUTER_CODEX_USER_EMAIL="gcp-deploy-ci@manaflow.ai" \
      SUBROUTER_CODEX_BIN="${CODEX_BINARY}" \
      "${CLIENT_BINARY}" codex exec --json --ignore-user-config --ignore-rules \
      --skip-git-repo-check -C "${WORK_DIR}" -s read-only -m "${CODEX_MODEL}" \
      "${extra[@]}" -o "${last_path}" "${prompt}"
  ) >"${log_path}" 2>&1 &
  client_wrapper_pids+=("$!")
  client_modes+=("${mode}")
  client_homes+=("${home}")
  client_logs+=("${log_path}")
  client_last_messages+=("${last_path}")
}

log "starting ${CLIENT_COUNT} real Codex sessions"
for index in $(seq 1 "${CLIENT_COUNT}"); do
  if (( index % 2 == 0 )); then
    start_live_client "${index}" http
  else
    start_live_client "${index}" websocket
  fi
done

wait_for_generation_connections "${old_generation}" "${MIN_DRAINED_CLIENTS}" >/dev/null

: >"${ARTIFACT_DIR}/transport-evidence.jsonl"
: >"${ARTIFACT_DIR}/process-evidence.jsonl"
for index in "${!client_wrapper_pids[@]}"; do
  wrapper_pid="${client_wrapper_pids[${index}]}"
  mode="${client_modes[${index}]}"
  thread_id="$(wait_for_client_thread_id "${wrapper_pid}" "${client_logs[${index}]}")" \
    || die "Codex client $((index + 1)) did not report a thread id before the drain point"
  observed="$(wait_for_observed_transport "${client_observer_events[${index}]}" "${mode}")" \
    || die "Codex client $((index + 1)) did not establish the expected ${mode} transport"
  child_pid="$(find_stream_owner "${wrapper_pid}" "${client_observer_ports[${index}]}")" \
    || die "Codex client $((index + 1)) has no descendant owning an established proxy socket"
  client_thread_ids+=("${thread_id}")
  client_observed_transports+=("${observed}")
  client_child_pids+=("${child_pid}")
  jq -n \
    --argjson client "$((index + 1))" \
    --arg thread_id "${thread_id}" \
    --arg configured "${mode}" \
    --arg observed "${observed}" \
    '{client:$client,thread_id:$thread_id,configured:$configured,observed:$observed}' \
    >>"${ARTIFACT_DIR}/transport-evidence.jsonl"
done
observed_transports_json="$(jq -s '[.[].observed] | unique' "${ARTIFACT_DIR}/transport-evidence.jsonl")"
jq -e 'index("http") != null and index("websocket") != null' \
  <<<"${observed_transports_json}" >/dev/null \
  || die "transport observer evidence did not cover both HTTP and WebSocket"

for index in "${!client_child_pids[@]}"; do
  child_pid="${client_child_pids[${index}]}"
  wrapper_pid="${client_wrapper_pids[${index}]}"
  kill -STOP "${child_pid}"
  stopped_pids+=("${child_pid}")
  process_state="$(wait_for_stopped_state "${child_pid}")" \
    || die "Codex stream owner ${child_pid} did not enter a stopped state"
  lsof -nP -a -p "${child_pid}" -iTCP:"${local_port}" -sTCP:ESTABLISHED 2>/dev/null \
    >/dev/null 2>&1 && die "Codex process ${child_pid} bypassed its transport observer"
  lsof -nP -a -p "${child_pid}" -iTCP:"${client_observer_ports[${index}]}" -sTCP:ESTABLISHED 2>/dev/null \
    | grep -q TCP \
    || die "stopped Codex process ${child_pid} no longer owns its observed proxy socket"
  observer_pid="${observer_pids[${index}]}"
  lsof -nP -a -p "${observer_pid}" -iTCP:443 -sTCP:ESTABLISHED 2>/dev/null \
    | grep -q TCP \
    || die "transport observer ${observer_pid} has no established public proxy socket"
  kill -STOP "${observer_pid}"
  stopped_pids+=("${observer_pid}")
  observer_state="$(wait_for_stopped_state "${observer_pid}")" \
    || die "transport observer ${observer_pid} did not enter a stopped state"
  process_name="$(ps -p "${child_pid}" -o comm= | xargs)"
  jq -n \
    --argjson client "$((index + 1))" \
    --argjson wrapper_pid "${wrapper_pid}" \
    --argjson stream_owner_pid "${child_pid}" \
    --arg state "${process_state}" \
    --arg process "${process_name}" \
    --argjson observer_pid "${observer_pid}" \
    --arg observer_state "${observer_state}" \
    '{client:$client,wrapper_pid:$wrapper_pid,stream_owner_pid:$stream_owner_pid,state:$state,process:$process,socket_state:"ESTABLISHED",observer_pid:$observer_pid,observer_state:$observer_state}' \
    >>"${ARTIFACT_DIR}/process-evidence.jsonl"
done
(( ${#client_child_pids[@]} >= MIN_DRAINED_CLIENTS )) \
  || die "only ${#client_child_pids[@]} Codex clients were still running at the drain point"
sleep 1
stopped_status="$(supervisor_status)"
stopped_connections="$(backend_connections "${stopped_status}" "${old_generation}")"
(( stopped_connections >= MIN_DRAINED_CLIENTS )) \
  || die "old worker retained ${stopped_connections} connections after pausing Codex; want ${MIN_DRAINED_CLIENTS}"

log "installing candidate while ${stopped_connections} Codex sessions are established"
deployment_started=1
gcloud_ssh "set -euo pipefail; sudo cp -p '${REMOTE_BINARY}' '${REMOTE_BACKUP}'; sudo install -m 0755 -o root -g root '${REMOTE_CANDIDATE}' '${REMOTE_BINARY}.incoming'; printf '%s  %s\\n' '${candidate_sum}' '${REMOTE_BINARY}.incoming' | sudo sha256sum -c - >/dev/null; sudo mv -f '${REMOTE_BINARY}.incoming' '${REMOTE_BINARY}'; if ! sudo curl -fsS --unix-socket '${CONTROL_SOCKET}' -X POST http://localhost/_subrouter/upgrade >/dev/null; then sudo install -m 0755 -o root -g root '${REMOTE_BACKUP}' '${REMOTE_BINARY}.incoming'; sudo mv -f '${REMOTE_BINARY}.incoming' '${REMOTE_BINARY}'; sudo curl -fsS --unix-socket '${CONTROL_SOCKET}' -X POST http://localhost/_subrouter/upgrade >/dev/null; exit 1; fi"

candidate_generation="$(wait_for_active_generation_change "${old_generation}" "candidate generation")" \
  || die "supervisor did not activate a new worker generation"
upgraded_status="$(supervisor_status)"
old_after_upgrade="$(backend_connections "${upgraded_status}" "${old_generation}")"
(( old_after_upgrade >= MIN_DRAINED_CLIENTS )) \
  || die "upgrade cut old worker connections: ${old_after_upgrade} remain"
jq -e --arg id "${old_generation}" \
  '.backends[] | select(.id == $id and .active == false)' \
  <<<"${upgraded_status}" >/dev/null \
  || die "old worker was not retained as a draining generation"

deployment_action="deployed"
new_generation="${candidate_generation}"
if [[ "${DEPLOY_MODE}" == "rollback-rehearsal" ]]; then
  rollback_deployment || die "rollback rehearsal failed; backup was preserved for recovery"
  deployment_action="rolled_back"
  restored_status="$(supervisor_status)"
  new_generation="$(jq -r '.active.id // empty' <<<"${restored_status}")"
  [[ -n "${new_generation}" && "${new_generation}" != "${candidate_generation}" ]] \
    || die "rollback rehearsal did not activate a restored worker generation"
  old_after_rollback="$(backend_connections "${restored_status}" "${old_generation}")"
  (( old_after_rollback >= MIN_DRAINED_CLIENTS )) \
    || die "rollback cut old worker connections: ${old_after_rollback} remain"
fi

run_codex_turn() {
  local home="$1"
  local mode="$2"
  local marker="$3"
  local log_path="$4"
  local last_path="$5"
  local extra=()
  if [[ "${mode}" == "http" ]]; then
    extra=(-c 'model_providers.subrouter.supports_websockets=false')
  fi
  env \
    CODEX_HOME="${home}" \
    SUBROUTER_CODEX_BASE_URL="${CLIENT_BASE_URL}/v1" \
    SUBROUTER_CODEX_USER_EMAIL="gcp-deploy-ci@manaflow.ai" \
    SUBROUTER_CODEX_BIN="${CODEX_BINARY}" \
    "${CLIENT_BINARY}" codex exec --json --ignore-user-config --ignore-rules \
    --skip-git-repo-check -C "${WORK_DIR}" -s read-only -m "${CODEX_MODEL}" \
    "${extra[@]}" -o "${last_path}" "Reply exactly ${marker}" \
    >"${log_path}" 2>&1
  grep -Fq "${marker}" "${last_path}" || die "Codex turn did not return ${marker}"
}

log "checking a new Codex session on generation ${new_generation}"
new_home="${WORK_DIR}/codex-new"
mkdir -p "${new_home}"
run_codex_turn "${new_home}" websocket NEW_GENERATION_OK \
  "${ARTIFACT_DIR}/new-generation.jsonl" "${ARTIFACT_DIR}/new-generation.last.txt"

log "resuming the paused Codex streams"
for child_pid in "${stopped_pids[@]}"; do
  kill -CONT "${child_pid}"
done
stopped_pids=()

wait_for_process() {
  local pid="$1"
  local timeout_seconds="$2"
  local waited=0
  while kill -0 "${pid}" 2>/dev/null; do
    if (( waited >= timeout_seconds * 10 )); then
      return 1
    fi
    sleep 0.1
    waited=$((waited + 1))
  done
  wait "${pid}"
}

thread_ids=()
for index in "${!client_wrapper_pids[@]}"; do
  wrapper_pid="${client_wrapper_pids[${index}]}"
  if ! wait_for_process "${wrapper_pid}" 300; then
    tail -n 60 "${client_logs[${index}]}" >&2 || true
    die "Codex client $((index + 1)) did not finish after the upgrade"
  fi
  marker="CI_STREAM_$((index + 1))_COMPLETE"
  grep -Fq "${marker}" "${client_last_messages[${index}]}" \
    || die "Codex client $((index + 1)) lost its completion marker"
  if grep -Eiq 'reconnecting|falling back|timed out|stream disconnected|error sending request' "${client_logs[${index}]}"; then
    tail -n 60 "${client_logs[${index}]}" >&2 || true
    die "Codex client $((index + 1)) reported a transport failure"
  fi
  thread_id="$(jq -Rr 'fromjson? | select(.type == "thread.started") | .thread_id' "${client_logs[${index}]}" | head -n 1)"
  [[ -n "${thread_id}" ]] || die "Codex client $((index + 1)) did not record a thread id"
  thread_ids+=("${thread_id}")
done
client_wrapper_pids=()

log "resuming saved Codex threads through the new worker"
# Each invocation below routes `codex exec resume` through the candidate sr
# wrapper so the continuity check uses the same headers and transport as users.
for index in "${!thread_ids[@]}"; do
  mode="${client_modes[${index}]}"
  extra=()
  if [[ "${mode}" == "http" ]]; then
    extra=(-c 'model_providers.subrouter.supports_websockets=false')
  fi
  marker="RESUME_$((index + 1))_OK"
  resume_log="${ARTIFACT_DIR}/resume-$((index + 1))-${mode}.jsonl"
  resume_last="${ARTIFACT_DIR}/resume-$((index + 1))-${mode}.last.txt"
  env \
    CODEX_HOME="${client_homes[${index}]}" \
    SUBROUTER_CODEX_BASE_URL="${CLIENT_BASE_URL}/v1" \
    SUBROUTER_CODEX_USER_EMAIL="gcp-deploy-ci@manaflow.ai" \
    SUBROUTER_CODEX_BIN="${CODEX_BINARY}" \
    "${CLIENT_BINARY}" codex exec --json --ignore-user-config --ignore-rules \
    --skip-git-repo-check -C "${WORK_DIR}" -s read-only -m "${CODEX_MODEL}" "${extra[@]}" \
    resume -o "${resume_last}" "${thread_ids[${index}]}" \
    "Reply exactly ${marker}" >"${resume_log}" 2>&1
  grep -Fq "${marker}" "${resume_last}" || die "saved thread $((index + 1)) did not resume"
done

wait_for_generation_absent "${old_generation}" "old generation"
if [[ "${DEPLOY_MODE}" == "rollback-rehearsal" ]]; then
  wait_for_generation_absent "${candidate_generation}" "candidate generation after rollback"
fi
gcloud_ssh "systemctl is-active --quiet '${SERVICE}'"
curl -fsS --max-time 5 "${TUNNEL_BASE_URL}/_subrouter/health" >/dev/null \
  || die "GCP health check failed after ${DEPLOY_MODE}"
curl -fsS --max-time 5 "${TUNNEL_BASE_URL}/_subrouter/ready" >/dev/null \
  || die "GCP readiness check failed after ${DEPLOY_MODE}"
restarts_after="$(service_restarts)"
oom_after="$(service_oom_kills)"
memory_peak="$(service_memory_peak)"
[[ "${restarts_after}" == "${restarts_before}" ]] \
  || die "systemd restart count changed: ${restarts_before} -> ${restarts_after}"
[[ "${oom_after}" == "${oom_before}" ]] \
  || die "OOM kill count changed: ${oom_before} -> ${oom_after}"
[[ "${memory_peak}" =~ ^[0-9]+$ ]] || die "invalid service memory peak: ${memory_peak}"
(( memory_peak <= MAX_MEMORY_BYTES )) \
  || die "service memory peak ${memory_peak} exceeds ${MAX_MEMORY_BYTES}"
curl -fsS --max-time 10 "${PUBLIC_HEALTH_URL}" >/dev/null \
  || die "public health check failed: ${PUBLIC_HEALTH_URL}"
installed_sum_after="$(gcloud_ssh "sudo sha256sum '${REMOTE_BINARY}' | awk '{print \$1}'" | tail -n 1)"
expected_installed_sum="${candidate_sum}"
if [[ "${DEPLOY_MODE}" == "rollback-rehearsal" ]]; then
  expected_installed_sum="${previous_sum}"
fi
[[ "${installed_sum_after}" == "${expected_installed_sum}" ]] \
  || die "installed checksum after ${DEPLOY_MODE} is ${installed_sum_after}, want ${expected_installed_sum}"

jq -n \
  --arg action "${deployment_action}" \
  --arg mode "${DEPLOY_MODE}" \
  --arg project "${PROJECT_ID}" \
  --arg zone "${ZONE}" \
  --arg instance "${INSTANCE}" \
  --arg candidate_sha256 "${candidate_sum}" \
  --arg previous_sha256 "${previous_sum}" \
  --arg installed_sha256 "${installed_sum_after}" \
  --arg old_generation "${old_generation}" \
  --arg candidate_generation "${candidate_generation}" \
  --arg new_generation "${new_generation}" \
  --arg deploy_revision "${DEPLOY_REVISION}" \
  --argjson drained_connections "${old_after_upgrade}" \
  --argjson codex_sessions "${CLIENT_COUNT}" \
  --argjson transports "${observed_transports_json}" \
  --argjson restarts "${restarts_after}" \
  --argjson oom_kills "${oom_after}" \
  --argjson memory_peak_bytes "${memory_peak}" \
  '{action:$action,mode:$mode,project:$project,zone:$zone,instance:$instance,deploy_revision:$deploy_revision,candidate_sha256:$candidate_sha256,previous_sha256:$previous_sha256,installed_sha256:$installed_sha256,old_generation:$old_generation,candidate_generation:$candidate_generation,new_generation:$new_generation,drained_connections:$drained_connections,codex_sessions:$codex_sessions,transports:$transports,resumed_threads:$codex_sessions,restarts:$restarts,oom_kills:$oom_kills,memory_peak_bytes:$memory_peak_bytes,rollback_verified:($action == "rolled_back")}' \
  >"${ARTIFACT_DIR}/result.json"

if [[ "${DEPLOY_MODE}" == "deploy" ]]; then
  deployment_committed=1
fi
gcloud_ssh "rm -f '${REMOTE_CANDIDATE}'; sudo rm -f '${REMOTE_BACKUP}'"
log "live ${DEPLOY_MODE} passed: ${old_generation} -> ${new_generation}, ${CLIENT_COUNT} Codex sessions resumed"
