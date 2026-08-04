#!/usr/bin/env bash
# Reverse one externally observed slot activation and retire the candidate
# without waiting for its externally held connections to close.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: rollback-slot.sh --activation-evidence PATH [--evidence-json PATH]

Validates slot-activation evidence, atomically restores its old slot, requests
candidate retirement, and returns while candidate connections remain pinned.
Run finalize-slot-retirement.sh with the emitted slot-rollback evidence after
the external golden gate closes those connections.
EOF
}

ACTIVATION_EVIDENCE=""
EVIDENCE_JSON=""
while (( $# > 0 )); do
  case "$1" in
    --activation-evidence) (( $# >= 2 )) || { usage >&2; exit 2; }; ACTIVATION_EVIDENCE="$2"; shift 2 ;;
    --evidence-json) (( $# >= 2 )) || { usage >&2; exit 2; }; EVIDENCE_JSON="$2"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ -n "${ACTIVATION_EVIDENCE}" ]] || { usage >&2; exit 2; }

PROJECT_ID="${SUBROUTER_GCP_PROJECT:?set SUBROUTER_GCP_PROJECT}"
ZONE="${SUBROUTER_GCP_ZONE:?set SUBROUTER_GCP_ZONE}"
INSTANCE="${SUBROUTER_GCP_INSTANCE:?set SUBROUTER_GCP_INSTANCE}"
GCLOUD_BINARY="${GCLOUD_BIN:-gcloud}"
FRONT_SOCKET="${SUBROUTER_FRONT_CONTROL_SOCKET:-/var/lib/subrouter/front.sock}"
STATE_DIR="${SUBROUTER_STATE_DIR:-/var/lib/subrouter}"
DEPLOY_LOCK_FILE="${SUBROUTER_DEPLOY_LOCK_FILE:-/run/lock/subrouter-deploy.lock}"
EXPECTED_CONNECTIONS_OVERRIDE="${SUBROUTER_EXPECTED_RETIRING_CONNECTIONS:-}"
ARTIFACT_DIR="${SUBROUTER_DEPLOY_ARTIFACT_DIR:-${PWD}/artifacts/gcp-slot-rollback}"
RUN_LABEL="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-rollback-$$"
RUN_LABEL="${RUN_LABEL//[^a-zA-Z0-9._-]/-}"
REMOTE_INSTALLER="/tmp/install-front-slots-${RUN_LABEL}.sh"
REMOTE_DEPLOYMENT_CONTRACT="/tmp/deployment-contract-${RUN_LABEL}.py"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/gcp/stream-shell-value.sh
source "${SCRIPT_DIR}/stream-shell-value.sh"
# shellcheck source=deploy/gcp/deploy-lock.sh
source "${SCRIPT_DIR}/deploy-lock.sh"

log() { printf 'gcp-slot-rollback: %s\n' "$*"; }
die() { log "$*" >&2; exit 1; }
INSTALL_FRONT_SLOTS="$(bash "${SCRIPT_DIR}/resolve-release-installer.sh" "${SCRIPT_DIR}/install-front-slots.sh")"
DEPLOYMENT_CONTRACT="$(bash "${SCRIPT_DIR}/resolve-release-contract.sh" "${SCRIPT_DIR}/deployment-contract.py")"
REMOTE_INSTALL_COMMAND="sudo env SUBROUTER_DEPLOYMENT_CONTRACT='${REMOTE_DEPLOYMENT_CONTRACT}' bash '${REMOTE_INSTALLER}'"

for command in "${GCLOUD_BINARY}" jq curl python3 sha256sum; do
  command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done
INSTALL_FRONT_SLOTS_SHA256="$(sha256sum "${INSTALL_FRONT_SLOTS}" | awk '{print $1}')"
DEPLOYMENT_CONTRACT_SHA256="$(sha256sum "${DEPLOYMENT_CONTRACT}" | awk '{print $1}')"
python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect slot-activation "${ACTIVATION_EVIDENCE}" >/dev/null
activation_sha256="$(sha256sum "${ACTIVATION_EVIDENCE}" | awk '{print $1}')"
[[ "$(jq -r '.continuity.configured_original_clients' "${ACTIVATION_EVIDENCE}")" == 4 ]] \
  || die "activation evidence must require exactly four original clients"
EXPECTED_CONNECTIONS="$(jq -r '.continuity.expected_candidate_connections_for_rollback' "${ACTIVATION_EVIDENCE}")"
[[ "${EXPECTED_CONNECTIONS}" == 1 ]] || die "activation evidence must bind exactly one candidate rollback connection"
if [[ -n "${EXPECTED_CONNECTIONS_OVERRIDE}" ]]; then
  [[ "${EXPECTED_CONNECTIONS_OVERRIDE}" =~ ^[0-9]+$ ]] \
    || die "SUBROUTER_EXPECTED_RETIRING_CONNECTIONS must be an integer"
  [[ "${EXPECTED_CONNECTIONS_OVERRIDE}" == "${EXPECTED_CONNECTIONS}" ]] \
    || die "SUBROUTER_EXPECTED_RETIRING_CONNECTIONS cannot weaken activation evidence"
fi
[[ "$(jq -r '.run.project' "${ACTIVATION_EVIDENCE}")" == "${PROJECT_ID}" &&
   "$(jq -r '.run.zone' "${ACTIVATION_EVIDENCE}")" == "${ZONE}" &&
   "$(jq -r '.run.instance' "${ACTIVATION_EVIDENCE}")" == "${INSTANCE}" ]] \
  || die "activation evidence target does not match the current GCP target"

mkdir -p "${ARTIFACT_DIR}"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd)"
EVIDENCE_JSON="${EVIDENCE_JSON:-${SUBROUTER_DEPLOY_EVIDENCE_JSON:-${ARTIFACT_DIR}/result.json}}"
mkdir -p "$(dirname "${EVIDENCE_JSON}")"
EVIDENCE_JSON="$(cd "$(dirname "${EVIDENCE_JSON}")" && pwd)/$(basename "${EVIDENCE_JSON}")"

old_slot="$(jq -r '.slots.before' "${ACTIVATION_EVIDENCE}")"
candidate_slot="$(jq -r '.slots.candidate' "${ACTIVATION_EVIDENCE}")"
old_generation="$(jq -r '.slots.old_generation' "${ACTIVATION_EVIDENCE}")"
candidate_generation="$(jq -r '.slots.candidate_generation' "${ACTIVATION_EVIDENCE}")"
old_checksum="$(jq -r '.checksums.installed_before' "${ACTIVATION_EVIDENCE}")"
candidate_checksum="$(jq -r '.checksums.candidate_installed' "${ACTIVATION_EVIDENCE}")"
release_json="$(jq -c '.release' "${ACTIVATION_EVIDENCE}")"

gcloud_ssh() {
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet --command "$1"
}

gcloud_scp() {
  "${GCLOUD_BINARY}" compute scp "$1" "${INSTANCE}:$2" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet
}

slot_address() {
  case "$1" in
    slot-a) printf '127.0.0.1:31417\n' ;;
    slot-b) printf '127.0.0.1:31418\n' ;;
    *) return 1 ;;
  esac
}

slot_service() { printf 'subrouter-slot@%s.service\n' "$1"; }
slot_socket() { printf '%s/%s.sock\n' "${STATE_DIR}" "$1"; }
utc_now() { python3 -c 'from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))'; }
front_status() { gcloud_ssh "sudo curl -fsS --unix-socket '${FRONT_SOCKET}' http://localhost/_subrouter/front-status"; }
front_active_json() { jq -c '.active | {id,network,address}' < <(stream_shell_value "$1"); }
front_connections() { jq -r --arg id "$2" '[.backends[]? | select(.id == $id) | .connections][0] // 0' < <(stream_shell_value "$1"); }

slot_snapshot() {
  local slot="$1" front="$2" status active_id active_connections inactive_connections service_active front_active
  status="$(gcloud_ssh "sudo curl -fsS --unix-socket '$(slot_socket "${slot}")' http://localhost/_subrouter/supervisor-status")"
  jq -e '(.accepting|type)=="boolean" and (.retiring|type)=="boolean" and (.active.id|type)=="string" and (.backends|type)=="array"' \
    < <(stream_shell_value "${status}") >/dev/null || die "${slot} returned an invalid supervisor status"
  active_id="$(jq -r '.active.id' < <(stream_shell_value "${status}"))"
  active_connections="$(jq -r --arg id "${active_id}" '[.backends[] | select(.id == $id) | .connections][0] // -1' < <(stream_shell_value "${status}"))"
  inactive_connections="$(jq -r --arg id "${active_id}" '[.backends[] | select(.id != $id) | .connections] | add // 0' < <(stream_shell_value "${status}"))"
  service_active="$(gcloud_ssh "if systemctl is-active --quiet '$(slot_service "${slot}")'; then echo true; else echo false; fi" | tail -n 1)"
  front_active="$(jq -r --arg id "${slot}" '[.backends[]? | select(.id == $id) | .active][0] // false' < <(stream_shell_value "${front}"))"
  jq -nc --argjson status "${status}" --argjson service_active "${service_active}" \
    --argjson front_active "${front_active}" --arg active_id "${active_id}" \
    --argjson active_connections "${active_connections}" --argjson inactive_connections "${inactive_connections}" \
    '{present:true,accepting:$status.accepting,retiring:$status.retiring,
      front_active:$front_active,active_generation:$active_id,
      active_connections:$active_connections,inactive_connections:$inactive_connections,
      service_active:$service_active}'
}

service_restarts() { gcloud_ssh "systemctl show '$(slot_service "$1")' -p NRestarts --value" | tail -n 1; }
service_oom_kills() {
  local service
  service="$(slot_service "$1")"
  gcloud_ssh "set -eu; cg=\$(systemctl show '${service}' -p ControlGroup --value); awk '\$1 == \"oom_kill\" {print \$2}' /sys/fs/cgroup\${cg}/memory.events" | tail -n 1
}
front_restarts() { gcloud_ssh "systemctl show subrouter-front.service -p NRestarts --value" | tail -n 1; }
front_oom_kills() {
  gcloud_ssh "set -eu; cg=\$(systemctl show subrouter-front.service -p ControlGroup --value); awk '\$1 == \"oom_kill\" {print \$2}' /sys/fs/cgroup\${cg}/memory.events" | tail -n 1
}
service_memory_max() { gcloud_ssh "systemctl show '$(slot_service "$1")' -p MemoryMax --value" | tail -n 1; }
front_memory_max() { gcloud_ssh "systemctl show subrouter-front.service -p MemoryMax --value" | tail -n 1; }

declare -A rss_sampler_pids=()
declare -A rss_sampler_sentinels=()
declare -A rss_sampler_results=()
start_rss_sampler() {
  local target="$1" sentinel result
  sentinel="/tmp/subrouter-rss-${RUN_LABEL}-${target}.running"
  result="/tmp/subrouter-rss-${RUN_LABEL}-${target}.peak"
  gcloud_ssh "sudo rm -f '${result}' '${result}.tmp'; sudo touch '${sentinel}'"
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet \
    --command "${REMOTE_INSTALL_COMMAND} sample-service-rss '${target}' '${RUN_LABEL}'" \
    >"${ARTIFACT_DIR}/rss-${target}.log" 2>&1 &
  rss_sampler_pids["${target}"]=$!
  rss_sampler_sentinels["${target}"]="${sentinel}"
  rss_sampler_results["${target}"]="${result}"
  for _ in $(seq 1 100); do
    gcloud_ssh "sudo test -s '${result}'" >/dev/null 2>&1 && return
    kill -0 "${rss_sampler_pids[${target}]}" 2>/dev/null || die "${target} RSS sampler exited"
    sleep 0.05
  done
  die "${target} RSS sampler did not produce a sample"
}
stop_rss_sampler() {
  local target="$1" pid result peak
  pid="${rss_sampler_pids[${target}]}"
  result="${rss_sampler_results[${target}]}"
  gcloud_ssh "sudo rm -f '${rss_sampler_sentinels[${target}]}'"
  wait "${pid}" || die "${target} RSS sampler failed"
  peak="$(gcloud_ssh "sudo cat '${result}'" | tail -n 1)"
  [[ "${peak}" =~ ^[0-9]+$ ]] || die "${target} RSS sampler returned invalid data"
  unset "rss_sampler_pids[${target}]"
  printf '%s\n' "${peak}"
}
stop_all_rss_samplers() {
  local target
  for target in "${!rss_sampler_pids[@]}"; do
    gcloud_ssh "sudo rm -f '${rss_sampler_sentinels[${target}]}'" >/dev/null 2>&1 || true
    wait "${rss_sampler_pids[${target}]}" >/dev/null 2>&1 || true
    unset "rss_sampler_pids[${target}]"
  done
}

lock_holder_pid=""
acquire_lock() {
  subrouter_acquire_deploy_lock "${ARTIFACT_DIR}/deploy-lock.log" \
    "${GCLOUD_BINARY}" "${INSTANCE}" "${PROJECT_ID}" "${ZONE}" "${DEPLOY_LOCK_FILE}" \
    || die "could not acquire ${DEPLOY_LOCK_FILE}"
}

release_lock() {
  subrouter_release_deploy_lock
}

transition_started=0
transition_committed=0
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  stop_all_rss_samplers
  if [[ "${transition_started}" == 1 && "${transition_committed}" == 0 ]]; then
    log "rollback evidence failed after the front transition; preserving the safe restored target" >&2
    status=1
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
before_status="$(front_status)"
[[ "$(jq -r '.active.id' < <(stream_shell_value "${before_status}"))" == "${candidate_slot}" ]] \
  || die "front no longer selects activation candidate ${candidate_slot}"
candidate_before="$(slot_snapshot "${candidate_slot}" "${before_status}")"
old_before="$(slot_snapshot "${old_slot}" "${before_status}")"
jq -e --arg generation "${candidate_generation}" \
  '.accepting and (.retiring|not) and .front_active and .service_active and .active_generation == $generation and .inactive_connections == 0' \
  < <(stream_shell_value "${candidate_before}") >/dev/null || die "candidate cannot be rolled back cleanly"
jq -e --arg generation "${old_generation}" \
  '.accepting and (.retiring|not) and (.front_active|not) and .service_active and .active_generation == $generation and .inactive_connections == 0' \
  < <(stream_shell_value "${old_before}") >/dev/null || die "old slot cannot be restored cleanly"
connections_before="$(front_connections "${before_status}" "${candidate_slot}")"
(( connections_before >= EXPECTED_CONNECTIONS )) \
  || die "candidate has ${connections_before}/${EXPECTED_CONNECTIONS} required externally held connections"

candidate_restarts_before="$(service_restarts "${candidate_slot}")"
candidate_oom_before="$(service_oom_kills "${candidate_slot}")"
old_restarts_before="$(service_restarts "${old_slot}")"
old_oom_before="$(service_oom_kills "${old_slot}")"
front_restarts_before="$(front_restarts)"
front_oom_before="$(front_oom_kills)"
candidate_memory_max="$(service_memory_max "${candidate_slot}")"
old_memory_max="$(service_memory_max "${old_slot}")"
front_memory_max_bytes="$(front_memory_max)"
[[ "${candidate_memory_max}" == 201326592 && "${old_memory_max}" == 201326592 ]] \
  || die "slot MemoryMax must be exactly 192 MiB"
[[ "${front_memory_max_bytes}" == 134217728 ]] || die "front MemoryMax must be exactly 128 MiB"
start_rss_sampler "${candidate_slot}"
start_rss_sampler "${old_slot}"
start_rss_sampler front

rollback_requested_at="$(utc_now)"
transition_started=1
gcloud_ssh "${REMOTE_INSTALL_COMMAND} enable-slot '${old_slot}'"
gcloud_ssh "${REMOTE_INSTALL_COMMAND} set-front-default '${old_slot}'"
old_address="$(slot_address "${old_slot}")"
payload="$(jq -nc --arg id "${old_slot}" --arg address "${old_address}" '{id:$id,network:"tcp",address:$address}')"
gcloud_ssh "sudo curl -fsS --unix-socket '${FRONT_SOCKET}' -X POST -H 'Content-Type: application/json' --data '${payload}' http://localhost/_subrouter/switch >/dev/null"
rollback_activated_at="$(utc_now)"
gcloud_ssh "${REMOTE_INSTALL_COMMAND} disable-slot '${candidate_slot}'"
retirement_requested_at="$(utc_now)"
gcloud_ssh "${REMOTE_INSTALL_COMMAND} retire-slot '${candidate_slot}'"
transition_committed=1

after_status="$(front_status)"
[[ "$(jq -r '.active.id' < <(stream_shell_value "${after_status}"))" == "${old_slot}" ]] || die "front did not restore ${old_slot}"
connections_after="$(front_connections "${after_status}" "${candidate_slot}")"
(( connections_after >= EXPECTED_CONNECTIONS )) || die "rollback cut candidate connections"
candidate_after="$(slot_snapshot "${candidate_slot}" "${after_status}")"
jq -e --arg generation "${candidate_generation}" \
  '(.accepting|not) and .retiring and (.front_active|not) and .service_active and .active_generation == $generation and .inactive_connections == 0' \
  < <(stream_shell_value "${candidate_after}") >/dev/null || die "candidate did not enter retirement"

candidate_restarts_after="$(service_restarts "${candidate_slot}")"
candidate_oom_after="$(service_oom_kills "${candidate_slot}")"
old_restarts_after="$(service_restarts "${old_slot}")"
old_oom_after="$(service_oom_kills "${old_slot}")"
front_restarts_after="$(front_restarts)"
front_oom_after="$(front_oom_kills)"
[[ "${candidate_restarts_after}" == "${candidate_restarts_before}" && "${candidate_oom_after}" == "${candidate_oom_before}" ]] || die "candidate restarted or OOM-killed during rollback"
[[ "${old_restarts_after}" == "${old_restarts_before}" && "${old_oom_after}" == "${old_oom_before}" ]] || die "old slot restarted or OOM-killed during rollback"
[[ "${front_restarts_after}" == "${front_restarts_before}" && "${front_oom_after}" == "${front_oom_before}" ]] || die "front restarted or OOM-killed during rollback"
candidate_peak_rss="$(stop_rss_sampler "${candidate_slot}")"
old_peak_rss="$(stop_rss_sampler "${old_slot}")"
front_peak_rss="$(stop_rss_sampler front)"
(( candidate_peak_rss <= candidate_memory_max )) || die "candidate rollback peak RSS exceeded MemoryMax"
(( old_peak_rss <= old_memory_max )) || die "restored slot rollback peak RSS exceeded MemoryMax"
(( front_peak_rss <= front_memory_max_bytes )) || die "front rollback peak RSS exceeded MemoryMax"
restored_checksum="$(gcloud_ssh "sudo sha256sum '/opt/subrouter/slots/${old_slot}/worker' | awk '{print \$1}'" | tail -n 1)"
[[ "${restored_checksum}" == "${old_checksum}" ]] || die "rollback restored unexpected bytes"
evidence_emitted_at="$(utc_now)"

evidence_tmp="$(mktemp "${EVIDENCE_JSON}.tmp.XXXXXX")"
jq -n --arg schema 'subrouter.gcp.deploy-evidence/v1' --arg evidence_type slot-rollback \
  --arg mode rollback-rehearsal --arg activation_sha "${activation_sha256}" \
  --arg run_id "${RUN_LABEL}" --arg project "${PROJECT_ID}" --arg zone "${ZONE}" --arg instance "${INSTANCE}" \
  --argjson release "${release_json}" --arg from "${candidate_slot}" --arg to "${old_slot}" \
  --arg from_generation "${candidate_generation}" --arg to_generation "${old_generation}" \
  --arg candidate_checksum "${candidate_checksum}" --arg restored_checksum "${restored_checksum}" \
  --arg requested_at "${rollback_requested_at}" --arg activated_at "${rollback_activated_at}" \
  --arg retirement_requested_at "${retirement_requested_at}" --arg emitted_at "${evidence_emitted_at}" \
  --argjson front_before "$(front_active_json "${before_status}")" --argjson front_after "$(front_active_json "${after_status}")" \
  --argjson retiring_before "${candidate_before}" --argjson retiring_after "${candidate_after}" \
  --argjson expected_connections "${EXPECTED_CONNECTIONS}" --argjson connections_before "${connections_before}" \
  --argjson connections_after "${connections_after}" \
  --argjson crb "${candidate_restarts_before}" --argjson cra "${candidate_restarts_after}" \
  --argjson cob "${candidate_oom_before}" --argjson coa "${candidate_oom_after}" \
  --argjson orb "${old_restarts_before}" --argjson ora "${old_restarts_after}" \
  --argjson oob "${old_oom_before}" --argjson ooa "${old_oom_after}" \
  --argjson frb "${front_restarts_before}" --argjson fra "${front_restarts_after}" \
  --argjson fob "${front_oom_before}" --argjson foa "${front_oom_after}" \
  --argjson candidate_peak "${candidate_peak_rss}" --argjson old_peak "${old_peak_rss}" \
  --argjson front_peak "${front_peak_rss}" --argjson slot_limit "${candidate_memory_max}" \
  --argjson front_limit "${front_memory_max_bytes}" \
  '{schema:$schema,evidence_type:$evidence_type,mode:$mode,success:true,
    activation_evidence_sha256:$activation_sha,
    run:{id:$run_id,project:$project,zone:$zone,instance:$instance},release:$release,
    slots:{from:$from,to:$to,final:$to,from_generation:$from_generation,to_generation:$to_generation},
    checksums:{candidate:$candidate_checksum,restored:$restored_checksum},
    timestamps:{rollback_requested_at:$requested_at,activated_at:$activated_at,
      retirement_requested_at:$retirement_requested_at,evidence_emitted_at:$emitted_at},
    front:{active_before:$front_before,active_after:$front_after},
    retiring_slot:{before:$retiring_before,after:$retiring_after},
    connections:{expected_external:$expected_connections,before:$connections_before,after:$connections_after},
    metrics:{retiring_slot:{nrestarts:{before:$crb,after:$cra},oom_kill:{before:$cob,after:$coa},
        run_scoped_peak_rss_bytes:$candidate_peak,memory_max_bytes:$slot_limit},
      restored_slot:{nrestarts:{before:$orb,after:$ora},oom_kill:{before:$oob,after:$ooa},
        run_scoped_peak_rss_bytes:$old_peak,memory_max_bytes:$slot_limit},
      front:{nrestarts:{before:$frb,after:$fra},oom_kill:{before:$fob,after:$foa},
        run_scoped_peak_rss_bytes:$front_peak,memory_max_bytes:$front_limit}},
    rollback:{performed:true,from:$from,to:$to,requested_at:$requested_at,activated_at:$activated_at},
    retirement:{target:$from,requested_at:$retirement_requested_at,state:"pending",evidence_file_required:true}}' \
  >"${evidence_tmp}"
python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect slot-rollback "${evidence_tmp}" >/dev/null
chmod 0600 "${evidence_tmp}"
mv -f -- "${evidence_tmp}" "${EVIDENCE_JSON}"
log "rollback passed: ${candidate_slot} -> ${old_slot}; candidate originals remain live"
jq -c . "${EVIDENCE_JSON}"
