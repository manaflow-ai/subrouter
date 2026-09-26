#!/usr/bin/env bash
# Transfer the live public listener from the legacy supervisor to the stable front.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: switch-front-migration.sh --operation final-cutover \
  --prior-evidence PATH --destination-proof-request PATH --destination-proof PATH \
  [--evidence-json PATH]

The final cutover duplicates the exact kernel listener into the stable front,
then retires only the legacy owner's descriptor. GCP routing is not changed.
Existing source connections remain correlated to their supervisor generation.
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
MIGRATION_PROPAGATION_LIMIT_MS=300000
DESTINATION_LIVENESS_LIMIT_MS=10000
REMOTE_INSTALLER="/tmp/install-front-slots-${RUN_LABEL}.sh"
REMOTE_DEPLOYMENT_CONTRACT="/tmp/deployment-contract-${RUN_LABEL}.py"
REMOTE_HANDOFF_UPLOAD="/tmp/front-handoff-${RUN_LABEL}.json"
HANDOFF_CHECKPOINT="/var/lib/subrouter/front-handoff-checkpoint.json"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
URL_MAP_ROUTING="${SCRIPT_DIR}/url-map-routing.py"
CANARY_SECURITY_POLICY_HELPER="${SCRIPT_DIR}/canary-security-policy.py"
# shellcheck source=deploy/gcp/stream-shell-value.sh
source "${SCRIPT_DIR}/stream-shell-value.sh"
# shellcheck source=deploy/gcp/deploy-lock.sh
source "${SCRIPT_DIR}/deploy-lock.sh"
LEGACY_RSS_LIMIT_BYTES="${SUBROUTER_LEGACY_RSS_LIMIT_BYTES:-201326592}"

log() { printf 'gcp-front-transition: %s\n' "$*"; }
die() { log "$*" >&2; exit 1; }
case "${INSTANCE}" in
  subrouter-team)
    EXPECTED_ACTIVE_MATCHER="__root__"
    EXPECTED_CANARY_MATCHER="subrouter-front-canary"
    EXPECTED_CANARY_HOST="front-canary.sr.cmux.internal"
    EXPECTED_CANARY_SECURITY_POLICY="subrouter-front-canary-policy"
    ;;
  subrouter-staging)
    EXPECTED_ACTIVE_MATCHER="staging-subrouter"
    EXPECTED_CANARY_MATCHER="staging-subrouter-front-canary"
    EXPECTED_CANARY_HOST="front-canary.staging.sr.cmux.internal"
    EXPECTED_CANARY_SECURITY_POLICY="subrouter-staging-front-canary-policy"
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
EXPECTED_CONNECTIONS=2
if [[ -n "${EXPECTED_CONNECTIONS_OVERRIDE}" ]]; then
  [[ "${EXPECTED_CONNECTIONS_OVERRIDE}" =~ ^[0-9]+$ ]] || die "SUBROUTER_EXPECTED_MIGRATION_CONNECTIONS must be an integer"
  [[ "${EXPECTED_CONNECTIONS_OVERRIDE}" == "${EXPECTED_CONNECTIONS}" ]] \
    || die "SUBROUTER_EXPECTED_MIGRATION_CONNECTIONS cannot weaken the phase-bound minimum"
fi
[[ "${LEGACY_RSS_LIMIT_BYTES}" == 201326592 ]] || die "legacy run-scoped RSS limit must be exactly 192 MiB"

prior_type="$(jq -r '.evidence_type // empty' "${PRIOR_EVIDENCE}")"
case "${OPERATION}:${prior_type}" in
  final-cutover:front-migration-preparation)
    python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect front-migration-preparation "${PRIOR_EVIDENCE}" >/dev/null
    source_kind=legacy
    destination_kind=front
    evidence_type=front-migration-cutover
    preparation_sha256="$(sha256sum "${PRIOR_EVIDENCE}" | awk '{print $1}')"
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
FRONT_BACKEND_SERVICE="$(jq -r '.routing.front_backend' "${PRIOR_EVIDENCE}")"
[[ "$(jq -r '.routing.active_matcher' "${PRIOR_EVIDENCE}")" == "${EXPECTED_ACTIVE_MATCHER}" &&
   "$(jq -r '.routing.canary.matcher' "${PRIOR_EVIDENCE}")" == "${EXPECTED_CANARY_MATCHER}" &&
   "$(jq -r '.routing.canary.host' "${PRIOR_EVIDENCE}")" == "${EXPECTED_CANARY_HOST}" &&
   "$(jq -r '.routing.canary.access_control.name' "${PRIOR_EVIDENCE}")" == "${EXPECTED_CANARY_SECURITY_POLICY}" ]] \
  || die "prior migration evidence routing selectors do not match the target instance"
ACTIVE_MATCHER="${EXPECTED_ACTIVE_MATCHER}"
CANARY_MATCHER="${EXPECTED_CANARY_MATCHER}"
CANARY_HOST="${EXPECTED_CANARY_HOST}"

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
  "${SCRIPT_DIR}/gcloud-scp.sh" "${GCLOUD_BINARY}" "$1" "${INSTANCE}:$2" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet
}
assert_canary_access_boundary() {
  local expected_policy_url backend_json policy_json live_access expected_token_sha
  expected_policy_url="https://www.googleapis.com/compute/v1/projects/${PROJECT_ID}/global/securityPolicies/${EXPECTED_CANARY_SECURITY_POLICY}"
  backend_json="$("${GCLOUD_BINARY}" compute backend-services describe "${FRONT_BACKEND_SERVICE}" \
    --project "${PROJECT_ID}" --global --format=json)"
  [[ "$(jq -r '.securityPolicy // empty' < <(stream_shell_value "${backend_json}"))" == "${expected_policy_url}" ]] \
    || die "front backend lost its canary security policy before ${OPERATION}"
  policy_json="$("${GCLOUD_BINARY}" compute security-policies describe "${EXPECTED_CANARY_SECURITY_POLICY}" \
    --project "${PROJECT_ID}" --global --format=json)"
  live_access="$(printf '%s\n' "${policy_json}" | python3 "${CANARY_SECURITY_POLICY_HELPER}" \
    assert-ready - "${EXPECTED_CANARY_SECURITY_POLICY}" "${EXPECTED_CANARY_HOST}")"
  expected_token_sha="$(jq -r '.routing.canary.access_control.key_fingerprint_sha256' "${PRIOR_EVIDENCE}")"
  [[ "$(jq -r '.key_fingerprint_sha256' < <(stream_shell_value "${live_access}"))" == "${expected_token_sha}" ]] \
    || die "live canary security policy no longer matches the preparation evidence"
}
utc_now() { python3 -c 'from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))'; }
epoch_millis() { python3 -c 'import time; print(time.time_ns() // 1_000_000)'; }
proof_attempt_path() {
  local base="$1" attempt="$2"
  if [[ "${attempt}" == 1 ]]; then printf '%s\n' "${base}"; else printf '%s.attempt-%s\n' "${base}" "${attempt}"; fi
}
destination_session_observed() {
  local session_id="$1" service
  [[ -n "${session_id}" && "${#session_id}" -le 256 && "${session_id}" =~ ^[A-Za-z0-9._:-]+$ ]] || return 1
  if [[ "${destination_kind}" == front ]]; then
    service="$(metric_service "${active_migration_slot}")"
  else
    service="$(metric_service legacy)"
  fi
  printf '%s\n' "${session_id}" | \
    "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
      --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet \
      --command "set -eu; IFS= read -r session_id; sudo journalctl --unit='${service}' --since='@${transition_requested_epoch}' --no-pager --output=cat | grep -F 'INFO proxy request agent=codex' | grep -F 'method=POST path=/v1/responses' | grep -F -e \"session=\${session_id} \" -e \"session=\${session_id}:\" -e \"session=\\\"\${session_id}\\\" \" -e \"session=\\\"\${session_id}:\" >/dev/null" \
    >/dev/null 2>&1
}

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
  local target="$1" output_name="$2" pid result peak
  pid="${rss_sampler_pids[${target}]}"
  result="${rss_sampler_results[${target}]}"
  gcloud_ssh "sudo rm -f '${rss_sampler_sentinels[${target}]}'"
  wait "${pid}" || die "${target} RSS sampler failed"
  peak="$(gcloud_ssh "sudo cat '${result}'" | tail -n 1)"
  [[ "${peak}" =~ ^[0-9]+$ ]] || die "${target} RSS sampler returned invalid data"
  unset "rss_sampler_pids[${target}]"
  printf -v "${output_name}" '%s' "${peak}"
}
stop_all_rss_samplers() {
  local target
  for target in "${!rss_sampler_pids[@]}"; do
    gcloud_ssh "sudo rm -f '${rss_sampler_sentinels[${target}]}'" >/dev/null 2>&1 || true
    wait "${rss_sampler_pids[${target}]}" >/dev/null 2>&1 || true
    unset "rss_sampler_pids[${target}]"
  done
}

persistent_legacy_sampler_started=0
persistent_legacy_sampler_label=""
persistent_legacy_sampler_unit=""
persistent_legacy_sampler_sentinel=""
persistent_legacy_sampler_result=""
persistent_legacy_sampler_oom_result=""
configure_persistent_legacy_sampler() {
  local label="$1"
  [[ "${label}" =~ ^[a-zA-Z0-9._-]+$ && "${#label}" -le 128 ]] \
    || die "persistent legacy sampler label is invalid"
  persistent_legacy_sampler_label="${label}"
  persistent_legacy_sampler_unit="subrouter-rss-${label}-legacy.service"
  persistent_legacy_sampler_sentinel="/tmp/subrouter-rss-${label}-legacy.running"
  persistent_legacy_sampler_result="/tmp/subrouter-rss-${label}-legacy.peak"
  persistent_legacy_sampler_oom_result="/tmp/subrouter-rss-${label}-legacy.oom"
}
configure_persistent_legacy_sampler "${RUN_LABEL}"
start_persistent_legacy_sampler() {
  gcloud_ssh "sudo rm -f '${persistent_legacy_sampler_result}' '${persistent_legacy_sampler_result}.tmp' '${persistent_legacy_sampler_oom_result}' '${persistent_legacy_sampler_oom_result}.tmp'; sudo touch '${persistent_legacy_sampler_sentinel}'; sudo systemctl reset-failed '${persistent_legacy_sampler_unit}' >/dev/null 2>&1 || true; sudo systemd-run --quiet --unit '${persistent_legacy_sampler_unit%.service}' --property=Type=exec /usr/bin/env SUBROUTER_DEPLOYMENT_CONTRACT='${REMOTE_DEPLOYMENT_CONTRACT}' /bin/bash '${REMOTE_INSTALLER}' sample-service-rss legacy '${persistent_legacy_sampler_label}'"
  persistent_legacy_sampler_started=1
  for _ in $(seq 1 100); do
    if gcloud_ssh "sudo test -s '${persistent_legacy_sampler_result}' -a -s '${persistent_legacy_sampler_oom_result}'" >/dev/null 2>&1; then
      return
    fi
    sleep 0.05
  done
  die "persistent legacy sampler did not produce its first sample"
}
read_persistent_legacy_sampler_status() {
  gcloud_ssh "state=\$(systemctl show '${persistent_legacy_sampler_unit}' -p ActiveState --value); result=\$(systemctl show '${persistent_legacy_sampler_unit}' -p Result --value); status=\$(systemctl show '${persistent_legacy_sampler_unit}' -p ExecMainStatus --value); printf '%s|%s|%s\\n' \"\${state}\" \"\${result}\" \"\${status}\"" | tail -n 1
}
adopt_persistent_legacy_sampler() {
  local unit_status state result status
  gcloud_ssh "sudo test -s '${persistent_legacy_sampler_result}' -a -s '${persistent_legacy_sampler_oom_result}'" \
    || die "committed handoff did not retain its continuous legacy sampler"
  unit_status="$(read_persistent_legacy_sampler_status)"
  IFS='|' read -r state result status <<<"${unit_status}"
  if [[ "${state}" == active ]]; then
    gcloud_ssh "sudo test -e '${persistent_legacy_sampler_sentinel}'" \
      || die "committed handoff sampler is active without its continuity sentinel"
  else
    [[ "${state}" == inactive && "${result}" == success && "${status}" == 0 ]] \
      || die "committed handoff sampler is ${unit_status}"
    # A Unix-socket pathname can survive its owner and become stale. Let the
    # installer prove the legacy units are inactive and remove only an
    # unowned stale socket. A pathname test alone rejects a valid resumable
    # handoff on VMs with that leftover inode.
    gcloud_ssh "! systemctl is-active --quiet subrouter.service" \
      || die "committed handoff sampler stopped before legacy service retirement"
    gcloud_ssh "${REMOTE_INSTALL_COMMAND} cleanup-stopped-legacy-control" \
      || die "committed handoff could not reconcile the stopped legacy control socket"
  fi
  persistent_legacy_sampler_started=1
}
read_persistent_legacy_sampler() {
  local peak_name="$1" oom_name="$2" peak oom
  peak="$(gcloud_ssh "sudo cat '${persistent_legacy_sampler_result}'" | tail -n 1)"
  oom="$(gcloud_ssh "sudo cat '${persistent_legacy_sampler_oom_result}'" | tail -n 1)"
  [[ "${peak}" =~ ^[0-9]+$ && "${oom}" =~ ^[0-9]+$ ]] \
    || die "persistent legacy sampler returned invalid metrics"
  printf -v "${peak_name}" '%s' "${peak}"
  printf -v "${oom_name}" '%s' "${oom}"
}
stop_persistent_legacy_sampler() {
  (( persistent_legacy_sampler_started == 1 )) || return 0
  gcloud_ssh "sudo rm -f '${persistent_legacy_sampler_sentinel}'; sudo systemctl stop '${persistent_legacy_sampler_unit}' >/dev/null 2>&1 || true"
  persistent_legacy_sampler_started=0
}

legacy_peak_rss=""
sampled_legacy_oom=""
slot_peak_rss=""
front_peak_rss=""

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
evidence_committed=0
resuming_handoff=0
evidence_run_label="${RUN_LABEL}"
before_yaml=""
handoff_checkpoint_json=""
acquire_lock() {
  subrouter_acquire_deploy_lock "${ARTIFACT_DIR}/deploy-lock.log" \
    "${GCLOUD_BINARY}" "${INSTANCE}" "${PROJECT_ID}" "${ZONE}" "${DEPLOY_LOCK_FILE}" "${RUN_LABEL}" \
    "${HANDOFF_CHECKPOINT}" \
    || die "could not acquire ${DEPLOY_LOCK_FILE}"
}
release_lock() {
  subrouter_release_deploy_lock
}
handoff_checkpoint_exists() {
  gcloud_ssh "sudo test -f '${HANDOFF_CHECKPOINT}'" >/dev/null 2>&1
}
load_handoff_checkpoint() {
  handoff_checkpoint_json="$(gcloud_ssh "set -eu; sudo test \"\$(stat -c '%u:%g:%a' '${HANDOFF_CHECKPOINT}')\" = '0:0:600'; sudo python3 '${REMOTE_DEPLOYMENT_CONTRACT}' validate-front-handoff-checkpoint '${HANDOFF_CHECKPOINT}' '${preparation_sha256}' '${PROJECT_ID}' '${ZONE}' '${INSTANCE}' '${active_migration_slot}' '${EXPECTED_CONNECTIONS}'")"
  jq -e '.schema == "subrouter.gcp.front-handoff-checkpoint/v1"' \
    < <(stream_shell_value "${handoff_checkpoint_json}") >/dev/null \
    || die "committed front handoff checkpoint is invalid"
}
ensure_legacy_units_disabled() {
  gcloud_ssh "sudo systemctl disable subrouter.service >/dev/null; sudo systemctl disable subrouter.socket >/dev/null 2>&1 || true; ! systemctl is-enabled --quiet subrouter.service; ! systemctl is-enabled --quiet subrouter.socket" \
    || die "legacy boot units remained enabled after listener handoff"
}
commit_handoff_checkpoint() {
  local checkpoint_path="$1"
  gcloud_scp "${checkpoint_path}" "${REMOTE_HANDOFF_UPLOAD}"
  gcloud_ssh "set -euo pipefail
    sudo install -d -o root -g root -m 0755 /var/lib/subrouter
    checkpoint_tmp='${HANDOFF_CHECKPOINT}.tmp-${RUN_LABEL}'
    service_enabled=0
    socket_enabled=0
    systemctl is-enabled --quiet subrouter.service && service_enabled=1 || true
    systemctl is-enabled --quiet subrouter.socket && socket_enabled=1 || true
    committed=0
    restore_units() {
      if test \"\${committed}\" = 0; then
        sudo rm -f \"\${checkpoint_tmp}\"
        test \"\${service_enabled}\" = 0 || sudo systemctl enable subrouter.service >/dev/null 2>&1 || true
        test \"\${socket_enabled}\" = 0 || sudo systemctl enable subrouter.socket >/dev/null 2>&1 || true
      fi
    }
    trap restore_units EXIT INT TERM
    sudo install -o root -g root -m 0600 '${REMOTE_HANDOFF_UPLOAD}' \"\${checkpoint_tmp}\"
    sudo python3 '${REMOTE_DEPLOYMENT_CONTRACT}' validate-front-handoff-checkpoint \
      \"\${checkpoint_tmp}\" '${preparation_sha256}' '${PROJECT_ID}' '${ZONE}' '${INSTANCE}' \
      '${active_migration_slot}' '${EXPECTED_CONNECTIONS}' >/dev/null
    sudo systemctl disable subrouter.service >/dev/null
    sudo systemctl disable subrouter.socket >/dev/null 2>&1 || true
    ! systemctl is-enabled --quiet subrouter.service
    ! systemctl is-enabled --quiet subrouter.socket
    sudo mv -f -- \"\${checkpoint_tmp}\" '${HANDOFF_CHECKPOINT}'
    committed=1
    trap - EXIT INT TERM
    test \"\$(stat -c '%u:%g:%a' '${HANDOFF_CHECKPOINT}')\" = '0:0:600'"
  load_handoff_checkpoint
}
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  stop_all_rss_samplers
  if [[ "${transition_started}" == 1 && "${transition_committed}" == 0 ]] && \
     handoff_checkpoint_exists; then
    transition_committed=1
  fi
  if [[ "${transition_started}" == 1 && "${transition_committed}" == 0 ]]; then
    log "listener handoff did not commit; restoring the front bootstrap listener" >&2
    if ! gcloud_ssh "${REMOTE_INSTALL_COMMAND} restore-front-bootstrap"; then
      log "failed to restore the front bootstrap listener" >&2
      status=1
    fi
  fi
  if [[ "${evidence_committed}" == 0 && "${transition_committed}" == 0 ]]; then
    stop_persistent_legacy_sampler >/dev/null 2>&1 || status=1
  fi
  gcloud_ssh "rm -f '${REMOTE_INSTALLER}' '${REMOTE_DEPLOYMENT_CONTRACT}' '${REMOTE_HANDOFF_UPLOAD}'" >/dev/null 2>&1 || true
  release_lock
  exit "${status}"
}
trap cleanup EXIT INT TERM

acquire_lock
assert_canary_access_boundary
gcloud_scp "${INSTALL_FRONT_SLOTS}" "${REMOTE_INSTALLER}"
gcloud_scp "${DEPLOYMENT_CONTRACT}" "${REMOTE_DEPLOYMENT_CONTRACT}"
gcloud_ssh "printf '%s  %s\n%s  %s\n' '${INSTALL_FRONT_SLOTS_SHA256}' '${REMOTE_INSTALLER}' '${DEPLOYMENT_CONTRACT_SHA256}' '${REMOTE_DEPLOYMENT_CONTRACT}' | sha256sum -c - >/dev/null"
active_migration_slot="$(jq -r '.slot' < <(stream_shell_value "${front_json}"))"
if handoff_checkpoint_exists; then
  load_handoff_checkpoint
  resuming_handoff=1
  transition_started=1
  transition_committed=1
  evidence_run_label="$(jq -r '.run.id' < <(stream_shell_value "${handoff_checkpoint_json}"))"
  configure_persistent_legacy_sampler "${evidence_run_label}"
  adopt_persistent_legacy_sampler
  source_before="$(jq -c '.source.before' < <(stream_shell_value "${handoff_checkpoint_json}"))"
  source_after="$(jq -c '.source.after' < <(stream_shell_value "${handoff_checkpoint_json}"))"
  source_listener_pid="$(jq -r '.listener.source_pid' < <(stream_shell_value "${handoff_checkpoint_json}"))"
  source_listener_fd="$(jq -r '.listener.source_fd' < <(stream_shell_value "${handoff_checkpoint_json}"))"
  source_listener_inode="$(jq -r '.listener.inode' < <(stream_shell_value "${handoff_checkpoint_json}"))"
  legacy_restarts_before="$(jq -r '.metrics.legacy.nrestarts' < <(stream_shell_value "${handoff_checkpoint_json}"))"
  legacy_oom_before="$(jq -r '.metrics.legacy.oom_kill' < <(stream_shell_value "${handoff_checkpoint_json}"))"
  slot_restarts_before="$(jq -r '.metrics.slot.nrestarts' < <(stream_shell_value "${handoff_checkpoint_json}"))"
  slot_oom_before="$(jq -r '.metrics.slot.oom_kill' < <(stream_shell_value "${handoff_checkpoint_json}"))"
  front_restarts_before="$(jq -r '.metrics.front.nrestarts' < <(stream_shell_value "${handoff_checkpoint_json}"))"
  front_oom_before="$(jq -r '.metrics.front.oom_kill' < <(stream_shell_value "${handoff_checkpoint_json}"))"
  checkpoint_front_listener="$(gcloud_ssh "${REMOTE_INSTALL_COMMAND} listener-status subrouter-front.service 31415" | tail -n 1)"
  jq -e --arg inode "${source_listener_inode}" \
    '.service == "subrouter-front.service" and .port == 31415 and .inode == $inode' \
    < <(stream_shell_value "${checkpoint_front_listener}") >/dev/null \
    || die "stable front no longer owns the checkpointed public listener"
  destination_listener_pid="$(jq -r '.pid' < <(stream_shell_value "${checkpoint_front_listener}"))"
  destination_listener_fd="$(jq -r '.fd' < <(stream_shell_value "${checkpoint_front_listener}"))"
  destination_listener_inode="$(jq -r '.inode' < <(stream_shell_value "${checkpoint_front_listener}"))"
  ensure_legacy_units_disabled
  log "resuming committed listener handoff ${evidence_run_label}"
else
  legacy_restarts_before="$(service_restarts legacy)"
  legacy_oom_before="$(service_oom_kills legacy)"
  slot_restarts_before="$(service_restarts "${active_migration_slot}")"
  slot_oom_before="$(service_oom_kills "${active_migration_slot}")"
  front_restarts_before="$(service_restarts front)"
  front_oom_before="$(service_oom_kills front)"
  start_persistent_legacy_sampler
fi
start_rss_sampler "${active_migration_slot}"
start_rss_sampler front
before_yaml="${ARTIFACT_DIR}/url-map-before.yaml"
after_yaml="${ARTIFACT_DIR}/url-map-after.yaml"
"${GCLOUD_BINARY}" compute url-maps export "${URL_MAP}" --project "${PROJECT_ID}" --global \
  --destination "${before_yaml}" --quiet
source_url="${legacy_backend_url}"
destination_url="${legacy_backend_url}"
python3 "${URL_MAP_ROUTING}" assert-state \
  "${before_yaml}" "${ACTIVE_MATCHER}" "${legacy_backend_url}" \
  "${CANARY_MATCHER}" "${CANARY_HOST}" "${front_backend_url}"
destination_before="$(supervisor_snapshot "${destination_kind}")"
transition_requested_at="$(utc_now)"
transition_requested_ms="$(epoch_millis)"
transition_requested_epoch=$((transition_requested_ms / 1000))
if (( resuming_handoff == 0 )); then
  source_before="$(supervisor_snapshot "${source_kind}")"
  jq -e --argjson expected "${EXPECTED_CONNECTIONS}" \
    '.public_connections >= $expected and .generation_connections >= $expected and .inactive_connections == 0' \
    < <(stream_shell_value "${source_before}") >/dev/null \
    || die "source did not correlate every external migration connection"
  legacy_listener="$(gcloud_ssh "${REMOTE_INSTALL_COMMAND} listener-status '${LEGACY_SERVICE:-subrouter.service}' 31415" | tail -n 1)"
  jq -e '.service == "subrouter.service" and (.pid|type)=="number" and (.fd|type)=="number" and
    (.inode|type)=="string" and (.port == 31415)' \
    < <(stream_shell_value "${legacy_listener}") >/dev/null || die "legacy listener ownership is invalid"
  source_listener_pid="$(jq -r '.pid' < <(stream_shell_value "${legacy_listener}"))"
  source_listener_fd="$(jq -r '.fd' < <(stream_shell_value "${legacy_listener}"))"
  source_listener_inode="$(jq -r '.inode' < <(stream_shell_value "${legacy_listener}"))"
  [[ "${source_listener_inode}" =~ ^socket:\[[0-9]+\]$ ]] \
    || die "legacy listener inode is not a socket identity"

  transition_started=1
  gcloud_ssh "${REMOTE_INSTALL_COMMAND} activate-front-takeover '${source_listener_pid}' '${source_listener_fd}'"
  front_listener="$(gcloud_ssh "${REMOTE_INSTALL_COMMAND} listener-status subrouter-front.service 31415" | tail -n 1)"
  destination_listener_pid="$(jq -r '.pid' < <(stream_shell_value "${front_listener}"))"
  destination_listener_fd="$(jq -r '.fd' < <(stream_shell_value "${front_listener}"))"
  destination_listener_inode="$(jq -r '.inode' < <(stream_shell_value "${front_listener}"))"
  [[ "${destination_listener_inode}" == "${source_listener_inode}" ]] \
    || die "front did not take ownership of the exact legacy listener"
  source_after="$(supervisor_snapshot "${source_kind}")"
  jq -e --argjson expected "${EXPECTED_CONNECTIONS}" \
    '.public_connections >= $expected and .generation_connections >= $expected and .inactive_connections == 0' \
    < <(stream_shell_value "${source_after}") >/dev/null \
    || die "listener handoff cut externally held source connections"
else
  front_listener="$(gcloud_ssh "${REMOTE_INSTALL_COMMAND} listener-status subrouter-front.service 31415" | tail -n 1)"
  jq -e --arg inode "${source_listener_inode}" \
    '.service == "subrouter-front.service" and .port == 31415 and .inode == $inode' \
    < <(stream_shell_value "${front_listener}") >/dev/null \
    || die "stable front no longer owns the checkpointed public listener"
  destination_listener_pid="$(jq -r '.pid' < <(stream_shell_value "${front_listener}"))"
  destination_listener_fd="$(jq -r '.fd' < <(stream_shell_value "${front_listener}"))"
  destination_listener_inode="$(jq -r '.inode' < <(stream_shell_value "${front_listener}"))"
fi
"${GCLOUD_BINARY}" compute url-maps export "${URL_MAP}" --project "${PROJECT_ID}" --global \
  --destination "${after_yaml}" --quiet
python3 "${URL_MAP_ROUTING}" assert-state \
  "${after_yaml}" "${ACTIVE_MATCHER}" "${legacy_backend_url}" \
  "${CANARY_MATCHER}" "${CANARY_HOST}" "${front_backend_url}"
source_snapshot_sha256="$(printf '%s' "${source_after}" | sha256sum | awk '{print $1}')"
if (( resuming_handoff == 0 )); then
  handoff_completed_at="$(utc_now)"
  handoff_checkpoint_path="${ARTIFACT_DIR}/front-handoff-checkpoint-${RUN_LABEL}.json"
  handoff_checkpoint_tmp="$(mktemp "${handoff_checkpoint_path}.tmp.XXXXXX")"
  jq -n --arg schema 'subrouter.gcp.front-handoff-checkpoint/v1' \
    --arg preparation_sha "${preparation_sha256}" --arg run_id "${RUN_LABEL}" \
    --arg project "${PROJECT_ID}" --arg zone "${ZONE}" --arg instance "${INSTANCE}" \
    --arg slot "${active_migration_slot}" --arg completed_at "${handoff_completed_at}" \
    --argjson source_pid "${source_listener_pid}" --argjson source_fd "${source_listener_fd}" \
    --arg listener_inode "${source_listener_inode}" \
    --argjson source_before "${source_before}" --argjson source_after "${source_after}" \
    --argjson legacy_restarts "${legacy_restarts_before}" --argjson legacy_oom "${legacy_oom_before}" \
    --argjson slot_restarts "${slot_restarts_before}" --argjson slot_oom "${slot_oom_before}" \
    --argjson front_restarts "${front_restarts_before}" --argjson front_oom "${front_oom_before}" \
    '{schema:$schema,preparation_evidence_sha256:$preparation_sha,
      run:{id:$run_id,project:$project,zone:$zone,instance:$instance},slot:$slot,
      listener:{source_pid:$source_pid,source_fd:$source_fd,inode:$listener_inode},
      source:{before:$source_before,after:$source_after},
      metrics:{legacy:{nrestarts:$legacy_restarts,oom_kill:$legacy_oom},
        slot:{nrestarts:$slot_restarts,oom_kill:$slot_oom},
        front:{nrestarts:$front_restarts,oom_kill:$front_oom}},
      handoff_completed_at:$completed_at}' >"${handoff_checkpoint_tmp}"
  python3 "${DEPLOYMENT_CONTRACT}" validate-front-handoff-checkpoint \
    "${handoff_checkpoint_tmp}" "${preparation_sha256}" "${PROJECT_ID}" "${ZONE}" \
    "${INSTANCE}" "${active_migration_slot}" "${EXPECTED_CONNECTIONS}" >/dev/null
  chmod 0600 "${handoff_checkpoint_tmp}"
  mv -f -- "${handoff_checkpoint_tmp}" "${handoff_checkpoint_path}"
  commit_handoff_checkpoint "${handoff_checkpoint_path}"
  transition_committed=1
fi
# The durable checkpoint and disabled legacy boot units make this stop
# resumable. Later failures preserve the front listener and sampler.
gcloud_ssh "sudo systemctl stop --no-block subrouter.service"
gcloud_ssh "for i in \$(seq 1 200); do current=\$(sudo readlink '/proc/${source_listener_pid}/fd/${source_listener_fd}' 2>/dev/null || true); test \"\${current}\" != '${source_listener_inode}' && exit 0; sleep 0.05; done; exit 1"
source_listener_retired_at="$(utc_now)"
front_listener_after="$(gcloud_ssh "${REMOTE_INSTALL_COMMAND} listener-status subrouter-front.service 31415" | tail -n 1)"
[[ "$(jq -r '.inode' < <(stream_shell_value "${front_listener_after}"))" == "${source_listener_inode}" ]] \
  || die "front lost the inherited listener while the legacy owner retired"
if [[ "${destination_kind}" == front ]]; then
  destination_generation="$(jq -r '.generation' < <(stream_shell_value "${front_json}"))"
else
  destination_generation="$(jq -r '.generation' < <(stream_shell_value "${legacy_json}"))"
fi
proof_max_attempts=64
proof_attempt=1
destination_correlated=false
while (( proof_attempt <= proof_max_attempts )); do
  proof_request_path="$(proof_attempt_path "${DESTINATION_PROOF_REQUEST}" "${proof_attempt}")"
  proof_path="$(proof_attempt_path "${DESTINATION_PROOF}" "${proof_attempt}")"
  liveness_request_path="${proof_path}.liveness-request"
  liveness_proof_path="${proof_path}.liveness-proof"
  [[ ! -e "${proof_request_path}" && ! -L "${proof_request_path}" && ! -e "${proof_path}" && ! -L "${proof_path}" &&
     ! -e "${liveness_request_path}" && ! -L "${liveness_request_path}" &&
     ! -e "${liveness_proof_path}" && ! -L "${liveness_proof_path}" ]] \
    || die "destination proof attempt path already exists"
  proof_challenge="$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
  proof_request_tmp="$(mktemp "${proof_request_path}.tmp.XXXXXX")"
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
  mv -f -- "${proof_request_tmp}" "${proof_request_path}"

  while [[ ! -f "${proof_path}" || -L "${proof_path}" ]]; do
    (( $(epoch_millis) - transition_requested_ms < MIGRATION_PROPAGATION_LIMIT_MS )) \
      || die "golden destination proof was not observed within the five-minute route propagation window"
    sleep 0.05
  done
  proof_received_at="$(utc_now)"
  proof_received_ms="$(epoch_millis)"
  (( proof_received_ms - transition_requested_ms < MIGRATION_PROPAGATION_LIMIT_MS )) \
    || die "golden destination proof arrived outside the five-minute route propagation window"
  python3 "${DEPLOYMENT_CONTRACT}" validate-destination-proof \
    "${proof_path}" "${proof_challenge}" "${OPERATION}" \
    "${destination_kind}" "${destination_generation}" "${source_kind}" \
    "$(jq -r '.generation' < <(stream_shell_value "${source_after}"))" "${source_snapshot_sha256}" \
    "${EXPECTED_CONNECTIONS}" "${transition_requested_at}" \
    "${proof_received_at}"
  destination_session_id="$(jq -r '.session_id // empty' "${proof_path}")"
  [[ -n "${destination_session_id}" && "${#destination_session_id}" -le 256 && "${destination_session_id}" =~ ^[A-Za-z0-9._:-]+$ ]] \
    || die "golden destination proof session ID is invalid"
  if destination_session_observed "${destination_session_id}"; then
    destination_correlated=true
    activated_at="$(jq -r '.observed_at' "${proof_path}")"
    destination_proof_sha256="$(sha256sum "${proof_path}" | awk '{print $1}')"
    destination_connection_id="$(jq -r '.connection_id' "${proof_path}")"
    break
  fi
  log "destination proof attempt ${proof_attempt} remained on ${source_kind}; retrying while original connections stay pinned"
  proof_attempt=$((proof_attempt + 1))
done
[[ "${destination_correlated}" == true ]] \
  || die "golden fresh public session did not reach the destination within ${proof_max_attempts} attempts"
destination_after="$(supervisor_snapshot "${destination_kind}")"
# Earlier proof attempts drain asynchronously, so their departures can hide the
# accepted proof in an aggregate before/after delta. Require a live connection
# on the exact generation, then challenge the golden observer to produce a
# response chunk on the accepted transport after this snapshot.
jq -e --arg generation "${destination_generation}" \
  '.generation == $generation and .public_connections >= 1 and
   .generation_connections >= 1 and .inactive_connections == 0' \
  < <(stream_shell_value "${destination_after}") >/dev/null \
  || die "golden fresh public connection was not correlated to the destination generation"
destination_snapshot_canonical="$(printf '%s' "${destination_after}" | jq -cS .)"
destination_snapshot_sha256="$(printf '%s' "${destination_snapshot_canonical}" | sha256sum | awk '{print $1}')"
liveness_challenge="$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
liveness_requested_at="$(utc_now)"
liveness_requested_ms="$(epoch_millis)"
liveness_request_tmp="$(mktemp "${liveness_request_path}.tmp.XXXXXX")"
jq -n --arg schema 'subrouter.gcp.destination-liveness-request/v1' \
  --arg challenge "${liveness_challenge}" --arg operation "${OPERATION}" \
  --arg destination "${destination_kind}" --arg generation "${destination_generation}" \
  --arg connection_id "${destination_connection_id}" --arg session_id "${destination_session_id}" \
  --arg snapshot_sha "${destination_snapshot_sha256}" --arg requested_at "${liveness_requested_at}" \
  '{schema:$schema,challenge:$challenge,operation:$operation,destination:$destination,
    destination_generation:$generation,connection_id:$connection_id,session_id:$session_id,
    destination_snapshot_sha256:$snapshot_sha,requested_at:$requested_at}' \
  >"${liveness_request_tmp}"
chmod 0600 "${liveness_request_tmp}"
mv -f -- "${liveness_request_tmp}" "${liveness_request_path}"
while [[ ! -f "${liveness_proof_path}" || -L "${liveness_proof_path}" ]]; do
  (( $(epoch_millis) - liveness_requested_ms < DESTINATION_LIVENESS_LIMIT_MS )) \
    || die "golden destination connection produced no response chunk after the destination snapshot"
  sleep 0.05
done
liveness_received_at="$(utc_now)"
python3 "${DEPLOYMENT_CONTRACT}" validate-destination-liveness \
  "${liveness_proof_path}" "${liveness_challenge}" "${OPERATION}" \
  "${destination_kind}" "${destination_generation}" "${destination_connection_id}" \
  "${destination_session_id}" "${destination_snapshot_sha256}" "${liveness_requested_at}" \
  "${liveness_received_at}"
liveness_proof_sha256="$(sha256sum "${liveness_proof_path}" | awk '{print $1}')"
liveness_response_chunk_at="$(jq -r '.response_chunk_at' "${liveness_proof_path}")"
destination_connection_delta="$(( $(jq -r '.generation_connections' < <(stream_shell_value "${destination_after}")) - $(jq -r '.generation_connections' < <(stream_shell_value "${destination_before}")) ))"
legacy_restarts_after="$(service_restarts legacy)"
read_persistent_legacy_sampler legacy_peak_rss sampled_legacy_oom
legacy_oom_after="${sampled_legacy_oom}"
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
stop_rss_sampler "${active_migration_slot}" slot_peak_rss
stop_rss_sampler front front_peak_rss
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
  --arg preparation_sha "${preparation_sha256}" --arg run_id "${evidence_run_label}" \
  --arg project "${PROJECT_ID}" --arg zone "${ZONE}" --arg instance "${INSTANCE}" \
  --argjson release "${release_json}" --argjson bootstrap "${bootstrap_json}" \
  --argjson predecessor "${predecessor_json}" \
  --argjson routing "${routing_json}" \
  --argjson legacy "${legacy_json}" --argjson front "${front_json}" \
  --arg source "${source_kind}" --arg destination "${destination_kind}" \
  --arg source_url "${source_url}" --arg destination_url "${destination_url}" \
  --arg requested_at "${transition_requested_at}" --arg activated_at "${activated_at}" \
  --arg source_listener_retired_at "${source_listener_retired_at}" \
  --arg proof_received_at "${proof_received_at}" --arg emitted_at "${evidence_emitted_at}" \
  --arg proof_sha "${destination_proof_sha256}" --arg challenge "${proof_challenge}" \
  --arg connection_id "${destination_connection_id}" --arg session_id "${destination_session_id}" \
  --arg liveness_sha "${liveness_proof_sha256}" --arg liveness_challenge "${liveness_challenge}" \
  --arg snapshot_sha "${destination_snapshot_sha256}" --arg liveness_requested_at "${liveness_requested_at}" \
  --arg liveness_response_chunk_at "${liveness_response_chunk_at}" \
  --arg liveness_received_at "${liveness_received_at}" \
  --argjson before "${source_before}" \
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
  --argjson source_listener_pid "${source_listener_pid}" --argjson source_listener_fd "${source_listener_fd}" \
  --arg source_listener_inode "${source_listener_inode}" \
  --argjson destination_listener_pid "${destination_listener_pid}" \
  --argjson destination_listener_fd "${destination_listener_fd}" \
  --arg destination_listener_inode "${destination_listener_inode}" \
  '{schema:$schema,evidence_type:$evidence_type,mode:$mode,success:true,
    prior_evidence_type:$prior_type,prior_evidence_sha256:$prior_sha,
    preparation_evidence_sha256:$preparation_sha,
    run:{id:$run_id,project:$project,zone:$zone,instance:$instance},release:$release,
    bootstrap:$bootstrap,predecessor:$predecessor,
    routing:($routing + {before:$source,after:$destination,source_backend_url:$source_url,
      destination_backend_url:$destination_url,mechanism:"listener-fd-takeover"}),legacy:$legacy,front:$front,
    listener:{source_pid:$source_listener_pid,source_fd:$source_listener_fd,
      source_inode:$source_listener_inode,destination_pid:$destination_listener_pid,
      destination_fd:$destination_listener_fd,destination_inode:$destination_listener_inode,
      same_kernel_socket:($source_listener_inode==$destination_listener_inode)},
    timestamps:{transition_requested_at:$requested_at,activated_at:$activated_at,
      source_listener_retired_at:$source_listener_retired_at,
      evidence_emitted_at:$emitted_at},
    destination_proof:{sha256:$proof_sha,challenge:$challenge,connection_id:$connection_id,
      session_id:$session_id,
      original_continuity_verified:true,fresh_public_connection:true,journal_correlated:true,
      observed_at:$activated_at,received_at:$proof_received_at,
      post_snapshot_liveness:{sha256:$liveness_sha,challenge:$liveness_challenge,
        connection_id:$connection_id,session_id:$session_id,
        destination_snapshot_sha256:$snapshot_sha,requested_at:$liveness_requested_at,
        response_chunk_at:$liveness_response_chunk_at,received_at:$liveness_received_at}},
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
    rollback:{required:false,performed:false}}' \
  >"${evidence_tmp}"
python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect "${evidence_type}" "${evidence_tmp}" >/dev/null
chmod 0600 "${evidence_tmp}"
mv -f -- "${evidence_tmp}" "${EVIDENCE_JSON}"
evidence_committed=1
log "${OPERATION} passed: ${source_kind} -> ${destination_kind}; source connections remain live"
jq -c . "${EVIDENCE_JSON}"
