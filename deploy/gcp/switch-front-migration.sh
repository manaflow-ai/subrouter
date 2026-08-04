#!/usr/bin/env bash
# Perform one observable URL-map transition between legacy:31415 and front:31416.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: switch-front-migration.sh --operation rehearsal-cutover|rollback|final-cutover \
  --prior-evidence PATH --destination-proof-request PATH --destination-proof PATH \
  [--evidence-json PATH]

Each invocation performs exactly one URL-map transition and emits one linked,
bounded evidence object. Resource preparation is separate. Existing source
connections must remain correlated to their supervisor generation at return.
EOF
}

OPERATION=""
PRIOR_EVIDENCE=""
EVIDENCE_JSON=""
DESTINATION_PROOF_REQUEST=""
DESTINATION_PROOF=""
while (( $# > 0 )); do
  case "$1" in
    --operation) (( $# >= 2 )) || { usage >&2; exit 2; }; OPERATION="$2"; shift 2 ;;
    --prior-evidence) (( $# >= 2 )) || { usage >&2; exit 2; }; PRIOR_EVIDENCE="$2"; shift 2 ;;
    --evidence-json) (( $# >= 2 )) || { usage >&2; exit 2; }; EVIDENCE_JSON="$2"; shift 2 ;;
    --destination-proof-request) (( $# >= 2 )) || { usage >&2; exit 2; }; DESTINATION_PROOF_REQUEST="$2"; shift 2 ;;
    --destination-proof) (( $# >= 2 )) || { usage >&2; exit 2; }; DESTINATION_PROOF="$2"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ -n "${OPERATION}" && -n "${PRIOR_EVIDENCE}" && -n "${DESTINATION_PROOF_REQUEST}" && -n "${DESTINATION_PROOF}" ]] \
  || { usage >&2; exit 2; }

PROJECT_ID="${SUBROUTER_GCP_PROJECT:?set SUBROUTER_GCP_PROJECT}"
ZONE="${SUBROUTER_GCP_ZONE:?set SUBROUTER_GCP_ZONE}"
INSTANCE="${SUBROUTER_GCP_INSTANCE:?set SUBROUTER_GCP_INSTANCE}"
GCLOUD_BINARY="${GCLOUD_BIN:-gcloud}"
DEPLOY_LOCK_FILE="${SUBROUTER_DEPLOY_LOCK_FILE:-/run/lock/subrouter-deploy.lock}"
EXPECTED_CONNECTIONS_OVERRIDE="${SUBROUTER_EXPECTED_MIGRATION_CONNECTIONS:-}"
ARTIFACT_DIR="${SUBROUTER_DEPLOY_ARTIFACT_DIR:-${PWD}/artifacts/gcp-front-transition}"
RUN_LABEL="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-lb-${OPERATION}-$$"
RUN_LABEL="${RUN_LABEL//[^a-zA-Z0-9._-]/-}"
REMOTE_INSTALLER="/tmp/install-front-slots-${RUN_LABEL}.sh"
REMOTE_DEPLOYMENT_CONTRACT="/tmp/deployment-contract-${RUN_LABEL}.py"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/gcp/stream-shell-value.sh
source "${SCRIPT_DIR}/stream-shell-value.sh"
# shellcheck source=deploy/gcp/deploy-lock.sh
source "${SCRIPT_DIR}/deploy-lock.sh"
LEGACY_RSS_LIMIT_BYTES="${SUBROUTER_LEGACY_RSS_LIMIT_BYTES:-201326592}"

log() { printf 'gcp-front-transition: %s\n' "$*"; }
die() { log "$*" >&2; exit 1; }
INSTALL_FRONT_SLOTS="$(bash "${SCRIPT_DIR}/resolve-release-installer.sh" "${SCRIPT_DIR}/install-front-slots.sh")"
DEPLOYMENT_CONTRACT="$(bash "${SCRIPT_DIR}/resolve-release-contract.sh" "${SCRIPT_DIR}/deployment-contract.py")"
REMOTE_INSTALL_COMMAND="sudo env SUBROUTER_DEPLOYMENT_CONTRACT='${REMOTE_DEPLOYMENT_CONTRACT}' bash '${REMOTE_INSTALLER}'"
for command in "${GCLOUD_BINARY}" jq curl python3 sha256sum; do
  command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done
INSTALL_FRONT_SLOTS_SHA256="$(sha256sum "${INSTALL_FRONT_SLOTS}" | awk '{print $1}')"
DEPLOYMENT_CONTRACT_SHA256="$(sha256sum "${DEPLOYMENT_CONTRACT}" | awk '{print $1}')"
if [[ "${OPERATION}" == rollback ]]; then EXPECTED_CONNECTIONS=1; else EXPECTED_CONNECTIONS=2; fi
if [[ -n "${EXPECTED_CONNECTIONS_OVERRIDE}" ]]; then
  [[ "${EXPECTED_CONNECTIONS_OVERRIDE}" =~ ^[0-9]+$ ]] || die "SUBROUTER_EXPECTED_MIGRATION_CONNECTIONS must be an integer"
  [[ "${EXPECTED_CONNECTIONS_OVERRIDE}" == "${EXPECTED_CONNECTIONS}" ]] \
    || die "SUBROUTER_EXPECTED_MIGRATION_CONNECTIONS cannot weaken the phase-bound minimum"
fi
[[ "${LEGACY_RSS_LIMIT_BYTES}" == 201326592 ]] || die "legacy run-scoped RSS limit must be exactly 192 MiB"

prior_type="$(jq -r '.evidence_type // empty' "${PRIOR_EVIDENCE}")"
case "${OPERATION}:${prior_type}" in
  rehearsal-cutover:front-migration-preparation)
    python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect front-migration-preparation "${PRIOR_EVIDENCE}" >/dev/null
    source_kind=legacy
    destination_kind=front
    evidence_type=front-migration-cutover
    preparation_sha256="$(sha256sum "${PRIOR_EVIDENCE}" | awk '{print $1}')"
    ;;
  rollback:front-migration-cutover)
    python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect front-migration-cutover "${PRIOR_EVIDENCE}" >/dev/null
    [[ "$(jq -r '.mode' "${PRIOR_EVIDENCE}")" == rehearsal-cutover ]] || die "rollback requires rehearsal cutover evidence"
    source_kind=front
    destination_kind=legacy
    evidence_type=front-migration-rollback
    preparation_sha256="$(jq -r '.preparation_evidence_sha256' "${PRIOR_EVIDENCE}")"
    ;;
  final-cutover:front-migration-rollback)
    python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect front-migration-rollback "${PRIOR_EVIDENCE}" >/dev/null
    source_kind=legacy
    destination_kind=front
    evidence_type=front-migration-cutover
    preparation_sha256="$(jq -r '.preparation_evidence_sha256' "${PRIOR_EVIDENCE}")"
    ;;
  *) die "${OPERATION} cannot follow ${prior_type:-missing evidence}" ;;
esac
[[ "$(jq -r '.run.project' "${PRIOR_EVIDENCE}")" == "${PROJECT_ID}" &&
   "$(jq -r '.run.zone' "${PRIOR_EVIDENCE}")" == "${ZONE}" &&
   "$(jq -r '.run.instance' "${PRIOR_EVIDENCE}")" == "${INSTANCE}" ]] \
  || die "prior migration evidence target does not match the current GCP target"
prior_sha256="$(sha256sum "${PRIOR_EVIDENCE}" | awk '{print $1}')"
release_json="$(jq -c '.release' "${PRIOR_EVIDENCE}")"
bootstrap_json="$(jq -c '.bootstrap' "${PRIOR_EVIDENCE}")"
predecessor_json="$(jq -c '.predecessor' "${PRIOR_EVIDENCE}")"
routing_json="$(jq -c '.routing' "${PRIOR_EVIDENCE}")"
legacy_json="$(jq -c '.legacy' "${PRIOR_EVIDENCE}")"
front_json="$(jq -c '.front | {slot,generation,checksum,control_checksum,worker_checksum}' "${PRIOR_EVIDENCE}")"
URL_MAP="$(jq -r '.routing.url_map' "${PRIOR_EVIDENCE}")"
legacy_backend_url="$(jq -r '.routing.legacy_backend_url' "${PRIOR_EVIDENCE}")"
front_backend_url="$(jq -r '.routing.front_backend_url' "${PRIOR_EVIDENCE}")"

mkdir -p "${ARTIFACT_DIR}"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd)"
EVIDENCE_JSON="${EVIDENCE_JSON:-${SUBROUTER_DEPLOY_EVIDENCE_JSON:-${ARTIFACT_DIR}/result.json}}"
mkdir -p "$(dirname "${EVIDENCE_JSON}")"
EVIDENCE_JSON="$(cd "$(dirname "${EVIDENCE_JSON}")" && pwd)/$(basename "${EVIDENCE_JSON}")"
mkdir -p "$(dirname "${DESTINATION_PROOF_REQUEST}")" "$(dirname "${DESTINATION_PROOF}")"
DESTINATION_PROOF_REQUEST="$(cd "$(dirname "${DESTINATION_PROOF_REQUEST}")" && pwd)/$(basename "${DESTINATION_PROOF_REQUEST}")"
DESTINATION_PROOF="$(cd "$(dirname "${DESTINATION_PROOF}")" && pwd)/$(basename "${DESTINATION_PROOF}")"
[[ ! -e "${DESTINATION_PROOF_REQUEST}" && ! -L "${DESTINATION_PROOF_REQUEST}" ]] \
  || die "destination proof request path must not already exist"
[[ ! -e "${DESTINATION_PROOF}" && ! -L "${DESTINATION_PROOF}" ]] \
  || die "destination proof path must not already exist"

gcloud_ssh() {
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet --command "$1"
}
gcloud_scp() {
  "${GCLOUD_BINARY}" compute scp "$1" "${INSTANCE}:$2" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet
}
utc_now() { python3 -c 'from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))'; }
epoch_millis() { python3 -c 'import time; print(time.time_ns() // 1_000_000)'; }

metric_service() {
  case "$1" in
    legacy) printf 'subrouter.service\n' ;;
    front) printf 'subrouter-front.service\n' ;;
    slot-a|slot-b) printf 'subrouter-slot@%s.service\n' "$1" ;;
    *) return 1 ;;
  esac
}
service_restarts() { gcloud_ssh "systemctl show '$(metric_service "$1")' -p NRestarts --value" | tail -n 1; }
service_oom_kills() {
  local service
  service="$(metric_service "$1")"
  gcloud_ssh "set -eu; cg=\$(systemctl show '${service}' -p ControlGroup --value); awk '\$1 == \"oom_kill\" {print \$2}' /sys/fs/cgroup\${cg}/memory.events" | tail -n 1
}
service_memory_max() { gcloud_ssh "systemctl show '$(metric_service "$1")' -p MemoryMax --value" | tail -n 1; }

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

supervisor_snapshot() {
  local kind="$1" status active_id generation_connections inactive_connections public_connections slot
  if [[ "${kind}" == legacy ]]; then
    status="$(gcloud_ssh "sudo curl -fsS --unix-socket /var/lib/subrouter/supervisor.sock http://localhost/_subrouter/supervisor-status")"
    public_connections="$(jq -r --arg id "$(jq -r '.active.id' < <(stream_shell_value "${status}"))" '[.backends[] | select(.id == $id) | .connections][0] // -1' < <(stream_shell_value "${status}"))"
  else
    front_status="$(gcloud_ssh "sudo curl -fsS --unix-socket /var/lib/subrouter/front.sock http://localhost/_subrouter/front-status")"
    slot="$(jq -r '.active.id' < <(stream_shell_value "${front_status}"))"
    [[ "${slot}" == "$(jq -r '.slot' < <(stream_shell_value "${front_json}"))" ]] || die "front active slot differs from preparation evidence"
    public_connections="$(jq -r --arg id "${slot}" '[.backends[] | select(.id == $id) | .connections][0] // -1' < <(stream_shell_value "${front_status}"))"
    status="$(gcloud_ssh "sudo curl -fsS --unix-socket /var/lib/subrouter/${slot}.sock http://localhost/_subrouter/supervisor-status")"
  fi
  jq -e '(.active.id|type)=="string" and (.backends|type)=="array" and ([.backends[].connections] | all(type=="number" and . >= 0))' \
    < <(stream_shell_value "${status}") >/dev/null || die "${kind} supervisor status is invalid"
  active_id="$(jq -r '.active.id' < <(stream_shell_value "${status}"))"
  generation_connections="$(jq -r --arg id "${active_id}" '[.backends[] | select(.id == $id) | .connections][0] // -1' < <(stream_shell_value "${status}"))"
  inactive_connections="$(jq -r --arg id "${active_id}" '[.backends[] | select(.id != $id) | .connections] | add // 0' < <(stream_shell_value "${status}"))"
  [[ "${public_connections}" =~ ^[0-9]+$ && "${generation_connections}" =~ ^[0-9]+$ && "${inactive_connections}" =~ ^[0-9]+$ ]] \
    || die "${kind} returned invalid connection counts"
  jq -nc --arg kind "${kind}" --arg generation "${active_id}" \
    --argjson public_connections "${public_connections}" \
    --argjson generation_connections "${generation_connections}" \
    --argjson inactive_connections "${inactive_connections}" \
    '{kind:$kind,generation:$generation,public_connections:$public_connections,
      generation_connections:$generation_connections,inactive_connections:$inactive_connections}'
}

lock_holder_pid=""
transition_started=0
transition_committed=0
before_yaml=""
acquire_lock() {
  subrouter_acquire_deploy_lock "${ARTIFACT_DIR}/deploy-lock.log" \
    "${GCLOUD_BINARY}" "${INSTANCE}" "${PROJECT_ID}" "${ZONE}" "${DEPLOY_LOCK_FILE}" \
    || die "could not acquire ${DEPLOY_LOCK_FILE}"
}
release_lock() {
  subrouter_release_deploy_lock
}
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  stop_all_rss_samplers
  if [[ "${transition_started}" == 1 && "${transition_committed}" == 0 && -n "${before_yaml}" ]]; then
    log "restoring the exact pre-transition URL map after failed validation" >&2
    restored_yaml="${ARTIFACT_DIR}/url-map-restored.yaml"
    if ! "${GCLOUD_BINARY}" compute url-maps import "${URL_MAP}" --project "${PROJECT_ID}" --global \
      --source "${before_yaml}" --quiet || \
      ! "${GCLOUD_BINARY}" compute url-maps export "${URL_MAP}" --project "${PROJECT_ID}" --global \
        --destination "${restored_yaml}" --quiet || \
      ! python3 "${DEPLOYMENT_CONTRACT}" assert-url-map \
        "${restored_yaml}" "${source_url}" 1 "${destination_url}" 0
    then
      log "failed to restore the pre-transition URL map" >&2
      status=1
    fi
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
active_migration_slot="$(jq -r '.slot' < <(stream_shell_value "${front_json}"))"
legacy_restarts_before="$(service_restarts legacy)"
legacy_oom_before="$(service_oom_kills legacy)"
slot_restarts_before="$(service_restarts "${active_migration_slot}")"
slot_oom_before="$(service_oom_kills "${active_migration_slot}")"
front_restarts_before="$(service_restarts front)"
front_oom_before="$(service_oom_kills front)"
start_rss_sampler legacy
start_rss_sampler "${active_migration_slot}"
start_rss_sampler front
before_yaml="${ARTIFACT_DIR}/url-map-before.yaml"
candidate_yaml="${ARTIFACT_DIR}/url-map-candidate.yaml"
after_yaml="${ARTIFACT_DIR}/url-map-after.yaml"
"${GCLOUD_BINARY}" compute url-maps export "${URL_MAP}" --project "${PROJECT_ID}" --global \
  --destination "${before_yaml}" --quiet
if [[ "${source_kind}" == legacy ]]; then source_url="${legacy_backend_url}"; destination_url="${front_backend_url}"; else source_url="${front_backend_url}"; destination_url="${legacy_backend_url}"; fi
python3 "${DEPLOYMENT_CONTRACT}" rewrite-url-map \
  "${before_yaml}" "${candidate_yaml}" "${source_url}" "${destination_url}"
source_before="$(supervisor_snapshot "${source_kind}")"
jq -e --argjson expected "${EXPECTED_CONNECTIONS}" \
  '.public_connections >= $expected and .generation_connections >= $expected and .inactive_connections == 0' \
  < <(stream_shell_value "${source_before}") >/dev/null || die "source did not correlate every external migration connection"
destination_before="$(supervisor_snapshot "${destination_kind}")"

transition_requested_at="$(utc_now)"
transition_requested_ms="$(epoch_millis)"
transition_started=1
"${GCLOUD_BINARY}" compute url-maps import "${URL_MAP}" --project "${PROJECT_ID}" --global \
  --source "${candidate_yaml}" --quiet
"${GCLOUD_BINARY}" compute url-maps export "${URL_MAP}" --project "${PROJECT_ID}" --global \
  --destination "${after_yaml}" --quiet
python3 "${DEPLOYMENT_CONTRACT}" assert-url-map \
  "${after_yaml}" "${source_url}" 0 "${destination_url}" 1
source_after="$(supervisor_snapshot "${source_kind}")"
jq -e --argjson expected "${EXPECTED_CONNECTIONS}" \
  '.public_connections >= $expected and .generation_connections >= $expected and .inactive_connections == 0' \
  < <(stream_shell_value "${source_after}") >/dev/null || die "URL-map transition cut externally held source connections"
source_snapshot_sha256="$(printf '%s' "${source_after}" | sha256sum | awk '{print $1}')"
if [[ "${destination_kind}" == front ]]; then
  destination_generation="$(jq -r '.generation' < <(stream_shell_value "${front_json}"))"
else
  destination_generation="$(jq -r '.generation' < <(stream_shell_value "${legacy_json}"))"
fi
proof_challenge="$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
proof_request_tmp="$(mktemp "${DESTINATION_PROOF_REQUEST}.tmp.XXXXXX")"
jq -n --arg schema 'subrouter.gcp.destination-proof-request/v1' \
  --arg challenge "${proof_challenge}" --arg operation "${OPERATION}" \
  --arg destination "${destination_kind}" --arg generation "${destination_generation}" \
  --arg source "${source_kind}" --arg source_generation "$(jq -r '.generation' < <(stream_shell_value "${source_after}"))" \
  --arg source_snapshot_sha "${source_snapshot_sha256}" --arg requested_at "${transition_requested_at}" \
  --argjson expected_source_connections "${EXPECTED_CONNECTIONS}" \
  '{schema:$schema,challenge:$challenge,operation:$operation,destination:$destination,
    destination_generation:$generation,source:$source,source_generation:$source_generation,
    source_snapshot_sha256:$source_snapshot_sha,
    expected_source_connections:$expected_source_connections,
    transition_requested_at:$requested_at}' \
  >"${proof_request_tmp}"
chmod 0600 "${proof_request_tmp}"
mv -f -- "${proof_request_tmp}" "${DESTINATION_PROOF_REQUEST}"

while [[ ! -f "${DESTINATION_PROOF}" || -L "${DESTINATION_PROOF}" ]]; do
  (( $(epoch_millis) - transition_requested_ms < 30000 )) \
    || die "golden destination proof was not observed strictly before 30 seconds"
  sleep 0.05
done
proof_received_at="$(utc_now)"
proof_received_ms="$(epoch_millis)"
(( proof_received_ms - transition_requested_ms < 30000 )) \
  || die "golden destination proof arrived at or after 30 seconds"
python3 "${DEPLOYMENT_CONTRACT}" validate-destination-proof \
  "${DESTINATION_PROOF}" "${proof_challenge}" "${OPERATION}" \
  "${destination_kind}" "${destination_generation}" "${source_kind}" \
  "$(jq -r '.generation' < <(stream_shell_value "${source_after}"))" "${source_snapshot_sha256}" \
  "${EXPECTED_CONNECTIONS}" "${transition_requested_at}" \
  "${proof_received_at}"
activated_at="$(jq -r '.observed_at' "${DESTINATION_PROOF}")"
destination_proof_sha256="$(sha256sum "${DESTINATION_PROOF}" | awk '{print $1}')"
destination_connection_id="$(jq -r '.connection_id' "${DESTINATION_PROOF}")"
destination_after="$(supervisor_snapshot "${destination_kind}")"
jq -e --arg generation "${destination_generation}" --argjson before "${destination_before}" \
  '.generation == $generation and .public_connections >= ($before.public_connections + 1) and
   .generation_connections >= ($before.generation_connections + 1) and .inactive_connections == 0' \
  < <(stream_shell_value "${destination_after}") >/dev/null \
  || die "golden fresh public connection was not correlated to the destination generation"
destination_connection_delta="$(( $(jq -r '.generation_connections' < <(stream_shell_value "${destination_after}")) - $(jq -r '.generation_connections' < <(stream_shell_value "${destination_before}")) ))"
legacy_restarts_after="$(service_restarts legacy)"
legacy_oom_after="$(service_oom_kills legacy)"
slot_restarts_after="$(service_restarts "${active_migration_slot}")"
slot_oom_after="$(service_oom_kills "${active_migration_slot}")"
front_restarts_after="$(service_restarts front)"
front_oom_after="$(service_oom_kills front)"
[[ "${legacy_restarts_after}" == "${legacy_restarts_before}" && "${legacy_oom_after}" == "${legacy_oom_before}" ]] \
  || die "legacy service restarted or OOM-killed during migration transition"
[[ "${slot_restarts_after}" == "${slot_restarts_before}" && "${slot_oom_after}" == "${slot_oom_before}" ]] \
  || die "slot service restarted or OOM-killed during migration transition"
[[ "${front_restarts_after}" == "${front_restarts_before}" && "${front_oom_after}" == "${front_oom_before}" ]] \
  || die "front service restarted or OOM-killed during migration transition"
legacy_peak_rss="$(stop_rss_sampler legacy)"
slot_peak_rss="$(stop_rss_sampler "${active_migration_slot}")"
front_peak_rss="$(stop_rss_sampler front)"
slot_memory_max="$(service_memory_max "${active_migration_slot}")"
front_memory_max="$(service_memory_max front)"
[[ "${slot_memory_max}" == 201326592 && "${front_memory_max}" == 134217728 ]] \
  || die "migration topology MemoryMax is incorrect"
(( legacy_peak_rss <= LEGACY_RSS_LIMIT_BYTES )) || die "legacy run-scoped RSS exceeded 192 MiB"
(( slot_peak_rss <= slot_memory_max )) || die "slot run-scoped RSS exceeded MemoryMax"
(( front_peak_rss <= front_memory_max )) || die "front run-scoped RSS exceeded MemoryMax"
evidence_emitted_at="$(utc_now)"

evidence_tmp="$(mktemp "${EVIDENCE_JSON}.tmp.XXXXXX")"
jq -n --arg schema 'subrouter.gcp.deploy-evidence/v1' --arg evidence_type "${evidence_type}" \
  --arg mode "${OPERATION}" --arg prior_type "${prior_type}" --arg prior_sha "${prior_sha256}" \
  --arg preparation_sha "${preparation_sha256}" --arg run_id "${RUN_LABEL}" \
  --arg project "${PROJECT_ID}" --arg zone "${ZONE}" --arg instance "${INSTANCE}" \
  --argjson release "${release_json}" --argjson bootstrap "${bootstrap_json}" \
  --argjson predecessor "${predecessor_json}" \
  --argjson routing "${routing_json}" \
  --argjson legacy "${legacy_json}" --argjson front "${front_json}" \
  --arg source "${source_kind}" --arg destination "${destination_kind}" \
  --arg source_url "${source_url}" --arg destination_url "${destination_url}" \
  --arg requested_at "${transition_requested_at}" --arg activated_at "${activated_at}" \
  --arg proof_received_at "${proof_received_at}" --arg emitted_at "${evidence_emitted_at}" \
  --arg proof_sha "${destination_proof_sha256}" --arg challenge "${proof_challenge}" \
  --arg connection_id "${destination_connection_id}" --argjson before "${source_before}" \
  --argjson after "${source_after}" --argjson expected "${EXPECTED_CONNECTIONS}" \
  --argjson destination_before "${destination_before}" --argjson destination_after "${destination_after}" \
  --argjson destination_delta "${destination_connection_delta}" \
  --arg slot_id "${active_migration_slot}" \
  --argjson legacy_rb "${legacy_restarts_before}" --argjson legacy_ra "${legacy_restarts_after}" \
  --argjson legacy_ob "${legacy_oom_before}" --argjson legacy_oa "${legacy_oom_after}" \
  --argjson slot_rb "${slot_restarts_before}" --argjson slot_ra "${slot_restarts_after}" \
  --argjson slot_ob "${slot_oom_before}" --argjson slot_oa "${slot_oom_after}" \
  --argjson front_rb "${front_restarts_before}" --argjson front_ra "${front_restarts_after}" \
  --argjson front_ob "${front_oom_before}" --argjson front_oa "${front_oom_after}" \
  --argjson legacy_peak "${legacy_peak_rss}" --argjson slot_peak "${slot_peak_rss}" \
  --argjson front_peak "${front_peak_rss}" --argjson legacy_limit "${LEGACY_RSS_LIMIT_BYTES}" \
  --argjson slot_limit "${slot_memory_max}" --argjson front_limit "${front_memory_max}" \
  '{schema:$schema,evidence_type:$evidence_type,mode:$mode,success:true,
    prior_evidence_type:$prior_type,prior_evidence_sha256:$prior_sha,
    preparation_evidence_sha256:$preparation_sha,
    run:{id:$run_id,project:$project,zone:$zone,instance:$instance},release:$release,
    bootstrap:$bootstrap,predecessor:$predecessor,
    routing:($routing + {before:$source,after:$destination,source_backend_url:$source_url,
      destination_backend_url:$destination_url}),legacy:$legacy,front:$front,
    timestamps:{transition_requested_at:$requested_at,activated_at:$activated_at,
      evidence_emitted_at:$emitted_at},
    destination_proof:{sha256:$proof_sha,challenge:$challenge,connection_id:$connection_id,
      original_continuity_verified:true,fresh_public_connection:true,
      observed_at:$activated_at,received_at:$proof_received_at},
    destination:{before:$destination_before,after:$destination_after,
      connection_count_delta:$destination_delta},
    metrics:{source_service:(if $source=="legacy" then "legacy" else "slot" end),
      destination_service:(if $destination=="legacy" then "legacy" else "slot" end),
      legacy:{nrestarts:{before:$legacy_rb,after:$legacy_ra},
        oom_kill:{before:$legacy_ob,after:$legacy_oa},run_scoped_peak_rss_bytes:$legacy_peak,
        rss_limit_bytes:$legacy_limit},
      slot:{id:$slot_id,nrestarts:{before:$slot_rb,after:$slot_ra},
        oom_kill:{before:$slot_ob,after:$slot_oa},run_scoped_peak_rss_bytes:$slot_peak,
        memory_max_bytes:$slot_limit},
      front:{nrestarts:{before:$front_rb,after:$front_ra},
        oom_kill:{before:$front_ob,after:$front_oa},run_scoped_peak_rss_bytes:$front_peak,
        memory_max_bytes:$front_limit}},
    source:{before:$before,after:$after,accepting_new_public_before:true,
      accepting_new_public_after:false},
    continuity:{expected_external_connections:$expected,preserved:true},
    rollback:{required:($mode=="rehearsal-cutover"),performed:($mode=="rollback")}}' \
  >"${evidence_tmp}"
python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect "${evidence_type}" "${evidence_tmp}" >/dev/null
chmod 0600 "${evidence_tmp}"
mv -f -- "${evidence_tmp}" "${EVIDENCE_JSON}"
transition_committed=1
log "${OPERATION} passed: ${source_kind} -> ${destination_kind}; source connections remain live"
jq -c . "${EVIDENCE_JSON}"
