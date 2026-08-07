#!/usr/bin/env bash
# Explicit staging-only normalization to the hard-pinned v0.1.60 worker before
# exercising the same legacy-to-front migration used by production.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: normalize-staging-predecessor.sh [--evidence-json PATH]

Downgrades only subrouter-staging's supervised legacy worker to the hard-pinned
v0.1.60 bytes, waits the prior generation to drain, and emits linked evidence.
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
PUBLIC_BASE_URL="${SUBROUTER_PUBLIC_BASE_URL:?set SUBROUTER_PUBLIC_BASE_URL}"
PREDECESSOR_TAG="${SUBROUTER_PREDECESSOR_TAG:?set SUBROUTER_PREDECESSOR_TAG}"
PREDECESSOR_BINARY="${SUBROUTER_PREDECESSOR_BINARY:?set SUBROUTER_PREDECESSOR_BINARY}"
PREDECESSOR_SHA256_FILE="${SUBROUTER_PREDECESSOR_SHA256_FILE:-${PREDECESSOR_BINARY}.sha256}"
PREDECESSOR_SHA256SUMS_FILE="${SUBROUTER_PREDECESSOR_SHA256SUMS_FILE:?set SUBROUTER_PREDECESSOR_SHA256SUMS_FILE}"
PREDECESSOR_REVISION="${SUBROUTER_PREDECESSOR_REVISION:?set SUBROUTER_PREDECESSOR_REVISION}"
PREDECESSOR_TAG_ON_MAIN="${SUBROUTER_PREDECESSOR_TAG_ON_MAIN:?set SUBROUTER_PREDECESSOR_TAG_ON_MAIN}"
GCLOUD_BINARY="${GCLOUD_BIN:-gcloud}"
DEPLOY_LOCK_FILE="${SUBROUTER_DEPLOY_LOCK_FILE:-/run/lock/subrouter-deploy.lock}"
DRAIN_TIMEOUT_SECONDS="${SUBROUTER_NORMALIZATION_DRAIN_TIMEOUT_SECONDS:-3600}"
ARTIFACT_DIR="${SUBROUTER_DEPLOY_ARTIFACT_DIR:-${PWD}/artifacts/gcp-staging-normalization}"
RUN_LABEL="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-normalize-$$"
RUN_LABEL="${RUN_LABEL//[^a-zA-Z0-9._-]/-}"
REMOTE_CANDIDATE="/tmp/subrouter-v0.1.60-${RUN_LABEL}"
REMOTE_BACKUP="/tmp/subrouter-pre-normalization-${RUN_LABEL}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/gcp/stream-shell-value.sh
source "${SCRIPT_DIR}/stream-shell-value.sh"
# shellcheck source=deploy/gcp/deploy-lock.sh
source "${SCRIPT_DIR}/deploy-lock.sh"

log() { printf 'gcp-staging-normalization: %s\n' "$*"; }
die() { log "$*" >&2; exit 1; }
for command in "${GCLOUD_BINARY}" curl go jq mktemp python3 sha256sum; do
  command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done
[[ "${INSTANCE}" == subrouter-staging ]] || die "normalization is restricted to subrouter-staging"
[[ "${PUBLIC_BASE_URL%/}" == https://staging.sr.cmux.com ]] || die "normalization requires the staging public origin"
[[ "${PREDECESSOR_TAG}" == v0.1.60 ]] || die "normalization predecessor must be v0.1.60"
[[ "${PREDECESSOR_REVISION}" == e169e94f2bea9a0455a5831631fcbac220bd65f2 ]] || die "v0.1.60 revision hard pin mismatch"
[[ "${PREDECESSOR_TAG_ON_MAIN}" == true ]] || die "v0.1.60 tag commit was not proven on main"
[[ -x "${PREDECESSOR_BINARY}" && -f "${PREDECESSOR_SHA256_FILE}" && -f "${PREDECESSOR_SHA256SUMS_FILE}" ]] \
  || die "verified v0.1.60 inputs are incomplete"
PREDECESSOR_SHA256="$(tr -d '[:space:]' <"${PREDECESSOR_SHA256_FILE}")"
[[ "${PREDECESSOR_SHA256}" == 6a8daa1361030311bdbe25a06cd4940e4dd07a45758c13c2dc8d687e70d87303 ]] \
  || die "v0.1.60 Linux asset hard pin mismatch"
[[ "$(sha256sum "${PREDECESSOR_BINARY}" | awk '{print $1}')" == "${PREDECESSOR_SHA256}" ]] \
  || die "v0.1.60 worker bytes changed"
[[ "$(awk '$2 == "subrouter_0.1.60_linux_amd64" {print $1}' "${PREDECESSOR_SHA256SUMS_FILE}")" == "${PREDECESSOR_SHA256}" ]] \
  || die "v0.1.60 SHA256SUMS does not match the hard pin"
bash "${SCRIPT_DIR}/verify-go-release-binary.sh" \
  "${PREDECESSOR_BINARY}" e169e94f2bea9a0455a5831631fcbac220bd65f2 \
  || die "v0.1.60 embedded metadata is invalid"
[[ "${DRAIN_TIMEOUT_SECONDS}" =~ ^[0-9]+$ ]] || die "normalization drain timeout must be an integer"
(( DRAIN_TIMEOUT_SECONDS > 0 )) || die "normalization drain timeout must be positive"

mkdir -p "${ARTIFACT_DIR}"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd)"
EVIDENCE_JSON="${EVIDENCE_JSON:-${ARTIFACT_DIR}/result.json}"
mkdir -p "$(dirname "${EVIDENCE_JSON}")"
EVIDENCE_JSON="$(cd "$(dirname "${EVIDENCE_JSON}")" && pwd)/$(basename "${EVIDENCE_JSON}")"

gcloud_ssh() {
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" --project "${PROJECT_ID}" --zone "${ZONE}" \
    --tunnel-through-iap --quiet --command "$1"
}
gcloud_scp() {
  "${SCRIPT_DIR}/gcloud-scp.sh" "${GCLOUD_BINARY}" "$1" "${INSTANCE}:$2" --project "${PROJECT_ID}" --zone "${ZONE}" \
    --tunnel-through-iap --quiet
}
utc_now() { python3 -c 'from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))'; }
supervisor_status() { gcloud_ssh "sudo curl -fsS --unix-socket /var/lib/subrouter/supervisor.sock http://localhost/_subrouter/supervisor-status"; }
service_restarts() { gcloud_ssh "systemctl show subrouter.service -p NRestarts --value" | tail -n 1; }
service_oom() { gcloud_ssh "set -eu; cg=\$(systemctl show subrouter.service -p ControlGroup --value); awk '\$1 == \"oom_kill\" {print \$2}' /sys/fs/cgroup\${cg}/memory.events" | tail -n 1; }
status_validation_file="$(mktemp "${TMPDIR:-/tmp}/subrouter-legacy-supervisor-status.XXXXXX")"
validate_clean_legacy_status() {
  printf '%s\n' "$1" >"${status_validation_file}"
  python3 "${SCRIPT_DIR}/deployment-contract.py" \
    validate-legacy-supervisor-status "${status_validation_file}"
}

lock_holder_pid=""
normalization_started=0
normalization_committed=0
acquire_lock() {
  subrouter_acquire_deploy_lock "${ARTIFACT_DIR}/deploy-lock.log" \
    "${GCLOUD_BINARY}" "${INSTANCE}" "${PROJECT_ID}" "${ZONE}" "${DEPLOY_LOCK_FILE}" "${RUN_LABEL}" \
    || die "could not acquire ${DEPLOY_LOCK_FILE}"
}
release_lock() {
  subrouter_release_deploy_lock
}
rollback_normalization() {
  local pre_rollback_status pre_rollback_generation restored_status restored_generation restored_checksum
  log "normalization failed; restoring the exact pre-normalization worker"
  if ! pre_rollback_status="$(supervisor_status)"; then
    log "could not read the generation before rollback" >&2
    return 1
  fi
  pre_rollback_generation="$(jq -r '.active.id // empty' < <(stream_shell_value "${pre_rollback_status}"))"
  [[ -n "${pre_rollback_generation}" ]] || {
    log "supervisor had no active generation before rollback" >&2
    return 1
  }
  if ! gcloud_ssh "set -e; sudo test -f '${REMOTE_BACKUP}'; printf '%s  %s\n' '${before_checksum}' '${REMOTE_BACKUP}' | sudo sha256sum -c - >/dev/null; sudo install -m 0755 -o root -g root '${REMOTE_BACKUP}' /usr/local/bin/subrouter.rollback; printf '%s  %s\n' '${before_checksum}' /usr/local/bin/subrouter.rollback | sudo sha256sum -c - >/dev/null; sudo mv -f /usr/local/bin/subrouter.rollback /usr/local/bin/subrouter; sudo curl -fsS --unix-socket /var/lib/subrouter/supervisor.sock -X POST http://localhost/_subrouter/upgrade >/dev/null"; then
    log "could not reinstall and request the exact prior worker" >&2
    return 1
  fi
  for _ in $(seq 1 120); do
    if restored_status="$(supervisor_status 2>/dev/null)"; then
      restored_generation="$(jq -r '.active.id // empty' < <(stream_shell_value "${restored_status}"))"
      restored_checksum="$(gcloud_ssh "sudo sha256sum /usr/local/bin/subrouter | awk '{print \$1}'" 2>/dev/null | tail -n 1)"
      if [[ -n "${restored_generation}" &&
            "${restored_generation}" != "${pre_rollback_generation}" &&
            "${restored_generation}" != "${before_generation}" &&
            "${restored_checksum}" == "${before_checksum}" ]] &&
          validate_clean_legacy_status "${restored_status}" >/dev/null 2>&1 &&
          curl -fsS --max-time 2 "${PUBLIC_BASE_URL%/}/_subrouter/health" >/dev/null 2>&1 &&
          curl -fsS --max-time 2 "${PUBLIC_BASE_URL%/}/_subrouter/ready" >/dev/null 2>&1; then
        log "rollback restored checksum ${before_checksum} as healthy generation ${restored_generation}"
        return 0
      fi
    fi
    sleep 0.25
  done
  log "rollback did not restore the prior checksum as a new healthy generation" >&2
  return 1
}
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  if [[ "${normalization_started}" == 1 && "${normalization_committed}" == 0 ]]; then
    rollback_normalization || status=1
  fi
  unlink "${status_validation_file}" 2>/dev/null || true
  gcloud_ssh "rm -f '${REMOTE_CANDIDATE}' '${REMOTE_BACKUP}'" >/dev/null 2>&1 || true
  release_lock
  exit "${status}"
}
trap cleanup EXIT INT TERM

curl -fsS --max-time 10 "${PUBLIC_BASE_URL%/}/_subrouter/health" >/dev/null || die "staging public health failed"
curl -fsS --max-time 10 "${PUBLIC_BASE_URL%/}/_subrouter/ready" >/dev/null || die "staging public readiness failed"
acquire_lock
gcloud_ssh "systemctl is-active --quiet subrouter.service; sudo test -S /var/lib/subrouter/supervisor.sock"
before_status="$(supervisor_status)"
validate_clean_legacy_status "${before_status}" \
  || die "staging legacy supervisor is not a clean active generation"
before_generation="$(jq -r '.active.id' < <(stream_shell_value "${before_status}"))"
before_connections="$(jq -r --arg id "${before_generation}" '[.backends[] | select(.id == $id) | .connections][0] // -1' < <(stream_shell_value "${before_status}"))"
inactive_connections_before="$(jq -r --arg id "${before_generation}" '[.backends[] | select(.id != $id) | .connections] | add // 0' < <(stream_shell_value "${before_status}"))"
before_checksum="$(gcloud_ssh "sudo sha256sum /usr/local/bin/subrouter | awk '{print \$1}'" | tail -n 1)"
[[ "${before_checksum}" =~ ^[0-9a-f]{64}$ ]] || die "staging worker checksum is invalid"
restarts_before="$(service_restarts)"
oom_before="$(service_oom)"
if [[ "${before_checksum}" == "${PREDECESSOR_SHA256}" ]]; then
  (( inactive_connections_before == 0 )) \
    || die "already-normalized staging has connections on an inactive generation"
  gcloud_ssh "printf '%s\n' v0.1.60 | sudo tee /etc/subrouter-version >/dev/null"
  after_status="$(supervisor_status)"
  validate_clean_legacy_status "${after_status}" \
    || die "already-normalized staging changed supervisor state during verification"
  after_generation="$(jq -r '.active.id // empty' < <(stream_shell_value "${after_status}"))"
  after_connections="$(jq -r --arg id "${after_generation}" '[.backends[] | select(.id == $id) | .connections][0] // -1' < <(stream_shell_value "${after_status}"))"
  inactive_connections_after="$(jq -r --arg id "${after_generation}" '[.backends[] | select(.id != $id) | .connections] | add // 0' < <(stream_shell_value "${after_status}"))"
  [[ "${after_generation}" == "${before_generation}" ]] \
    || die "already-normalized staging changed generation during verification"
  (( inactive_connections_after == 0 )) \
    || die "already-normalized staging gained an inactive generation"
  curl -fsS --max-time 10 "${PUBLIC_BASE_URL%/}/_subrouter/health" >/dev/null \
    || die "already-normalized staging public health failed"
  curl -fsS --max-time 10 "${PUBLIC_BASE_URL%/}/_subrouter/ready" >/dev/null \
    || die "already-normalized staging public readiness failed"
  after_checksum="$(gcloud_ssh "sudo sha256sum /usr/local/bin/subrouter | awk '{print \$1}'" | tail -n 1)"
  [[ "${after_checksum}" == "${PREDECESSOR_SHA256}" ]] \
    || die "already-normalized staging worker checksum changed during verification"
  restarts_after="$(service_restarts)"
  oom_after="$(service_oom)"
  [[ "${restarts_after}" == "${restarts_before}" && "${oom_after}" == "${oom_before}" ]] \
    || die "already-normalized supervisor restarted or OOM-killed during verification"
  verified_at="$(utc_now)"
  emitted_at="$(utc_now)"
  evidence_tmp="$(mktemp "${EVIDENCE_JSON}.tmp.XXXXXX")"
  jq -n --arg schema 'subrouter.gcp.deploy-evidence/v1' --arg evidence_type staging-predecessor-normalization \
    --arg run_id "${RUN_LABEL}" --arg project "${PROJECT_ID}" --arg zone "${ZONE}" --arg instance "${INSTANCE}" \
    --arg checksum "${after_checksum}" --arg generation "${after_generation}" \
    --arg verified_at "${verified_at}" --arg emitted_at "${emitted_at}" \
    --argjson before_connections "${before_connections}" --argjson after_connections "${after_connections}" \
    --argjson rb "${restarts_before}" --argjson ra "${restarts_after}" \
    --argjson ob "${oom_before}" --argjson oa "${oom_after}" \
    '{schema:$schema,evidence_type:$evidence_type,mode:"staging-only",success:true,
      normalization_performed:false,normalization_result:"already-normalized",
      run:{id:$run_id,project:$project,zone:$zone,instance:$instance},
      predecessor:{tag:"v0.1.60",sha256:$checksum,
        source_revision:"e169e94f2bea9a0455a5831631fcbac220bd65f2",tag_on_main:true,
        hard_pin_verified:true,sha256sums_match:true,embedded_revision_verified:true,
        live_worker_checksum_match:true},
      checksums:{before:$checksum,after:$checksum},
      generations:{before:$generation,after:$generation},
      connections:{active_generation_before:$before_connections,
        active_generation_after:$after_connections,inactive_after:0},
      public:{health:true,ready:true},
      timestamps:{verified_at:$verified_at,evidence_emitted_at:$emitted_at},
      metrics:{nrestarts:{before:$rb,after:$ra},oom_kill:{before:$ob,after:$oa}}}' \
    >"${evidence_tmp}"
  python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect staging-predecessor-normalization "${evidence_tmp}" >/dev/null
  chmod 0600 "${evidence_tmp}"
  mv -f -- "${evidence_tmp}" "${EVIDENCE_JSON}"
  normalization_committed=1
  log "staging already uses the exact hard-pinned v0.1.60 worker"
  jq -c . "${EVIDENCE_JSON}"
  exit 0
fi
gcloud_scp "${PREDECESSOR_BINARY}" "${REMOTE_CANDIDATE}"
gcloud_ssh "set -e; printf '%s  %s\n' '${PREDECESSOR_SHA256}' '${REMOTE_CANDIDATE}' | sha256sum -c - >/dev/null; sudo cp -p /usr/local/bin/subrouter '${REMOTE_BACKUP}'; sudo install -m 0755 -o root -g root '${REMOTE_CANDIDATE}' /usr/local/bin/subrouter.incoming; sudo mv -f /usr/local/bin/subrouter.incoming /usr/local/bin/subrouter"
normalization_started=1
upgrade_requested_at="$(utc_now)"
gcloud_ssh "sudo curl -fsS --unix-socket /var/lib/subrouter/supervisor.sock -X POST http://localhost/_subrouter/upgrade >/dev/null"

for _ in $(seq 1 120); do
  after_status="$(supervisor_status)"
  after_generation="$(jq -r '.active.id // empty' < <(stream_shell_value "${after_status}"))"
  if [[ -n "${after_generation}" && "${after_generation}" != "${before_generation}" ]] && \
      curl -fsS --max-time 2 "${PUBLIC_BASE_URL%/}/_subrouter/health" >/dev/null 2>&1 && \
      curl -fsS --max-time 2 "${PUBLIC_BASE_URL%/}/_subrouter/ready" >/dev/null 2>&1; then
    activated_at="$(utc_now)"
    break
  fi
  sleep 0.25
done
[[ -n "${activated_at:-}" ]] || die "v0.1.60 worker generation did not become healthy"

deadline=$(( $(date +%s) + DRAIN_TIMEOUT_SECONDS ))
while true; do
  after_status="$(supervisor_status)"
  old_connections="$(jq -r --arg id "${before_generation}" '[.backends[] | select(.id == $id) | .connections][0] // 0' < <(stream_shell_value "${after_status}"))"
  inactive_connections="$(jq -r --arg id "$(jq -r '.active.id' < <(stream_shell_value "${after_status}"))" '[.backends[] | select(.id != $id) | .connections] | add // 0' < <(stream_shell_value "${after_status}"))"
  if (( old_connections == 0 && inactive_connections == 0 )); then
    drained_at="$(utc_now)"
    break
  fi
  (( $(date +%s) < deadline )) || die "pre-normalization generation did not drain"
  sleep 0.1
done
after_generation="$(jq -r '.active.id' < <(stream_shell_value "${after_status}"))"
after_checksum="$(gcloud_ssh "sudo sha256sum /usr/local/bin/subrouter | awk '{print \$1}'" | tail -n 1)"
[[ "${after_checksum}" == "${PREDECESSOR_SHA256}" ]] || die "staging normalization installed unexpected bytes"
restarts_after="$(service_restarts)"
oom_after="$(service_oom)"
[[ "${restarts_after}" == "${restarts_before}" && "${oom_after}" == "${oom_before}" ]] \
  || die "legacy supervisor restarted or OOM-killed during staging normalization"
gcloud_ssh "printf '%s\n' v0.1.60 | sudo tee /etc/subrouter-version >/dev/null"
emitted_at="$(utc_now)"

evidence_tmp="$(mktemp "${EVIDENCE_JSON}.tmp.XXXXXX")"
jq -n --arg schema 'subrouter.gcp.deploy-evidence/v1' --arg evidence_type staging-predecessor-normalization \
  --arg run_id "${RUN_LABEL}" --arg project "${PROJECT_ID}" --arg zone "${ZONE}" --arg instance "${INSTANCE}" \
  --arg before_checksum "${before_checksum}" --arg after_checksum "${after_checksum}" \
  --arg before_generation "${before_generation}" --arg after_generation "${after_generation}" \
  --arg requested_at "${upgrade_requested_at}" --arg activated_at "${activated_at}" \
  --arg drained_at "${drained_at}" --arg emitted_at "${emitted_at}" \
  --argjson before_connections "${before_connections}" --argjson rb "${restarts_before}" \
  --argjson ra "${restarts_after}" --argjson ob "${oom_before}" --argjson oa "${oom_after}" \
  '{schema:$schema,evidence_type:$evidence_type,mode:"staging-only",success:true,
    normalization_performed:true,normalization_result:"replaced-worker",
    run:{id:$run_id,project:$project,zone:$zone,instance:$instance},
    predecessor:{tag:"v0.1.60",sha256:$after_checksum,
      source_revision:"e169e94f2bea9a0455a5831631fcbac220bd65f2",tag_on_main:true,
      hard_pin_verified:true,sha256sums_match:true,embedded_revision_verified:true,
      live_worker_checksum_match:true},
    checksums:{before:$before_checksum,after:$after_checksum},
    generations:{before:$before_generation,after:$after_generation},
    connections:{old_generation_before:$before_connections,old_generation_after:0,
      inactive_after:0},public:{health:true,ready:true},
    timestamps:{upgrade_requested_at:$requested_at,activated_at:$activated_at,
      drained_at:$drained_at,evidence_emitted_at:$emitted_at},
    metrics:{nrestarts:{before:$rb,after:$ra},oom_kill:{before:$ob,after:$oa}}}' \
  >"${evidence_tmp}"
python3 "${SCRIPT_DIR}/validate-deploy-evidence.py" --expect staging-predecessor-normalization "${evidence_tmp}" >/dev/null
chmod 0600 "${evidence_tmp}"
mv -f -- "${evidence_tmp}" "${EVIDENCE_JSON}"
normalization_committed=1
log "staging normalized to hard-pinned v0.1.60 and prior generation drained"
jq -c . "${EVIDENCE_JSON}"
