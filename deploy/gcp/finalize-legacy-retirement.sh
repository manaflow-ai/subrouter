#!/usr/bin/env bash
# Finalize the disabled legacy supervisor after its handed-off sessions drain.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: finalize-legacy-retirement.sh --cutover-evidence PATH [--evidence-json PATH]

Requires linked listener-handoff evidence and an unchanged live URL map. The
stable front must own the exact kernel listener previously held by the legacy
supervisor. The disabled legacy process remains alive until its held sessions drain.
EOF
}

CUTOVER_EVIDENCE=""
EVIDENCE_JSON=""
while (( $# > 0 )); do
  case "$1" in
    --cutover-evidence) (( $# >= 2 )) || { usage >&2; exit 2; }; CUTOVER_EVIDENCE="$2"; shift 2 ;;
    --evidence-json) (( $# >= 2 )) || { usage >&2; exit 2; }; EVIDENCE_JSON="$2"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ -n "${CUTOVER_EVIDENCE}" ]] || { usage >&2; exit 2; }

PROJECT_ID="${SUBROUTER_GCP_PROJECT:?set SUBROUTER_GCP_PROJECT}"
ZONE="${SUBROUTER_GCP_ZONE:?set SUBROUTER_GCP_ZONE}"
INSTANCE="${SUBROUTER_GCP_INSTANCE:?set SUBROUTER_GCP_INSTANCE}"
GCLOUD_BINARY="${GCLOUD_BIN:-gcloud}"
DEPLOY_LOCK_FILE="${SUBROUTER_DEPLOY_LOCK_FILE:-/run/lock/subrouter-deploy.lock}"
DRAIN_TIMEOUT_SECONDS="${SUBROUTER_RETIRE_DRAIN_TIMEOUT_SECONDS:-3600}"
ARTIFACT_DIR="${SUBROUTER_DEPLOY_ARTIFACT_DIR:-${PWD}/artifacts/gcp-legacy-retirement}"
RUN_LABEL="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-legacy-retire-$$"
RUN_LABEL="${RUN_LABEL//[^a-zA-Z0-9._-]/-}"
REMOTE_INSTALLER="/tmp/install-front-slots-${RUN_LABEL}.sh"
REMOTE_DEPLOYMENT_CONTRACT="/tmp/deployment-contract-${RUN_LABEL}.py"
HANDOFF_CHECKPOINT="/var/lib/subrouter/front-handoff-checkpoint.json"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
URL_MAP_ROUTING="${SCRIPT_DIR}/url-map-routing.py"
CANARY_SECURITY_POLICY_HELPER="${SCRIPT_DIR}/canary-security-policy.py"
# shellcheck source=deploy/gcp/stream-shell-value.sh
source "${SCRIPT_DIR}/stream-shell-value.sh"
# shellcheck source=deploy/gcp/deploy-lock.sh
source "${SCRIPT_DIR}/deploy-lock.sh"

log() { printf 'gcp-legacy-retirement: %s\n' "$*"; }
die() { log "$*" >&2; exit 1; }
case "${INSTANCE}" in
  subrouter-team)
    expected_active_matcher="__root__"
    expected_canary_matcher="subrouter-front-canary"
    expected_canary_host="front-canary.sr.cmux.internal"
    expected_canary_security_policy="subrouter-front-canary-policy"
    ;;
  subrouter-staging)
    expected_active_matcher="staging-subrouter"
    expected_canary_matcher="staging-subrouter-front-canary"
    expected_canary_host="front-canary.staging.sr.cmux.internal"
    expected_canary_security_policy="subrouter-staging-front-canary-policy"
    ;;
  *) die "unsupported instance: ${INSTANCE}" ;;
esac
INSTALL_FRONT_SLOTS="$(bash "${SCRIPT_DIR}/resolve-release-installer.sh" "${SCRIPT_DIR}/install-front-slots.sh")"
DEPLOYMENT_CONTRACT="$(bash "${SCRIPT_DIR}/resolve-release-contract.sh" "${SCRIPT_DIR}/deployment-contract.py")"
REMOTE_INSTALL_COMMAND="sudo env SUBROUTER_DEPLOYMENT_CONTRACT='${REMOTE_DEPLOYMENT_CONTRACT}' bash '${REMOTE_INSTALLER}'"
for command in "${GCLOUD_BINARY}" jq curl python3 sha256sum; do
  command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done
[[ -f "${URL_MAP_ROUTING}" ]] || die "URL-map routing helper is missing"
[[ -f "${CANARY_SECURITY_POLICY_HELPER}" ]] || die "canary security policy helper is missing"
INSTALL_FRONT_SLOTS_SHA256="$(sha256sum "${INSTALL_FRONT_SLOTS}" | awk '{print $1}')"
DEPLOYMENT_CONTRACT_SHA256="$(sha256sum "${DEPLOYMENT_CONTRACT}" | awk '{print $1}')"
[[ "${DRAIN_TIMEOUT_SECONDS}" =~ ^[0-9]+$ ]] || die "SUBROUTER_RETIRE_DRAIN_TIMEOUT_SECONDS must be an integer"
(( DRAIN_TIMEOUT_SECONDS > 0 )) || die "SUBROUTER_RETIRE_DRAIN_TIMEOUT_SECONDS must be positive"
python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect front-migration-cutover "${CUTOVER_EVIDENCE}" >/dev/null
[[ "$(jq -r '.mode' "${CUTOVER_EVIDENCE}")" == final-cutover ]] || die "legacy retirement requires final-cutover evidence"
[[ "$(jq -r '.run.project' "${CUTOVER_EVIDENCE}")" == "${PROJECT_ID}" &&
   "$(jq -r '.run.zone' "${CUTOVER_EVIDENCE}")" == "${ZONE}" &&
   "$(jq -r '.run.instance' "${CUTOVER_EVIDENCE}")" == "${INSTANCE}" ]] \
  || die "cutover evidence target does not match the current GCP target"
cutover_sha256="$(sha256sum "${CUTOVER_EVIDENCE}" | awk '{print $1}')"
preparation_sha256="$(jq -r '.preparation_evidence_sha256' "${CUTOVER_EVIDENCE}")"
release_json="$(jq -c '.release' "${CUTOVER_EVIDENCE}")"
bootstrap_json="$(jq -c '.bootstrap' "${CUTOVER_EVIDENCE}")"
predecessor_json="$(jq -c '.predecessor' "${CUTOVER_EVIDENCE}")"
URL_MAP="$(jq -r '.routing.url_map' "${CUTOVER_EVIDENCE}")"
legacy_backend_url="$(jq -r '.routing.legacy_backend_url' "${CUTOVER_EVIDENCE}")"
front_backend_url="$(jq -r '.routing.front_backend_url' "${CUTOVER_EVIDENCE}")"
front_backend_service="$(jq -r '.routing.front_backend' "${CUTOVER_EVIDENCE}")"
[[ "$(jq -r '.routing.active_matcher' "${CUTOVER_EVIDENCE}")" == "${expected_active_matcher}" &&
   "$(jq -r '.routing.canary.matcher' "${CUTOVER_EVIDENCE}")" == "${expected_canary_matcher}" &&
   "$(jq -r '.routing.canary.host' "${CUTOVER_EVIDENCE}")" == "${expected_canary_host}" &&
   "$(jq -r '.routing.canary.access_control.name' "${CUTOVER_EVIDENCE}")" == "${expected_canary_security_policy}" ]] \
  || die "cutover evidence routing selectors do not match the target instance"
active_matcher="${expected_active_matcher}"
canary_matcher="${expected_canary_matcher}"
canary_host="${expected_canary_host}"
legacy_generation="$(jq -r '.legacy.generation' "${CUTOVER_EVIDENCE}")"
legacy_checksum="$(jq -r '.legacy.checksum' "${CUTOVER_EVIDENCE}")"
accepting_new_public_false_at="$(jq -r '.timestamps.source_listener_retired_at' "${CUTOVER_EVIDENCE}")"
cutover_listener_inode="$(jq -r '.listener.destination_inode' "${CUTOVER_EVIDENCE}")"
cutover_run_label="$(jq -r '.run.id' "${CUTOVER_EVIDENCE}")"
[[ "${cutover_run_label}" =~ ^[a-zA-Z0-9._-]+$ ]] || die "cutover sampler run label is invalid"

mkdir -p "${ARTIFACT_DIR}"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd)"
EVIDENCE_JSON="${EVIDENCE_JSON:-${SUBROUTER_DEPLOY_EVIDENCE_JSON:-${ARTIFACT_DIR}/result.json}}"
mkdir -p "$(dirname "${EVIDENCE_JSON}")"
EVIDENCE_JSON="$(cd "$(dirname "${EVIDENCE_JSON}")" && pwd)/$(basename "${EVIDENCE_JSON}")"
retirement_already_complete=0
if [[ -e "${EVIDENCE_JSON}" || -L "${EVIDENCE_JSON}" ]]; then
  [[ -f "${EVIDENCE_JSON}" && ! -L "${EVIDENCE_JSON}" ]] \
    || die "legacy retirement evidence path is not a regular file"
  python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect legacy-retirement "${EVIDENCE_JSON}" >/dev/null
  [[ "$(jq -r '.cutover_evidence_sha256' "${EVIDENCE_JSON}")" == "${cutover_sha256}" ]] \
    || die "existing legacy retirement evidence belongs to a different cutover"
  retirement_already_complete=1
fi

gcloud_ssh() {
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet --command "$1"
}
gcloud_scp() {
  "${GCLOUD_BINARY}" compute scp "$1" "${INSTANCE}:$2" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet
}
assert_canary_access_boundary() {
  local expected_policy_url backend_json policy_json live_access expected_token_sha
  expected_policy_url="https://www.googleapis.com/compute/v1/projects/${PROJECT_ID}/global/securityPolicies/${expected_canary_security_policy}"
  backend_json="$("${GCLOUD_BINARY}" compute backend-services describe "${front_backend_service}" \
    --project "${PROJECT_ID}" --global --format=json)"
  [[ "$(jq -r '.securityPolicy // empty' < <(stream_shell_value "${backend_json}"))" == "${expected_policy_url}" ]] \
    || die "front backend lost its canary security policy before legacy retirement"
  policy_json="$("${GCLOUD_BINARY}" compute security-policies describe "${expected_canary_security_policy}" \
    --project "${PROJECT_ID}" --global --format=json)"
  live_access="$(printf '%s\n' "${policy_json}" | python3 "${CANARY_SECURITY_POLICY_HELPER}" \
    assert-ready - "${expected_canary_security_policy}" "${expected_canary_host}")"
  expected_token_sha="$(jq -r '.routing.canary.access_control.key_fingerprint_sha256' "${CUTOVER_EVIDENCE}")"
  [[ "$(jq -r '.key_fingerprint_sha256' < <(stream_shell_value "${live_access}"))" == "${expected_token_sha}" ]] \
    || die "live canary security policy no longer matches the cutover evidence"
}
utc_now() { python3 -c 'from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))'; }
epoch_millis() { python3 -c 'import time; print(time.time_ns() // 1_000_000)'; }
legacy_status() { gcloud_ssh "sudo curl -fsS --unix-socket /var/lib/subrouter/supervisor.sock http://localhost/_subrouter/supervisor-status"; }
legacy_counts() {
  local status="$1" active_id active_connections inactive_connections
  jq -e '(.active.id|type)=="string" and (.backends|type)=="array" and ([.backends[].connections] | all(type=="number" and . >= 0))' \
    < <(stream_shell_value "${status}") >/dev/null || die "legacy supervisor returned invalid status"
  active_id="$(jq -r '.active.id' < <(stream_shell_value "${status}"))"
  [[ "${active_id}" == "${legacy_generation}" ]] || die "legacy generation changed from cutover evidence"
  active_connections="$(jq -r --arg id "${active_id}" '[.backends[] | select(.id == $id) | .connections][0] // -1' < <(stream_shell_value "${status}"))"
  inactive_connections="$(jq -r --arg id "${active_id}" '[.backends[] | select(.id != $id) | .connections] | add // 0' < <(stream_shell_value "${status}"))"
  jq -nc --argjson active "${active_connections}" --argjson inactive "${inactive_connections}" \
    '{active:$active,inactive:$inactive,total:($active+$inactive)}'
}
legacy_restarts() { gcloud_ssh "systemctl show subrouter.service -p NRestarts --value" | tail -n 1; }
legacy_oom_kills() {
  gcloud_ssh "set -eu; cg=\$(systemctl show subrouter.service -p ControlGroup --value); awk '\$1 == \"oom_kill\" {print \$2}' /sys/fs/cgroup\${cg}/memory.events" | tail -n 1
}

sampler_adopted=0
sampler_unit="subrouter-rss-${cutover_run_label}-legacy.service"
sampler_sentinel="/tmp/subrouter-rss-${cutover_run_label}-legacy.running"
sampler_result="/tmp/subrouter-rss-${cutover_run_label}-legacy.peak"
sampler_oom_result="/tmp/subrouter-rss-${cutover_run_label}-legacy.oom"
read_sampler_unit_status() {
  gcloud_ssh "state=\$(systemctl show '${sampler_unit}' -p ActiveState --value); result=\$(systemctl show '${sampler_unit}' -p Result --value); status=\$(systemctl show '${sampler_unit}' -p ExecMainStatus --value); printf '%s|%s|%s\\n' \"\${state}\" \"\${result}\" \"\${status}\"" | tail -n 1
}
assert_sampler_completed_successfully() {
  local unit_status state result status
  unit_status="$(read_sampler_unit_status)"
  IFS='|' read -r state result status <<<"${unit_status}"
  [[ "${state}" == inactive && "${result}" == success && "${status}" == 0 ]] \
    || die "legacy retirement sampler ended as ${unit_status}, expected inactive|success|0"
}
adopt_legacy_sampler() {
  local unit_status state result status
  gcloud_ssh "sudo test -s '${sampler_result}' -a -s '${sampler_oom_result}'" \
    || die "cutover did not leave a continuous legacy sampler"
  unit_status="$(read_sampler_unit_status)"
  IFS='|' read -r state result status <<<"${unit_status}"
  if [[ "${state}" != active ]]; then
    [[ "${state}" == inactive && "${result}" == success && "${status}" == 0 ]] \
      || die "cutover legacy sampler is ${unit_status}"
    gcloud_ssh "! systemctl is-active --quiet subrouter.service && sudo test ! -S /var/lib/subrouter/supervisor.sock" \
      || die "legacy sampler stopped before legacy service retirement"
  fi
  sampler_adopted=1
}
stop_legacy_sampler() {
  gcloud_ssh "sudo rm -f '${sampler_sentinel}'"
  for _ in $(seq 1 200); do
    gcloud_ssh "! systemctl is-active --quiet '${sampler_unit}'" && break
    sleep 0.05
  done
  gcloud_ssh "! systemctl is-active --quiet '${sampler_unit}'" \
    || die "legacy retirement sampler did not stop"
  assert_sampler_completed_successfully
  legacy_peak_rss="$(gcloud_ssh "sudo cat '${sampler_result}'" | tail -n 1)"
  sampled_oom_after="$(gcloud_ssh "sudo cat '${sampler_oom_result}'" | tail -n 1)"
  [[ "${legacy_peak_rss}" =~ ^[0-9]+$ && "${sampled_oom_after}" =~ ^[0-9]+$ ]] \
    || die "legacy retirement sampler returned invalid metrics"
  sampler_adopted=0
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
  if (( sampler_adopted == 1 )); then
    log "preserving continuous legacy sampler ${sampler_unit} for retirement retry" >&2
  fi
  gcloud_ssh "rm -f '${REMOTE_INSTALLER}' '${REMOTE_DEPLOYMENT_CONTRACT}'" >/dev/null 2>&1 || true
  release_lock
  exit "${status}"
}
trap cleanup EXIT INT TERM

acquire_lock
if (( retirement_already_complete == 1 )); then
  gcloud_ssh "sudo rm -f '${HANDOFF_CHECKPOINT}'; sudo test ! -e '${HANDOFF_CHECKPOINT}'"
  log "legacy retirement already completed"
  jq -c . "${EVIDENCE_JSON}"
  exit 0
fi
assert_canary_access_boundary
gcloud_scp "${INSTALL_FRONT_SLOTS}" "${REMOTE_INSTALLER}"
gcloud_scp "${DEPLOYMENT_CONTRACT}" "${REMOTE_DEPLOYMENT_CONTRACT}"
gcloud_ssh "printf '%s  %s\n%s  %s\n' '${INSTALL_FRONT_SLOTS_SHA256}' '${REMOTE_INSTALLER}' '${DEPLOYMENT_CONTRACT_SHA256}' '${REMOTE_DEPLOYMENT_CONTRACT}' | sha256sum -c - >/dev/null"
handoff_checkpoint="$(gcloud_ssh "set -eu; sudo test \"\$(stat -c '%u:%g:%a' '${HANDOFF_CHECKPOINT}')\" = '0:0:600'; sudo python3 '${REMOTE_DEPLOYMENT_CONTRACT}' validate-front-handoff-checkpoint '${HANDOFF_CHECKPOINT}' '${preparation_sha256}' '${PROJECT_ID}' '${ZONE}' '${INSTANCE}' '$(jq -r '.front.slot' "${CUTOVER_EVIDENCE}")' 2")"
jq -e --arg run_id "${cutover_run_label}" --arg inode "${cutover_listener_inode}" \
  '.run.id == $run_id and .listener.inode == $inode' \
  < <(stream_shell_value "${handoff_checkpoint}") >/dev/null \
  || die "legacy retirement checkpoint does not match the cutover evidence"
url_map_applied="${ARTIFACT_DIR}/url-map-final.yaml"
"${GCLOUD_BINARY}" compute url-maps export "${URL_MAP}" --project "${PROJECT_ID}" --global \
  --destination "${url_map_applied}" --quiet
python3 "${URL_MAP_ROUTING}" assert-state \
  "${url_map_applied}" "${active_matcher}" "${legacy_backend_url}" \
  "${canary_matcher}" "${canary_host}" "${front_backend_url}"
front_listener="$(gcloud_ssh "${REMOTE_INSTALL_COMMAND} listener-status subrouter-front.service 31415" | tail -n 1)"
[[ "$(jq -r '.inode' < <(stream_shell_value "${front_listener}"))" == "${cutover_listener_inode}" ]] \
  || die "stable front no longer owns the handed-off public listener"
gcloud_ssh "sudo curl -fsS --unix-socket /var/lib/subrouter/front.sock http://localhost/_subrouter/front-status >/dev/null; curl -fsS http://127.0.0.1:31415/_subrouter/ready >/dev/null"
installed_sum="$(gcloud_ssh "sudo sha256sum /usr/local/bin/subrouter | awk '{print \$1}'" | tail -n 1)"
[[ "${installed_sum}" == "${legacy_checksum}" ]] || die "legacy bytes changed after final cutover"
initial_counts="$(jq -c '.source.after | {active:.generation_connections,inactive:.inactive_connections,total:(.generation_connections+.inactive_connections)}' "${CUTOVER_EVIDENCE}")"
restarts_before="$(jq -r '.metrics.legacy.nrestarts.after' "${CUTOVER_EVIDENCE}")"
oom_before="$(jq -r '.metrics.legacy.oom_kill.after' "${CUTOVER_EVIDENCE}")"
legacy_peak_rss="$(jq -r '.metrics.legacy.run_scoped_peak_rss_bytes' "${CUTOVER_EVIDENCE}")"
[[ "${restarts_before}" =~ ^[0-9]+$ && "${oom_before}" =~ ^[0-9]+$ && "${legacy_peak_rss}" =~ ^[0-9]+$ ]] \
  || die "cutover evidence has invalid legacy metrics"
adopt_legacy_sampler

deadline=$(( $(date +%s) + DRAIN_TIMEOUT_SECONDS ))
while true; do
  if gcloud_ssh "! systemctl is-active --quiet subrouter.service && sudo test ! -S /var/lib/subrouter/supervisor.sock"; then
    last_connection_closed_at="$(utc_now)"
    last_connection_closed_ms="$(epoch_millis)"
    break
  fi
  gcloud_ssh "systemctl is-active --quiet '${sampler_unit}'" \
    || die "legacy retirement sampler stopped before legacy service retirement"
  (( $(date +%s) < deadline )) || die "legacy supervisor did not drain within ${DRAIN_TIMEOUT_SECONDS}s"
  sleep 0.1
done

stop_legacy_sampler
oom_after="${sampled_oom_after}"
[[ "${oom_after}" == "${oom_before}" ]] || die "legacy service was OOM-killed while draining"
(( legacy_peak_rss <= 201326592 )) || die "legacy run-scoped RSS exceeded 192 MiB"
stop_requested_at="$(utc_now)"
gcloud_ssh "sudo systemctl disable subrouter.service; sudo systemctl disable subrouter.socket >/dev/null 2>&1 || true"
for _ in $(seq 1 300); do
  if gcloud_ssh "! systemctl is-active --quiet subrouter.service && sudo test ! -S /var/lib/subrouter/supervisor.sock"; then
    absent_at="$(utc_now)"
    absent_ms="$(epoch_millis)"
    absence_latency_ms=$((absent_ms - last_connection_closed_ms))
    break
  fi
  sleep 0.1
done
[[ -n "${absence_latency_ms:-}" ]] || die "legacy service remained present 30 seconds after drain"
(( absence_latency_ms >= 0 && absence_latency_ms < 30000 )) || die "legacy absence was not strictly below 30 seconds"
restarts_after="$(legacy_restarts)"
[[ "${restarts_after}" == "${restarts_before}" ]] || die "legacy service restarted during retirement"
service_result="$(gcloud_ssh "systemctl show subrouter.service -p Result --value" | tail -n 1)"
[[ "${service_result}" == success ]] || die "legacy service result is ${service_result}, expected success"
enabled_after="$(gcloud_ssh "if systemctl is-enabled --quiet subrouter.service; then echo true; else echo false; fi" | tail -n 1)"
[[ "${enabled_after}" == false ]] || die "legacy service stayed enabled"
gcloud_ssh "! systemctl is-enabled --quiet subrouter.socket" \
  || die "legacy socket stayed enabled"
evidence_emitted_at="$(utc_now)"

evidence_tmp="$(mktemp "${EVIDENCE_JSON}.tmp.XXXXXX")"
jq -n --arg schema 'subrouter.gcp.deploy-evidence/v1' --arg evidence_type legacy-retirement \
  --arg cutover_sha "${cutover_sha256}" --arg preparation_sha "${preparation_sha256}" \
  --arg run_id "${RUN_LABEL}" --arg project "${PROJECT_ID}" --arg zone "${ZONE}" --arg instance "${INSTANCE}" \
  --argjson release "${release_json}" --argjson bootstrap "${bootstrap_json}" \
  --argjson predecessor "${predecessor_json}" \
  --arg generation "${legacy_generation}" --arg checksum "${legacy_checksum}" \
  --arg legacy_backend_url "${legacy_backend_url}" \
  --arg accepting_false_at "${accepting_new_public_false_at}" --arg closed_at "${last_connection_closed_at}" \
  --arg stop_requested_at "${stop_requested_at}" --arg absent_at "${absent_at}" \
  --arg emitted_at "${evidence_emitted_at}" --arg result "${service_result}" \
  --argjson latency "${absence_latency_ms}" --argjson initial_counts "${initial_counts}" \
  --argjson rb "${restarts_before}" --argjson ra "${restarts_after}" \
  --argjson oom_before "${oom_before}" --argjson oom_after "${oom_after}" \
  --argjson peak_rss "${legacy_peak_rss}" --argjson rss_limit 201326592 \
  '{schema:$schema,evidence_type:$evidence_type,mode:"final-cutover",success:true,
    cutover_evidence_sha256:$cutover_sha,preparation_evidence_sha256:$preparation_sha,
    run:{id:$run_id,project:$project,zone:$zone,instance:$instance},release:$release,
    bootstrap:$bootstrap,predecessor:$predecessor,
    routing:{active:"front",mechanism:"listener-fd-takeover",
      legacy_backend_url:$legacy_backend_url,active_backend_url:$legacy_backend_url,
      legacy_backend_retained:true,accepting_new_public:false},
    legacy:{service:"subrouter.service",generation:$generation,checksum:$checksum},
    connections:{before:$initial_counts,after:{active:0,inactive:0,total:0}},
    retirement:{accepting_new_public_false_at:$accepting_false_at,
      last_connection_closed_at:$closed_at,stop_requested_at:$stop_requested_at,
      absent_at:$absent_at,absence_latency_ms:$latency,service_active_after:false,
      control_socket_present_after:false,enabled_after:false,service_result:$result},
    metrics:{nrestarts:{before:$rb,after:$ra},oom_kill:{before:$oom_before,after:$oom_after},
      run_scoped_peak_rss_bytes:$peak_rss,rss_limit_bytes:$rss_limit},
    evidence_emitted_at:$emitted_at}' >"${evidence_tmp}"
python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect legacy-retirement "${evidence_tmp}" >/dev/null
chmod 0600 "${evidence_tmp}"
mv -f -- "${evidence_tmp}" "${EVIDENCE_JSON}"
gcloud_ssh "sudo rm -f '${HANDOFF_CHECKPOINT}'; sudo test ! -e '${HANDOFF_CHECKPOINT}'"
log "legacy retirement passed; service absent and rollback backend resources retained"
jq -c . "${EVIDENCE_JSON}"
