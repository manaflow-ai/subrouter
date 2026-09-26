#!/usr/bin/env bash
# Retire and finalize the inactive slot named by a prior activation or rollback.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: finalize-slot-retirement.sh --transition-evidence PATH [--evidence-json PATH]

Validates and hashes the exact prior slot-activation or slot-rollback object.
If activation left the old slot accepting for a possible rehearsal, this
command requests retirement. It waits a bounded drain window, then requires the
retired supervisor and control socket to disappear within 30 seconds of the
last held connection closing. It never force-stops a live supervisor.
EOF
}

TRANSITION_EVIDENCE=""
EVIDENCE_JSON=""
while (( $# > 0 )); do
  case "$1" in
    --transition-evidence) (( $# >= 2 )) || { usage >&2; exit 2; }; TRANSITION_EVIDENCE="$2"; shift 2 ;;
    --evidence-json) (( $# >= 2 )) || { usage >&2; exit 2; }; EVIDENCE_JSON="$2"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ -n "${TRANSITION_EVIDENCE}" ]] || { usage >&2; exit 2; }

PROJECT_ID="${SUBROUTER_GCP_PROJECT:?set SUBROUTER_GCP_PROJECT}"
ZONE="${SUBROUTER_GCP_ZONE:?set SUBROUTER_GCP_ZONE}"
INSTANCE="${SUBROUTER_GCP_INSTANCE:?set SUBROUTER_GCP_INSTANCE}"
GCLOUD_BINARY="${GCLOUD_BIN:-gcloud}"
FRONT_SOCKET="${SUBROUTER_FRONT_CONTROL_SOCKET:-/var/lib/subrouter/front.sock}"
STATE_DIR="${SUBROUTER_STATE_DIR:-/var/lib/subrouter}"
DEPLOY_LOCK_FILE="${SUBROUTER_DEPLOY_LOCK_FILE:-/run/lock/subrouter-deploy.lock}"
DRAIN_TIMEOUT_SECONDS="${SUBROUTER_RETIRE_DRAIN_TIMEOUT_SECONDS:-3600}"
ARTIFACT_DIR="${SUBROUTER_DEPLOY_ARTIFACT_DIR:-${PWD}/artifacts/gcp-slot-retirement}"
RUN_LABEL="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-retire-$$"
RUN_LABEL="${RUN_LABEL//[^a-zA-Z0-9._-]/-}"
REMOTE_INSTALLER="/tmp/install-front-slots-${RUN_LABEL}.sh"
REMOTE_DEPLOYMENT_CONTRACT="/tmp/deployment-contract-${RUN_LABEL}.py"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/gcp/stream-shell-value.sh
source "${SCRIPT_DIR}/stream-shell-value.sh"
# shellcheck source=deploy/gcp/deploy-lock.sh
source "${SCRIPT_DIR}/deploy-lock.sh"

log() { printf 'gcp-slot-retirement: %s\n' "$*"; }
die() { log "$*" >&2; exit 1; }
INSTALL_FRONT_SLOTS="$(bash "${SCRIPT_DIR}/resolve-release-installer.sh" "${SCRIPT_DIR}/install-front-slots.sh")"
DEPLOYMENT_CONTRACT="$(bash "${SCRIPT_DIR}/resolve-release-contract.sh" "${SCRIPT_DIR}/deployment-contract.py")"
REMOTE_INSTALL_COMMAND="sudo env SUBROUTER_DEPLOYMENT_CONTRACT='${REMOTE_DEPLOYMENT_CONTRACT}' bash '${REMOTE_INSTALLER}'"
for command in "${GCLOUD_BINARY}" jq curl python3; do
  command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done
if command -v sha256sum >/dev/null 2>&1; then
  hash_file() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  hash_file() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  die "required command not found: sha256sum or shasum"
fi
INSTALL_FRONT_SLOTS_SHA256="$(hash_file "${INSTALL_FRONT_SLOTS}")"
DEPLOYMENT_CONTRACT_SHA256="$(hash_file "${DEPLOYMENT_CONTRACT}")"
[[ "${DRAIN_TIMEOUT_SECONDS}" =~ ^[0-9]+$ ]] \
  || die "SUBROUTER_RETIRE_DRAIN_TIMEOUT_SECONDS must be an integer"
(( DRAIN_TIMEOUT_SECONDS > 0 )) || die "SUBROUTER_RETIRE_DRAIN_TIMEOUT_SECONDS must be positive"

transition_type="$(jq -r '.evidence_type // empty' "${TRANSITION_EVIDENCE}")"
case "${transition_type}" in
  slot-activation)
    python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect slot-activation "${TRANSITION_EVIDENCE}" >/dev/null
    mode=deploy
    retired_slot="$(jq -r '.slots.before' "${TRANSITION_EVIDENCE}")"
    active_slot="$(jq -r '.slots.candidate' "${TRANSITION_EVIDENCE}")"
    retired_generation="$(jq -r '.slots.old_generation' "${TRANSITION_EVIDENCE}")"
    retired_checksum="$(jq -r '.checksums.installed_before' "${TRANSITION_EVIDENCE}")"
    expected_restarts="$(jq -r '.metrics.old_slot.nrestarts.after' "${TRANSITION_EVIDENCE}")"
    expected_oom="$(jq -r '.metrics.old_slot.oom_kill.after' "${TRANSITION_EVIDENCE}")"
    retirement_requested_at=""
    ;;
  slot-rollback)
    python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect slot-rollback "${TRANSITION_EVIDENCE}" >/dev/null
    mode=rollback-rehearsal
    retired_slot="$(jq -r '.slots.from' "${TRANSITION_EVIDENCE}")"
    active_slot="$(jq -r '.slots.to' "${TRANSITION_EVIDENCE}")"
    retired_generation="$(jq -r '.slots.from_generation' "${TRANSITION_EVIDENCE}")"
    retired_checksum="$(jq -r '.checksums.candidate' "${TRANSITION_EVIDENCE}")"
    expected_restarts="$(jq -r '.metrics.retiring_slot.nrestarts.after' "${TRANSITION_EVIDENCE}")"
    expected_oom="$(jq -r '.metrics.retiring_slot.oom_kill.after' "${TRANSITION_EVIDENCE}")"
    retirement_requested_at="$(jq -r '.retirement.requested_at' "${TRANSITION_EVIDENCE}")"
    ;;
  *) die "transition evidence must be slot-activation or slot-rollback" ;;
esac
transition_sha256="$(hash_file "${TRANSITION_EVIDENCE}")"
[[ "$(jq -r '.run.project' "${TRANSITION_EVIDENCE}")" == "${PROJECT_ID}" &&
   "$(jq -r '.run.zone' "${TRANSITION_EVIDENCE}")" == "${ZONE}" &&
   "$(jq -r '.run.instance' "${TRANSITION_EVIDENCE}")" == "${INSTANCE}" ]] \
  || die "transition evidence target does not match the current GCP target"

mkdir -p "${ARTIFACT_DIR}"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd)"
EVIDENCE_JSON="${EVIDENCE_JSON:-${SUBROUTER_DEPLOY_EVIDENCE_JSON:-${ARTIFACT_DIR}/result.json}}"
mkdir -p "$(dirname "${EVIDENCE_JSON}")"
EVIDENCE_JSON="$(cd "$(dirname "${EVIDENCE_JSON}")" && pwd)/$(basename "${EVIDENCE_JSON}")"

gcloud_ssh() {
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet --command "$1"
}
gcloud_scp() {
  "${SCRIPT_DIR}/gcloud-scp.sh" "${GCLOUD_BINARY}" "$1" "${INSTANCE}:$2" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet
}
slot_service() { printf 'subrouter-slot@%s.service\n' "$1"; }
slot_socket() { printf '%s/%s.sock\n' "${STATE_DIR}" "$1"; }
utc_now() { python3 -c 'from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))'; }
epoch_millis() { python3 -c 'import time; print(time.time_ns() // 1_000_000)'; }
front_status() { gcloud_ssh "sudo curl -fsS --unix-socket '${FRONT_SOCKET}' http://localhost/_subrouter/front-status"; }
front_connections() { jq -r --arg id "$2" '[.backends[]? | select(.id == $id) | .connections][0] // 0' < <(stream_shell_value "$1"); }

supervisor_status() {
  gcloud_ssh "sudo curl -fsS --unix-socket '$(slot_socket "${retired_slot}")' http://localhost/_subrouter/supervisor-status"
}

assert_supervisor_status() {
  local status="$1" accepting="$2" retiring="$3" active_id active_connections inactive_connections
  jq -e '(.accepting|type)=="boolean" and (.retiring|type)=="boolean" and (.active.id|type)=="string" and (.backends|type)=="array" and ([.backends[].connections] | all(type=="number" and . >= 0))' \
    < <(stream_shell_value "${status}") >/dev/null || die "retired slot returned invalid supervisor status"
  active_id="$(jq -r '.active.id' < <(stream_shell_value "${status}"))"
  [[ "${active_id}" == "${retired_generation}" ]] || die "retired generation changed from linked evidence"
  active_connections="$(jq -r --arg id "${active_id}" '[.backends[] | select(.id == $id) | .connections][0] // -1' < <(stream_shell_value "${status}"))"
  inactive_connections="$(jq -r --arg id "${active_id}" '[.backends[] | select(.id != $id) | .connections] | add // 0' < <(stream_shell_value "${status}"))"
  [[ "$(jq -r '.accepting' < <(stream_shell_value "${status}"))" == "${accepting}" && "$(jq -r '.retiring' < <(stream_shell_value "${status}"))" == "${retiring}" ]] \
    || die "retired slot accepting state does not match the transition phase"
  (( inactive_connections == 0 )) || die "inactive supervisor generations still hold connections"
  printf '%s\n' "${active_connections}"
}

service_restarts() { gcloud_ssh "systemctl show '$(slot_service "${retired_slot}")' -p NRestarts --value" | tail -n 1; }
service_oom_kills() {
  local service
  service="$(slot_service "${retired_slot}")"
  gcloud_ssh "set -eu; cg=\$(systemctl show '${service}' -p ControlGroup --value); awk '\$1 == \"oom_kill\" {print \$2}' /sys/fs/cgroup\${cg}/memory.events" | tail -n 1
}

sampler_pid=""
sampler_sentinel="/tmp/subrouter-rss-${RUN_LABEL}-${retired_slot}.running"
sampler_result="/tmp/subrouter-rss-${RUN_LABEL}-${retired_slot}.peak"
sampler_oom_result="/tmp/subrouter-rss-${RUN_LABEL}-${retired_slot}.oom"
start_retirement_sampler() {
  gcloud_ssh "sudo rm -f '${sampler_result}' '${sampler_result}.tmp' '${sampler_oom_result}' '${sampler_oom_result}.tmp'; sudo touch '${sampler_sentinel}'"
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet \
    --command "${REMOTE_INSTALL_COMMAND} sample-service-rss '${retired_slot}' '${RUN_LABEL}'" \
    >"${ARTIFACT_DIR}/rss-${retired_slot}.log" 2>&1 &
  sampler_pid=$!
  for _ in $(seq 1 100); do
    gcloud_ssh "sudo test -s '${sampler_result}' -a -s '${sampler_oom_result}'" >/dev/null 2>&1 && return
    kill -0 "${sampler_pid}" 2>/dev/null || die "retirement RSS sampler exited before its first sample"
    sleep 0.05
  done
  die "retirement RSS sampler did not produce a sample"
}
stop_retirement_sampler() {
  gcloud_ssh "sudo rm -f '${sampler_sentinel}'" >/dev/null 2>&1 || true
  [[ -n "${sampler_pid}" ]] || return 0
  wait "${sampler_pid}" || die "retirement RSS sampler failed"
  sampler_pid=""
  retirement_peak_rss="$(gcloud_ssh "sudo cat '${sampler_result}'" | tail -n 1)"
  retirement_oom_after="$(gcloud_ssh "sudo cat '${sampler_oom_result}'" | tail -n 1)"
  [[ "${retirement_peak_rss}" =~ ^[0-9]+$ && "${retirement_oom_after}" =~ ^[0-9]+$ ]] \
    || die "retirement sampler returned invalid metrics"
}

lock_holder_pid=""
acquire_lock() {
  subrouter_acquire_deploy_lock "${ARTIFACT_DIR}/deploy-lock.log" \
    "${GCLOUD_BINARY}" "${INSTANCE}" "${PROJECT_ID}" "${ZONE}" "${DEPLOY_LOCK_FILE}" "${RUN_LABEL}" \
    || die "could not acquire ${DEPLOY_LOCK_FILE}"
}
release_lock() {
  subrouter_release_deploy_lock
}
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  if [[ -n "${sampler_pid}" ]]; then
    gcloud_ssh "sudo rm -f '${sampler_sentinel}'" >/dev/null 2>&1 || true
    wait "${sampler_pid}" >/dev/null 2>&1 || true
  fi
  gcloud_ssh "rm -f '${REMOTE_INSTALLER}' '${REMOTE_DEPLOYMENT_CONTRACT}'" >/dev/null 2>&1 || true
  release_lock
  exit "${status}"
}
trap cleanup EXIT INT TERM

acquire_lock
gcloud_scp "${INSTALL_FRONT_SLOTS}" "${REMOTE_INSTALLER}"
gcloud_scp "${DEPLOYMENT_CONTRACT}" "${REMOTE_DEPLOYMENT_CONTRACT}"
gcloud_ssh "printf '%s  %s\n%s  %s\n' '${INSTALL_FRONT_SLOTS_SHA256}' '${REMOTE_INSTALLER}' '${DEPLOYMENT_CONTRACT_SHA256}' '${REMOTE_DEPLOYMENT_CONTRACT}' | sha256sum -c - >/dev/null"
initial_front="$(front_status)"
[[ "$(jq -r '.active.id' < <(stream_shell_value "${initial_front}"))" == "${active_slot}" ]] || die "linked active slot is no longer selected"
initial_sum="$(gcloud_ssh "sudo sha256sum '/opt/subrouter/slots/${retired_slot}/worker' | awk '{print \$1}'" | tail -n 1)"
[[ "${initial_sum}" == "${retired_checksum}" ]] || die "retired slot bytes differ from linked evidence"
current_oom="$(service_oom_kills)"
[[ "${current_oom}" == "${expected_oom}" ]] || die "retired service OOM count changed before finalization"
retirement_memory_max="$(gcloud_ssh "systemctl show '$(slot_service "${retired_slot}")' -p MemoryMax --value" | tail -n 1)"
[[ "${retirement_memory_max}" == 201326592 ]] || die "retired slot MemoryMax must be exactly 192 MiB"
start_retirement_sampler

if status="$(supervisor_status 2>/dev/null)"; then
  if [[ "${transition_type}" == slot-activation ]]; then
    assert_supervisor_status "${status}" true false >/dev/null
    retirement_requested_at="$(utc_now)"
    gcloud_ssh "${REMOTE_INSTALL_COMMAND} retire-slot '${retired_slot}'"
    status="$(supervisor_status)"
    assert_supervisor_status "${status}" false true >/dev/null
  else
    assert_supervisor_status "${status}" false true >/dev/null
  fi
else
  [[ "${transition_type}" == slot-rollback ]] \
    || die "old slot disappeared before retirement was requested"
  gcloud_ssh "! systemctl is-active --quiet '$(slot_service "${retired_slot}")' && sudo test ! -S '$(slot_socket "${retired_slot}")'" \
    || die "retired slot status is unavailable but the service is not absent"
fi

deadline=$(( $(date +%s) + DRAIN_TIMEOUT_SECONDS ))
while true; do
  current_front="$(front_status)"
  front_count="$(front_connections "${current_front}" "${retired_slot}")"
  [[ "${front_count}" =~ ^[0-9]+$ ]] || die "front returned invalid retired connection count"
  supervisor_count=0
  if status="$(supervisor_status 2>/dev/null)"; then
    supervisor_count="$(assert_supervisor_status "${status}" false true)"
  else
    gcloud_ssh "! systemctl is-active --quiet '$(slot_service "${retired_slot}")' && sudo test ! -S '$(slot_socket "${retired_slot}")'" \
      || die "retired supervisor status disappeared before service absence"
  fi
  if (( front_count == 0 && supervisor_count == 0 )); then
    last_connection_closed_at="$(utc_now)"
    last_connection_closed_ms="$(epoch_millis)"
    break
  fi
  (( $(date +%s) < deadline )) || die "retired slot did not drain within ${DRAIN_TIMEOUT_SECONDS}s"
  sleep 0.1
done

for _ in $(seq 1 300); do
  if gcloud_ssh "! systemctl is-active --quiet '$(slot_service "${retired_slot}")' && sudo test ! -S '$(slot_socket "${retired_slot}")'"; then
    absent_at="$(utc_now)"
    absent_ms="$(epoch_millis)"
    absence_latency_ms=$((absent_ms - last_connection_closed_ms))
    break
  fi
  sleep 0.1
done
[[ -n "${absence_latency_ms:-}" ]] || die "retired slot remained present 30 seconds after drain"
(( absence_latency_ms >= 0 && absence_latency_ms < 30000 )) || die "retired slot absence was not strictly below 30 seconds"
stop_retirement_sampler
[[ "${retirement_oom_after}" == "${expected_oom}" ]] || die "retired slot was OOM-killed while draining"
(( retirement_peak_rss <= retirement_memory_max )) || die "retired slot run-scoped RSS exceeded MemoryMax"
gcloud_ssh "${REMOTE_INSTALL_COMMAND} stop-drained-slot '${retired_slot}'"

final_front="$(front_status)"
[[ "$(jq -r '.active.id' < <(stream_shell_value "${final_front}"))" == "${active_slot}" ]] || die "active slot changed during retirement"
final_connections="$(front_connections "${final_front}" "${retired_slot}")"
(( final_connections == 0 )) || die "retired front backend regained connections"
final_restarts="$(service_restarts)"
[[ "${final_restarts}" == "${expected_restarts}" ]] || die "retired service restart count changed"
service_result="$(gcloud_ssh "systemctl show '$(slot_service "${retired_slot}")' -p Result --value" | tail -n 1)"
[[ "${service_result}" == success ]] || die "retired service result is ${service_result}, expected success"
enabled_after="$(gcloud_ssh "if systemctl is-enabled --quiet '$(slot_service "${retired_slot}")'; then echo true; else echo false; fi" | tail -n 1)"
[[ "${enabled_after}" == false ]] || die "retired service stayed enabled"
evidence_emitted_at="$(utc_now)"

evidence_tmp="$(mktemp "${EVIDENCE_JSON}.tmp.XXXXXX")"
jq -n --arg schema 'subrouter.gcp.deploy-evidence/v1' --arg evidence_type slot-retirement \
  --arg mode "${mode}" --arg transition_type "${transition_type}" --arg transition_sha "${transition_sha256}" \
  --arg run_id "${RUN_LABEL}" --arg project "${PROJECT_ID}" --arg zone "${ZONE}" --arg instance "${INSTANCE}" \
  --arg retired "${retired_slot}" --arg active "${active_slot}" --arg generation "${retired_generation}" \
  --arg requested_at "${retirement_requested_at}" --arg closed_at "${last_connection_closed_at}" \
  --arg absent_at "${absent_at}" --arg emitted_at "${evidence_emitted_at}" --arg service_result "${service_result}" \
  --argjson latency "${absence_latency_ms}" --argjson final_connections "${final_connections}" \
  --argjson restarts "${expected_restarts}" --argjson oom_before "${expected_oom}" \
  --argjson oom_after "${retirement_oom_after}" --argjson peak_rss "${retirement_peak_rss}" \
  --argjson memory_max "${retirement_memory_max}" \
  '{schema:$schema,evidence_type:$evidence_type,mode:$mode,success:true,
    transition_evidence_type:$transition_type,transition_evidence_sha256:$transition_sha,
    run:{id:$run_id,project:$project,zone:$zone,instance:$instance},
    slots:{retired:$retired,active:$active,retired_generation:$generation},
    front:{active:{id:$active,network:"tcp",address:(if $active=="slot-a" then "127.0.0.1:31417" else "127.0.0.1:31418" end)},
      retired_connections_after:$final_connections},
    retirement:{requested_at:$requested_at,last_connection_closed_at:$closed_at,
      absent_at:$absent_at,absence_latency_ms:$latency,service_active_after:false,
      control_socket_present_after:false,enabled_after:false,service_result:$service_result},
    metrics:{old_slot:{nrestarts:{before:$restarts,after:$restarts},
      oom_kill:{before:$oom_before,after:$oom_after},
      run_scoped_peak_rss_bytes:$peak_rss,memory_max_bytes:$memory_max}},
    evidence_emitted_at:$emitted_at}' >"${evidence_tmp}"
python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect slot-retirement "${evidence_tmp}" >/dev/null
chmod 0600 "${evidence_tmp}"
mv -f -- "${evidence_tmp}" "${EVIDENCE_JSON}"
log "retirement passed: ${retired_slot} absent, ${active_slot} remains active"
jq -c . "${EVIDENCE_JSON}"
