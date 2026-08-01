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
CODEX_BINARY="${SUBROUTER_CODEX_BIN:-codex}"
GCLOUD_BINARY="${GCLOUD_BIN:-gcloud}"
SERVICE="${SUBROUTER_SERVICE:-subrouter.service}"
CONTROL_SOCKET="${SUBROUTER_CONTROL_SOCKET:-/var/lib/subrouter/supervisor.sock}"
REMOTE_BINARY="${SUBROUTER_REMOTE_BINARY:-/usr/local/bin/subrouter}"
CLIENT_COUNT="${SUBROUTER_LIVE_CLIENTS:-4}"
MIN_DRAINED_CLIENTS="${SUBROUTER_MIN_DRAINED_CLIENTS:-2}"
CODEX_MODEL="${SUBROUTER_CODEX_MODEL:-gpt-5.6-sol}"
MAX_MEMORY_BYTES="${SUBROUTER_MAX_MEMORY_BYTES:-201326592}"
PUBLIC_HEALTH_URL="${SUBROUTER_PUBLIC_HEALTH_URL:-}"
DEPLOY_REVISION="${SUBROUTER_DEPLOY_REVISION:-}"
ARTIFACT_DIR="${SUBROUTER_DEPLOY_ARTIFACT_DIR:-${PWD}/artifacts/gcp-live-upgrade}"
RUN_LABEL="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$"
RUN_LABEL="${RUN_LABEL//[^a-zA-Z0-9._-]/-}"
REMOTE_CANDIDATE="/tmp/subrouter-${RUN_LABEL}"
REMOTE_BACKUP="${REMOTE_BINARY}.rollback-${RUN_LABEL}"

log() { printf 'gcp-live-upgrade: %s\n' "$*"; }
die() { log "$*" >&2; exit 1; }

for command in "${GCLOUD_BINARY}" "${CODEX_BINARY}" jq curl python3 pgrep; do
  command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done
[[ -x "${DEPLOY_BINARY}" ]] || die "deploy binary is not executable: ${DEPLOY_BINARY}"
[[ -x "${CLIENT_BINARY}" ]] || die "client binary is not executable: ${CLIENT_BINARY}"
[[ "${CLIENT_COUNT}" =~ ^[0-9]+$ ]] || die "SUBROUTER_LIVE_CLIENTS must be an integer"
[[ "${MIN_DRAINED_CLIENTS}" =~ ^[0-9]+$ ]] || die "SUBROUTER_MIN_DRAINED_CLIENTS must be an integer"
(( CLIENT_COUNT >= MIN_DRAINED_CLIENTS )) || die "live client count must cover the drain minimum"

mkdir -p "${ARTIFACT_DIR}"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd)"
WORK_DIR="${ARTIFACT_DIR}/work"
mkdir -p "${WORK_DIR}"

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

local_port="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"
TUNNEL_BASE_URL="http://127.0.0.1:${local_port}"

client_wrapper_pids=()
client_child_pids=()
client_modes=()
client_homes=()
client_logs=()
client_last_messages=()
stopped_pids=()
tunnel_pid=""
deployment_started=0
deployment_committed=0

rollback_deployment() {
  log "rolling back candidate worker"
  gcloud_ssh "set -euo pipefail; if sudo test -f '${REMOTE_BACKUP}'; then sudo install -m 0755 -o root -g root '${REMOTE_BACKUP}' '${REMOTE_BINARY}.incoming'; sudo mv -f '${REMOTE_BINARY}.incoming' '${REMOTE_BINARY}'; sudo curl -fsS --unix-socket '${CONTROL_SOCKET}' -X POST http://localhost/_subrouter/upgrade >/dev/null; fi" || true
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  for pid in "${stopped_pids[@]:-}"; do
    kill -CONT "${pid}" 2>/dev/null || true
  done
  for pid in "${client_wrapper_pids[@]:-}"; do
    kill -TERM "${pid}" 2>/dev/null || true
  done
  if [[ "${deployment_started}" -eq 1 && "${deployment_committed}" -eq 0 ]]; then
    rollback_deployment
  fi
  if [[ -n "${tunnel_pid}" ]]; then
    kill "${tunnel_pid}" 2>/dev/null || true
    wait "${tunnel_pid}" 2>/dev/null || true
  fi
  gcloud_ssh "rm -f '${REMOTE_CANDIDATE}'; sudo rm -f '${REMOTE_BACKUP}'" >/dev/null 2>&1 || true
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
for _ in $(seq 1 60); do
  if curl -fsS --max-time 2 "${TUNNEL_BASE_URL}/_subrouter/health" >/dev/null 2>&1; then
    break
  fi
  kill -0 "${tunnel_pid}" 2>/dev/null || die "IAP tunnel exited before Subrouter became reachable"
  sleep 0.5
done
curl -fsS --max-time 2 "${TUNNEL_BASE_URL}/_subrouter/health" >/dev/null \
  || die "Subrouter is not reachable through the IAP tunnel"

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
log "uploading candidate ${candidate_sum}"
gcloud_scp "${DEPLOY_BINARY}" "${REMOTE_CANDIDATE}"
gcloud_ssh "printf '%s  %s\\n' '${candidate_sum}' '${REMOTE_CANDIDATE}' | sha256sum -c - >/dev/null"

start_live_client() {
  local index="$1"
  local mode="$2"
  local home="${WORK_DIR}/codex-${index}"
  local log_path="${ARTIFACT_DIR}/client-${index}-${mode}.jsonl"
  local last_path="${ARTIFACT_DIR}/client-${index}-${mode}.last.txt"
  local marker="CI_STREAM_${index}_COMPLETE"
  local prompt="Do not use tools. Print 600 numbered lines. Prefix every line with CI_STREAM_${index} and finish with exactly ${marker}."
  local extra=()
  if [[ "${mode}" == "http" ]]; then
    extra=(-c 'model_providers.subrouter.supports_websockets=false')
  fi
  mkdir -p "${home}"
  (
    env \
      CODEX_HOME="${home}" \
      SUBROUTER_CODEX_BASE_URL="${TUNNEL_BASE_URL}/v1" \
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

for wrapper_pid in "${client_wrapper_pids[@]}"; do
  child_pid=""
  for _ in $(seq 1 100); do
    child_pid="$(pgrep -P "${wrapper_pid}" | head -n 1 || true)"
    [[ -n "${child_pid}" ]] && break
    kill -0 "${wrapper_pid}" 2>/dev/null || break
    sleep 0.05
  done
  client_child_pids+=("${child_pid}")
done

wait_for_generation_connections "${old_generation}" "${MIN_DRAINED_CLIENTS}" >/dev/null
for child_pid in "${client_child_pids[@]}"; do
  if [[ -n "${child_pid}" ]] && kill -0 "${child_pid}" 2>/dev/null; then
    kill -STOP "${child_pid}"
    stopped_pids+=("${child_pid}")
  fi
done
(( ${#stopped_pids[@]} >= MIN_DRAINED_CLIENTS )) \
  || die "only ${#stopped_pids[@]} Codex clients were still running at the drain point"
sleep 1
stopped_status="$(supervisor_status)"
stopped_connections="$(backend_connections "${stopped_status}" "${old_generation}")"
(( stopped_connections >= MIN_DRAINED_CLIENTS )) \
  || die "old worker retained ${stopped_connections} connections after pausing Codex; want ${MIN_DRAINED_CLIENTS}"

log "installing candidate while ${stopped_connections} Codex sessions are established"
deployment_started=1
gcloud_ssh "set -euo pipefail; sudo cp -p '${REMOTE_BINARY}' '${REMOTE_BACKUP}'; sudo install -m 0755 -o root -g root '${REMOTE_CANDIDATE}' '${REMOTE_BINARY}.incoming'; printf '%s  %s\\n' '${candidate_sum}' '${REMOTE_BINARY}.incoming' | sudo sha256sum -c - >/dev/null; sudo mv -f '${REMOTE_BINARY}.incoming' '${REMOTE_BINARY}'; if ! sudo curl -fsS --unix-socket '${CONTROL_SOCKET}' -X POST http://localhost/_subrouter/upgrade >/dev/null; then sudo mv -f '${REMOTE_BACKUP}' '${REMOTE_BINARY}'; sudo curl -fsS --unix-socket '${CONTROL_SOCKET}' -X POST http://localhost/_subrouter/upgrade >/dev/null 2>&1 || true; exit 1; fi"

upgraded_status="$(supervisor_status)"
new_generation="$(jq -r '.active.id // empty' <<<"${upgraded_status}")"
[[ -n "${new_generation}" && "${new_generation}" != "${old_generation}" ]] \
  || die "supervisor did not activate a new worker generation"
old_after_upgrade="$(backend_connections "${upgraded_status}" "${old_generation}")"
(( old_after_upgrade >= MIN_DRAINED_CLIENTS )) \
  || die "upgrade cut old worker connections: ${old_after_upgrade} remain"
jq -e --arg id "${old_generation}" \
  '.backends[] | select(.id == $id and .active == false)' \
  <<<"${upgraded_status}" >/dev/null \
  || die "old worker was not retained as a draining generation"

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
    SUBROUTER_CODEX_BASE_URL="${TUNNEL_BASE_URL}/v1" \
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
    SUBROUTER_CODEX_BASE_URL="${TUNNEL_BASE_URL}/v1" \
    SUBROUTER_CODEX_USER_EMAIL="gcp-deploy-ci@manaflow.ai" \
    SUBROUTER_CODEX_BIN="${CODEX_BINARY}" \
    "${CLIENT_BINARY}" codex exec --json --ignore-user-config --ignore-rules \
    --skip-git-repo-check -m "${CODEX_MODEL}" "${extra[@]}" \
    resume -o "${resume_last}" "${thread_ids[${index}]}" \
    "Reply exactly ${marker}" >"${resume_log}" 2>&1
  grep -Fq "${marker}" "${resume_last}" || die "saved thread $((index + 1)) did not resume"
done

gcloud_ssh "set -eu; i=0; while [ \$i -lt 300 ]; do status=\$(sudo curl -fsS --unix-socket '${CONTROL_SOCKET}' http://localhost/_subrouter/supervisor-status); if ! printf '%s' \"\$status\" | jq -e --arg id '${old_generation}' '.backends[] | select(.id == \$id)' >/dev/null; then exit 0; fi; i=\$((i + 1)); sleep 0.1; done; echo 'old generation did not drain' >&2; exit 1"
gcloud_ssh "systemctl is-active --quiet '${SERVICE}'"
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
if [[ -n "${PUBLIC_HEALTH_URL}" ]]; then
  curl -fsS --max-time 10 "${PUBLIC_HEALTH_URL}" >/dev/null \
    || die "public health check failed: ${PUBLIC_HEALTH_URL}"
fi

jq -n \
  --arg project "${PROJECT_ID}" \
  --arg zone "${ZONE}" \
  --arg instance "${INSTANCE}" \
  --arg candidate_sha256 "${candidate_sum}" \
  --arg old_generation "${old_generation}" \
  --arg new_generation "${new_generation}" \
  --arg deploy_revision "${DEPLOY_REVISION}" \
  --argjson drained_connections "${old_after_upgrade}" \
  --argjson codex_sessions "${CLIENT_COUNT}" \
  --argjson restarts "${restarts_after}" \
  --argjson oom_kills "${oom_after}" \
  --argjson memory_peak_bytes "${memory_peak}" \
  '{project:$project,zone:$zone,instance:$instance,deploy_revision:$deploy_revision,candidate_sha256:$candidate_sha256,old_generation:$old_generation,new_generation:$new_generation,drained_connections:$drained_connections,codex_sessions:$codex_sessions,transports:["websocket","http"],resumed_threads:$codex_sessions,restarts:$restarts,oom_kills:$oom_kills,memory_peak_bytes:$memory_peak_bytes}' \
  >"${ARTIFACT_DIR}/result.json"

deployment_committed=1
gcloud_ssh "rm -f '${REMOTE_CANDIDATE}'; sudo rm -f '${REMOTE_BACKUP}'"
log "live upgrade passed: ${old_generation} -> ${new_generation}, ${CLIENT_COUNT} Codex sessions resumed"
