#!/usr/bin/env bash
# Read-only release and live-topology proof. Every deployment mutation belongs
# to the local Mac golden harness under its remote deployment lock.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: preflight-deployment.sh [--evidence-json PATH]

Reads public health, GCP routing, and VM supervisor state. It never uploads,
installs, starts, stops, or changes a GCP resource.
EOF
}

EVIDENCE_JSON=""
while (( $# > 0 )); do
  case "$1" in
    --evidence-json) (( $# >= 2 )) || { usage >&2; exit 2; }; EVIDENCE_JSON="$2"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

PROJECT_ID="${SUBROUTER_GCP_PROJECT:?set SUBROUTER_GCP_PROJECT}"
ZONE="${SUBROUTER_GCP_ZONE:?set SUBROUTER_GCP_ZONE}"
INSTANCE="${SUBROUTER_GCP_INSTANCE:?set SUBROUTER_GCP_INSTANCE}"
MODE="${SUBROUTER_PREFLIGHT_TOPOLOGY:?set SUBROUTER_PREFLIGHT_TOPOLOGY}"
RELEASE_TAG="${SUBROUTER_RELEASE_TAG:?set SUBROUTER_RELEASE_TAG}"
DEPLOY_BINARY="${SUBROUTER_DEPLOY_BINARY:?set SUBROUTER_DEPLOY_BINARY}"
RELEASE_SHA256_FILE="${SUBROUTER_RELEASE_SHA256_FILE:-${DEPLOY_BINARY}.sha256}"
DEPLOY_REVISION="${SUBROUTER_DEPLOY_REVISION:?set SUBROUTER_DEPLOY_REVISION}"
TAG_ON_MAIN="${SUBROUTER_RELEASE_TAG_ON_MAIN:?set SUBROUTER_RELEASE_TAG_ON_MAIN}"
ATTESTATION_VERIFIED="${SUBROUTER_RELEASE_ATTESTATION_VERIFIED:?set SUBROUTER_RELEASE_ATTESTATION_VERIFIED}"
RELEASE_IMMUTABLE="${SUBROUTER_RELEASE_IMMUTABLE:?set SUBROUTER_RELEASE_IMMUTABLE}"
PUBLIC_BASE_URL="${SUBROUTER_PUBLIC_BASE_URL:?set SUBROUTER_PUBLIC_BASE_URL}"
GCLOUD_BINARY="${GCLOUD_BIN:-gcloud}"
URL_MAP="${SUBROUTER_GCP_URL_MAP:-subrouter-urlmap}"
ARTIFACT_DIR="${SUBROUTER_DEPLOY_ARTIFACT_DIR:-${PWD}/artifacts/gcp-preflight}"
RUN_LABEL="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-preflight-$$"
RUN_LABEL="${RUN_LABEL//[^a-zA-Z0-9._-]/-}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/gcp/stream-shell-value.sh
source "${SCRIPT_DIR}/stream-shell-value.sh"

case "${INSTANCE}" in
  subrouter-team)
    LEGACY_BACKEND_SERVICE="${SUBROUTER_GCP_BACKEND_SERVICE:-subrouter-backend}"
    FRONT_BACKEND_SERVICE="${SUBROUTER_GCP_FRONT_BACKEND_SERVICE:-subrouter-front-backend}"
    INSTANCE_GROUP="${SUBROUTER_GCP_INSTANCE_GROUP:-subrouter-ig}"
    ACTIVE_MATCHER="__root__"
    CANARY_MATCHER="subrouter-front-canary"
    CANARY_HOST="front-canary.sr.cmux.internal"
    CANARY_SECURITY_POLICY="subrouter-front-canary-policy"
    ;;
  subrouter-staging)
    LEGACY_BACKEND_SERVICE="${SUBROUTER_GCP_BACKEND_SERVICE:-subrouter-staging-backend}"
    FRONT_BACKEND_SERVICE="${SUBROUTER_GCP_FRONT_BACKEND_SERVICE:-subrouter-staging-front-backend}"
    INSTANCE_GROUP="${SUBROUTER_GCP_INSTANCE_GROUP:-subrouter-staging-ig}"
    ACTIVE_MATCHER="staging-subrouter"
    CANARY_MATCHER="staging-subrouter-front-canary"
    CANARY_HOST="front-canary.staging.sr.cmux.internal"
    CANARY_SECURITY_POLICY="subrouter-staging-front-canary-policy"
    ;;
  *)
    LEGACY_BACKEND_SERVICE="${SUBROUTER_GCP_BACKEND_SERVICE:?set SUBROUTER_GCP_BACKEND_SERVICE}"
    FRONT_BACKEND_SERVICE="${SUBROUTER_GCP_FRONT_BACKEND_SERVICE:?set SUBROUTER_GCP_FRONT_BACKEND_SERVICE}"
    INSTANCE_GROUP="${SUBROUTER_GCP_INSTANCE_GROUP:?set SUBROUTER_GCP_INSTANCE_GROUP}"
    ACTIVE_MATCHER="${SUBROUTER_GCP_ACTIVE_MATCHER:?set SUBROUTER_GCP_ACTIVE_MATCHER}"
    CANARY_MATCHER="${SUBROUTER_GCP_CANARY_MATCHER:?set SUBROUTER_GCP_CANARY_MATCHER}"
    CANARY_HOST="${SUBROUTER_GCP_CANARY_HOST:?set SUBROUTER_GCP_CANARY_HOST}"
    CANARY_SECURITY_POLICY="${SUBROUTER_GCP_CANARY_SECURITY_POLICY:-}"
    ;;
esac

log() { printf 'gcp-preflight: %s\n' "$*"; }
die() { log "$*" >&2; exit 1; }
for command in "${GCLOUD_BINARY}" curl jq python3; do
  command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done
if command -v sha256sum >/dev/null 2>&1; then
  hash_file() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  hash_file() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  die "required command not found: sha256sum or shasum"
fi
[[ "${MODE}" == slot || "${MODE}" == migrate-front ]] || die "preflight topology must be slot or migrate-front"
[[ "${RELEASE_TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || die "release tag is invalid"
[[ -x "${DEPLOY_BINARY}" && -f "${RELEASE_SHA256_FILE}" ]] || die "verified release asset is missing"
EXPECTED_SHA256="$(tr -d '[:space:]' <"${RELEASE_SHA256_FILE}")"
[[ "${EXPECTED_SHA256}" =~ ^[0-9a-f]{64}$ ]] || die "release checksum is invalid"
[[ "$(hash_file "${DEPLOY_BINARY}")" == "${EXPECTED_SHA256}" ]] || die "release checksum changed"
[[ "${DEPLOY_REVISION}" =~ ^[0-9a-f]{40}$ ]] || die "release revision is invalid"
[[ "${TAG_ON_MAIN}" == true && "${ATTESTATION_VERIFIED}" == true && "${RELEASE_IMMUTABLE}" == true ]] \
  || die "release trust proof is incomplete"
[[ "${PUBLIC_BASE_URL}" =~ ^https://[^/?#]+/?$ ]] || die "public base URL must be an HTTPS origin"
PUBLIC_BASE_URL="${PUBLIC_BASE_URL%/}"

mkdir -p "${ARTIFACT_DIR}"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd)"
EVIDENCE_JSON="${EVIDENCE_JSON:-${ARTIFACT_DIR}/result.json}"
mkdir -p "$(dirname "${EVIDENCE_JSON}")"
EVIDENCE_JSON="$(cd "$(dirname "${EVIDENCE_JSON}")" && pwd)/$(basename "${EVIDENCE_JSON}")"

gcloud_ssh() {
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" --project "${PROJECT_ID}" --zone "${ZONE}" \
    --tunnel-through-iap --quiet --command "$1"
}
verify_legacy_backend_port() {
  local backend_json expected_group_url
  backend_json="$("${GCLOUD_BINARY}" compute backend-services describe "${LEGACY_BACKEND_SERVICE}" \
    --project "${PROJECT_ID}" --global --format=json)"
  expected_group_url="https://www.googleapis.com/compute/v1/projects/${PROJECT_ID}/zones/${ZONE}/instanceGroups/${INSTANCE_GROUP}"
  jq -e --arg group_url "${expected_group_url}" \
    '.portName == "http" and .protocol == "HTTP" and
     (.backends | type == "array" and length == 1) and
     .backends[0].group == $group_url' \
    < <(stream_shell_value "${backend_json}") >/dev/null \
    || die "legacy backend is not pinned to the http:31415 listener"
  jq -e '[.namedPorts[]? | select(.name == "http" and .port == 31415)] | length == 1' \
    < <(stream_shell_value "${group_json}") >/dev/null \
    || die "instance group http:31415 mapping is missing"
  backend_port_verified=true
  legacy_backend_port_name="http"
  instance_group_http_port=31415
}
utc_now() { python3 -c 'from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))'; }

curl -fsS --max-time 10 "${PUBLIC_BASE_URL}/_subrouter/health" >/dev/null || die "public health failed"
curl -fsS --max-time 10 "${PUBLIC_BASE_URL}/_subrouter/ready" >/dev/null || die "public readiness failed"
url_map_json="$("${GCLOUD_BINARY}" compute url-maps describe "${URL_MAP}" --project "${PROJECT_ID}" --global --format=json)"
group_json="$("${GCLOUD_BINARY}" compute instance-groups describe "${INSTANCE_GROUP}" --project "${PROJECT_ID}" --zone "${ZONE}" --format=json)"
URL_MAP_YAML="${ARTIFACT_DIR}/url-map-${RUN_LABEL}.yaml"
"${GCLOUD_BINARY}" compute url-maps export "${URL_MAP}" --project "${PROJECT_ID}" --global \
  --destination "${URL_MAP_YAML}" --quiet
legacy_backend_url="https://www.googleapis.com/compute/v1/projects/${PROJECT_ID}/global/backendServices/${LEGACY_BACKEND_SERVICE}"
front_backend_url="https://www.googleapis.com/compute/v1/projects/${PROJECT_ID}/global/backendServices/${FRONT_BACKEND_SERVICE}"
legacy_refs="$(jq -r --arg value "${legacy_backend_url}" '[.. | strings | select(. == $value)] | length' < <(stream_shell_value "${url_map_json}"))"
front_refs="$(jq -r --arg value "${front_backend_url}" '[.. | strings | select(. == $value)] | length' < <(stream_shell_value "${url_map_json}"))"
active_backend_url=""
canary_backend_url="${front_backend_url}"
route_assignments_verified=false
canary_present=false
canary_access_control_json='null'
backend_port_verified=false
legacy_backend_port_name=""
instance_group_http_port=0

if [[ "${MODE}" == migrate-front ]]; then
  [[ "${legacy_refs}" == 1 && "${front_refs}" == 0 ]] || die "URL map is not exactly on the legacy backend"
  active_backend_url="$(python3 "${SCRIPT_DIR}/url-map-routing.py" active-backend \
    "${URL_MAP_YAML}" "${ACTIVE_MATCHER}")" \
    || die "URL map active route could not be parsed"
  [[ "${active_backend_url}" == "${legacy_backend_url}" ]] \
    || die "URL map active route does not point to the legacy backend"
  route_assignments_verified=true
  verify_legacy_backend_port
  front_state="$(gcloud_ssh "if systemctl is-active --quiet subrouter-front.service || sudo test -S /var/lib/subrouter/front.sock; then echo present; else echo absent; fi" | tail -n 1)"
  [[ "${front_state}" == absent ]] || die "front topology already exists; use a slot preflight or finish the existing migration"
  legacy_status="$(gcloud_ssh "systemctl is-active --quiet subrouter.service; sudo curl -fsS --unix-socket /var/lib/subrouter/supervisor.sock http://localhost/_subrouter/supervisor-status")"
  jq -e '(.active.id|type)=="string" and (.backends|type)=="array" and ([.backends[].connections] | all(type=="number" and . >= 0))' \
    < <(stream_shell_value "${legacy_status}") >/dev/null || die "legacy supervisor status is invalid"
  legacy_generation="$(jq -r '.active.id' < <(stream_shell_value "${legacy_status}"))"
  legacy_active_connections="$(jq -r --arg id "${legacy_generation}" '[.backends[] | select(.id == $id) | .connections][0] // -1' < <(stream_shell_value "${legacy_status}"))"
  legacy_inactive_connections="$(jq -r --arg id "${legacy_generation}" '[.backends[] | select(.id != $id) | .connections] | add // 0' < <(stream_shell_value "${legacy_status}"))"
  (( legacy_inactive_connections == 0 )) || die "legacy inactive generation still owns connections"
  legacy_checksum="$(gcloud_ssh "sudo sha256sum /usr/local/bin/subrouter | awk '{print \$1}'" | tail -n 1)"
  [[ "${legacy_checksum}" =~ ^[0-9a-f]{64}$ && "${legacy_checksum}" != "${EXPECTED_SHA256}" ]] \
    || die "migration candidate must differ from the active legacy worker"
  topology="$(jq -nc --arg kind legacy --arg current legacy --arg generation "${legacy_generation}" \
    --arg checksum "${legacy_checksum}" --argjson active "${legacy_active_connections}" \
    --argjson inactive "${legacy_inactive_connections}" \
    '{kind:$kind,routing_current:$current,front_state:"absent",legacy:{service_active:true,
      generation:$generation,checksum:$checksum,active_connections:$active,
      inactive_connections:$inactive},candidate_differs_from_active:true}')"
else
  # After the listener handoff, the public URL map still names the legacy
  # backend. That backend's :31415 named port is now owned by the stable front,
  # while the front backend remains referenced by the protected canary host.
  # A routine slot deployment must accept that deliberate state and prove the
  # listener owner. Keep accepting the direct-front shape for fresh topology
  # and older installations.
  routing_current=""
  route_assignments_verified=false
  listener_takeover_verified=false
  listener_takeover_json='null'
  if [[ "${legacy_refs}" == 0 && "${front_refs}" == 1 ]]; then
    routing_current="front"
    active_backend_url="$(python3 "${SCRIPT_DIR}/url-map-routing.py" active-backend \
      "${URL_MAP_YAML}" "${ACTIVE_MATCHER}")" \
      || die "URL map active route could not be parsed"
    [[ "${active_backend_url}" == "${front_backend_url}" ]] \
      || die "URL map active route does not point to the front backend"
    route_assignments_verified=true
  elif [[ "${legacy_refs}" == 1 && "${front_refs}" == 1 ]]; then
    routing_current="front-listener"
    python3 "${SCRIPT_DIR}/url-map-routing.py" assert-state \
      "${URL_MAP_YAML}" "${ACTIVE_MATCHER}" "${legacy_backend_url}" \
      "${CANARY_MATCHER}" "${CANARY_HOST}" "${front_backend_url}" \
      || die "URL map active and canary assignments do not match the post-migration route"
    active_backend_url="${legacy_backend_url}"
    route_assignments_verified=true
    [[ -n "${CANARY_SECURITY_POLICY}" ]] || die "canary security policy is not configured for the listener-takeover route"
    verify_legacy_backend_port
    expected_policy_url="https://www.googleapis.com/compute/v1/projects/${PROJECT_ID}/global/securityPolicies/${CANARY_SECURITY_POLICY}"
    backend_policy_json="$("${GCLOUD_BINARY}" compute backend-services describe "${FRONT_BACKEND_SERVICE}" \
      --project "${PROJECT_ID}" --global --format=json)"
    [[ "$(jq -r '.securityPolicy // empty' < <(stream_shell_value "${backend_policy_json}"))" == "${expected_policy_url}" ]] \
      || die "front backend is not attached to the expected canary security policy"
    policy_json="$("${GCLOUD_BINARY}" compute security-policies describe "${CANARY_SECURITY_POLICY}" \
      --project "${PROJECT_ID}" --global --format=json)"
    canary_access_control_json="$(printf '%s\n' "${policy_json}" | python3 "${SCRIPT_DIR}/canary-security-policy.py" \
      assert-ready - "${CANARY_SECURITY_POLICY}" "${CANARY_HOST}")" \
      || die "canary security policy does not satisfy the access boundary"
    listener_takeover_output=""
    if ! listener_takeover_output="$(gcloud_ssh "set -eu; front_pid=\$(systemctl show subrouter-front.service -p MainPID --value); test \"\${front_pid}\" -gt 1; systemctl is-active --quiet subrouter-front.service; ! systemctl is-active --quiet subrouter.service; ! systemctl is-active --quiet subrouter.socket; ! systemctl is-enabled --quiet subrouter.service; ! systemctl is-enabled --quiet subrouter.socket; sudo ss -H -lntp 'sport = :31415' | grep -F \"pid=\${front_pid},\" >/dev/null; printf '%s\\n' \"SUBROUTER_LISTENER_TAKEOVER_PROOF={\\\"verified\\\":true,\\\"service\\\":\\\"subrouter-front.service\\\",\\\"port\\\":31415,\\\"pid\\\":\${front_pid}}\"")"; then
      die "listener takeover proof command failed"
    fi
    listener_takeover_json=""
    listener_takeover_records=0
    while IFS= read -r listener_takeover_line; do
      case "${listener_takeover_line}" in
        SUBROUTER_LISTENER_TAKEOVER_PROOF=*)
          listener_takeover_candidate="${listener_takeover_line#SUBROUTER_LISTENER_TAKEOVER_PROOF=}"
          if ! jq -e 'type == "object" and .verified == true and .service == "subrouter-front.service" and .port == 31415 and (.pid | type) == "number" and .pid > 1' \
            <<<"${listener_takeover_candidate}" >/dev/null; then
            die "listener takeover proof is invalid"
          fi
          listener_takeover_json="${listener_takeover_candidate}"
          listener_takeover_records=$((listener_takeover_records + 1))
          ;;
      esac
    done <<<"${listener_takeover_output}"
    [[ "${listener_takeover_records}" == 1 && -n "${listener_takeover_json}" ]] \
      || die "listener takeover proof is missing or duplicated"
    listener_takeover_verified=true
  else
    die "URL map does not describe a supported post-migration route"
  fi
  canary_backend_url="${front_backend_url}"
  canary_present=false
  [[ "${routing_current}" == front-listener ]] && canary_present=true
  front_status="$(gcloud_ssh "systemctl is-active --quiet subrouter-front.service; sudo curl -fsS --unix-socket /var/lib/subrouter/front.sock http://localhost/_subrouter/front-status")"
  active_slot="$(jq -r '.active.id // empty' < <(stream_shell_value "${front_status}"))"
  [[ "${active_slot}" == slot-a || "${active_slot}" == slot-b ]] || die "front active slot is invalid"
  slot_status="$(gcloud_ssh "systemctl is-active --quiet 'subrouter-slot@${active_slot}.service'; sudo curl -fsS --unix-socket '/var/lib/subrouter/${active_slot}.sock' http://localhost/_subrouter/supervisor-status")"
  jq -e '(.accepting == true) and (.retiring == false) and (.active.id|type)=="string" and
    (.backends|type)=="array" and ([.backends[].connections] | all(type=="number" and . >= 0))' \
    < <(stream_shell_value "${slot_status}") >/dev/null || die "active slot supervisor status is invalid"
  slot_generation="$(jq -r '.active.id' < <(stream_shell_value "${slot_status}"))"
  slot_inactive_connections="$(jq -r --arg id "${slot_generation}" '[.backends[] | select(.id != $id) | .connections] | add // 0' < <(stream_shell_value "${slot_status}"))"
  (( slot_inactive_connections == 0 )) || die "active slot has inactive-generation connections"
  worker_checksum="$(gcloud_ssh "sudo sha256sum '/opt/subrouter/slots/${active_slot}/worker' | awk '{print \$1}'" | tail -n 1)"
  control_checksum="$(gcloud_ssh "sudo sha256sum /opt/subrouter/control/subrouter | awk '{print \$1}'" | tail -n 1)"
  front_checksum="$(gcloud_ssh "sudo sha256sum /opt/subrouter/front/subrouter | awk '{print \$1}'" | tail -n 1)"
  slot_memory="$(gcloud_ssh "systemctl show 'subrouter-slot@${active_slot}.service' -p MemoryMax --value" | tail -n 1)"
  front_memory="$(gcloud_ssh "systemctl show subrouter-front.service -p MemoryMax --value" | tail -n 1)"
  [[ "${worker_checksum}" =~ ^[0-9a-f]{64}$ && "${control_checksum}" =~ ^[0-9a-f]{64}$ && "${front_checksum}" =~ ^[0-9a-f]{64}$ ]] \
    || die "front topology checksum is invalid"
  [[ "${worker_checksum}" != "${EXPECTED_SHA256}" ]] || die "slot candidate is already active"
  [[ "${slot_memory}" == 201326592 && "${front_memory}" == 134217728 ]] || die "front topology MemoryMax is incorrect"
  topology="$(jq -nc --arg kind front-slots --arg current "${routing_current}" --arg slot "${active_slot}" \
    --arg generation "${slot_generation}" --arg worker "${worker_checksum}" \
    --arg control "${control_checksum}" --arg front "${front_checksum}" \
    --argjson inactive "${slot_inactive_connections}" \
    --argjson route_assignments_verified "${route_assignments_verified}" \
    --argjson canary_present "${canary_present}" \
    --argjson listener_takeover_verified "${listener_takeover_verified}" \
    --argjson listener_takeover "${listener_takeover_json}" \
    '{kind:$kind,routing_current:$current,front:{service_active:true,active_slot:$slot,
      checksum:$front,memory_max_bytes:134217728},slot:{service_active:true,
      generation:$generation,worker_checksum:$worker,control_checksum:$control,
      inactive_connections:$inactive,memory_max_bytes:201326592},
      listener_takeover_verified:$listener_takeover_verified,
      listener_takeover:$listener_takeover,candidate_differs_from_active:true}')"
fi

emitted_at="$(utc_now)"
evidence_tmp="$(mktemp "${EVIDENCE_JSON}.tmp.XXXXXX")"
jq -n --arg schema 'subrouter.gcp.deploy-evidence/v1' --arg evidence_type deployment-preflight \
  --arg mode "${MODE}" --arg run_id "${RUN_LABEL}" --arg project "${PROJECT_ID}" \
  --arg zone "${ZONE}" --arg instance "${INSTANCE}" --arg tag "${RELEASE_TAG}" \
  --arg sha "${EXPECTED_SHA256}" --arg revision "${DEPLOY_REVISION}" --arg url_map "${URL_MAP}" \
  --arg active_matcher "${ACTIVE_MATCHER}" --arg active_backend_url "${active_backend_url}" \
  --arg legacy_backend_url "${legacy_backend_url}" --arg front_backend_url "${front_backend_url}" \
  --arg canary_matcher "${CANARY_MATCHER}" --arg canary_host "${CANARY_HOST}" \
  --arg canary_backend_url "${canary_backend_url}" \
  --arg legacy_backend_port_name "${legacy_backend_port_name}" \
  --argjson instance_group_http_port "${instance_group_http_port}" \
  --argjson canary_access_control "${canary_access_control_json}" \
  --argjson legacy_refs "${legacy_refs}" --argjson front_refs "${front_refs}" \
  --argjson backend_port_verified "${backend_port_verified}" \
  --argjson route_assignments_verified "${route_assignments_verified}" \
  --argjson canary_present "${canary_present}" \
  --argjson topology "${topology}" --arg emitted_at "${emitted_at}" \
  '{schema:$schema,evidence_type:$evidence_type,mode:$mode,success:true,
    mutation_performed:false,local_golden_required:true,
    run:{id:$run_id,project:$project,zone:$zone,instance:$instance},
    release:{tag:$tag,sha256:$sha,source_revision:$revision,tag_on_main:true,
      attestation_verified:true,immutable:true},public:{health:true,ready:true},
    routing:{url_map:$url_map,legacy_backend_references:$legacy_refs,
      front_backend_references:$front_refs,active_matcher:$active_matcher,
      active_backend_url:$active_backend_url,legacy_backend_url:$legacy_backend_url,
      front_backend_url:$front_backend_url,route_assignments_verified:$route_assignments_verified,
      backend_port_verified:$backend_port_verified,legacy_backend_port_name:$legacy_backend_port_name,
      instance_group_http_port:$instance_group_http_port,
      canary:{present:$canary_present,host:$canary_host,matcher:$canary_matcher,
        backend_url:$canary_backend_url,access_control:$canary_access_control}},
      topology:$topology,evidence_emitted_at:$emitted_at}' \
  >"${evidence_tmp}"
python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect deployment-preflight "${evidence_tmp}" >/dev/null
chmod 0600 "${evidence_tmp}"
mv -f -- "${evidence_tmp}" "${EVIDENCE_JSON}"
log "${MODE} topology is read-only verified; local Mac golden remains required"
jq -c . "${EVIDENCE_JSON}"
