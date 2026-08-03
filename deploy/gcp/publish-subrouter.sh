#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
instance_name="${INSTANCE_NAME:-subrouter-team}"
server_name="${SERVER_NAME:-team}"
zone="${ZONE:-us-south1-a}"
server_url="${SERVER_URL:-}"
subrouter_version="${1:-${SUBROUTER_RELEASE_TAG:-${SUBROUTER_VERSION:-}}}"
sr_bin="${SR_BIN:-sr}"
bootstrap_evidence="${SUBROUTER_FRESH_VM_BOOTSTRAP_EVIDENCE:-${PWD}/artifacts/gcp-vm-provision/result.json}"
acceptance_evidence="${SUBROUTER_FRESH_VM_ACCEPTANCE_EVIDENCE:-${PWD}/artifacts/gcp-vm-acceptance/result.json}"

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}
utc_now() { python3 -c 'from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))'; }
die() { printf 'publish-subrouter: %s\n' "$*" >&2; exit 1; }

if [[ ! "${subrouter_version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  echo "Pass an explicit release tag, for example: deploy/gcp/publish-subrouter.sh v0.1.52" >&2
  exit 1
fi

if [[ -z "${server_url}" ]]; then
  case "${instance_name}" in
    subrouter-team) server_url="https://sr.cmux.com" ;;
    subrouter-staging) server_url="https://staging.sr.cmux.com" ;;
    *)
      echo "SERVER_URL is required for an instance without a registered public hostname." >&2
      exit 1
      ;;
  esac
fi

if [[ ! "${server_url}" =~ ^https://[^/?#]+/?$ ]]; then
  echo "SERVER_URL must be an HTTPS origin without a path, query, or fragment." >&2
  exit 1
fi

if ! command -v "${sr_bin}" >/dev/null 2>&1; then
  echo "sr is required. Install it with:" >&2
  echo "  curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sh" >&2
  exit 1
fi

if ! command -v gcloud >/dev/null 2>&1; then
  echo "gcloud is required. Install Google Cloud CLI first." >&2
  exit 1
fi
for required_command in curl install jq python3; do
  command -v "${required_command}" >/dev/null 2>&1 || {
    echo "${required_command} is required." >&2
    exit 1
  }
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
  echo "sha256sum or shasum is required." >&2
  exit 1
fi

active_account="$(gcloud config get-value account 2>/dev/null || true)"
if [[ -z "${active_account}" || "${active_account}" == "(unset)" ]]; then
  echo "No active gcloud account. Run: gcloud auth login" >&2
  exit 1
fi

project_id="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}"
if [[ -z "${project_id}" || "${project_id}" == "(unset)" ]]; then
  echo "No GCP project configured. Run: gcloud config set project <project-id>" >&2
  exit 1
fi

live_instance_identity_json=""
query_live_instance_identity() {
  local instance_json_file
  local parsed_identity
  instance_json_file="$(mktemp "${TMPDIR:-/tmp}/subrouter-gce-instance.json.XXXXXX")"
  if ! gcloud compute instances describe "${instance_name}" \
      --project "${project_id}" --zone "${zone}" --format=json >"${instance_json_file}"; then
    rm -f -- "${instance_json_file}"
    die "could not query GCE instance identity"
  fi
  if ! parsed_identity="$(python3 - "${instance_json_file}" "${instance_name}" "${zone}" <<'PY'
from datetime import datetime, timezone
import json
from pathlib import Path
import re
import sys

path, expected_name, expected_zone = sys.argv[1:]
document = json.loads(Path(path).read_text())
if document.get("name") != expected_name:
    raise SystemExit("GCE instance response name does not match the deployment target")
returned_zone = document.get("zone")
if not isinstance(returned_zone, str) or returned_zone.rsplit("/", 1)[-1] != expected_zone:
    raise SystemExit("GCE instance response zone does not match the deployment target")
raw_id = document.get("id")
if isinstance(raw_id, bool) or not isinstance(raw_id, (str, int)):
    raise SystemExit("GCE instance ID is missing or invalid")
instance_id = str(raw_id)
if re.fullmatch(r"[1-9][0-9]{0,19}", instance_id) is None or int(instance_id) > 2**64 - 1:
    raise SystemExit("GCE instance ID is missing or invalid")
raw_created = document.get("creationTimestamp")
if not isinstance(raw_created, str) or not raw_created:
    raise SystemExit("GCE instance creationTimestamp is missing or invalid")
try:
    created = datetime.fromisoformat(raw_created.replace("Z", "+00:00"))
except ValueError as error:
    raise SystemExit(f"GCE instance creationTimestamp is invalid: {error}") from error
if created.tzinfo is None:
    raise SystemExit("GCE instance creationTimestamp must include a timezone")
created_utc = created.astimezone(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")
print(json.dumps({"creation_timestamp": created_utc, "id": instance_id}, separators=(",", ":"), sort_keys=True))
PY
)"; then
    rm -f -- "${instance_json_file}"
    die "could not validate GCE instance identity"
  fi
  rm -f -- "${instance_json_file}"
  live_instance_identity_json="${parsed_identity}"
}

deploy_lock_file="${SUBROUTER_DEPLOY_LOCK_FILE:-/run/lock/subrouter-deploy.lock}"
run_label="publish-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$"
run_label="${run_label//[^a-zA-Z0-9._-]/-}"
remote_lock_sentinel="/tmp/subrouter-deploy-lock-${run_label}"
lock_log="$(mktemp "${TMPDIR:-/tmp}/subrouter-publish-lock.XXXXXX")"
lock_holder_pid=""
remote_probe=""
topology_tmp=""
acceptance_tmp=""
bootstrap_snapshot=""

gcloud_ssh() {
  gcloud compute ssh "${instance_name}" \
    --project "${project_id}" --zone "${zone}" --tunnel-through-iap --quiet \
    --command "$1"
}

# shellcheck disable=SC2329 # Called by the EXIT/signal cleanup trap.
release_deploy_lock() {
  if [[ -n "${lock_holder_pid}" ]]; then
    gcloud_ssh "rm -f '${remote_lock_sentinel}'" >/dev/null 2>&1 || true
    wait "${lock_holder_pid}" || true
    lock_holder_pid=""
  fi
}

# shellcheck disable=SC2329 # Called by the EXIT/signal cleanup trap.
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  if [[ -n "${remote_probe}" ]]; then
    gcloud_ssh "rm -f '${remote_probe}'" >/dev/null 2>&1 || true
  fi
  release_deploy_lock
  rm -f -- "${lock_log}"
  [[ -z "${topology_tmp}" ]] || rm -f -- "${topology_tmp}"
  [[ -z "${acceptance_tmp}" ]] || rm -f -- "${acceptance_tmp}"
  [[ -z "${bootstrap_snapshot}" ]] || rm -f -- "${bootstrap_snapshot}"
  exit "${status}"
}
trap cleanup EXIT INT TERM

gcloud_ssh "umask 077; : > '${remote_lock_sentinel}'; command -v flock >/dev/null"
gcloud compute ssh "${instance_name}" \
  --project "${project_id}" --zone "${zone}" --tunnel-through-iap --quiet \
  --command "sudo flock -x -w 300 '${deploy_lock_file}' sh -c 'echo LOCKED; while test -e \"${remote_lock_sentinel}\"; do sleep 1; done'" \
  >"${lock_log}" 2>&1 &
lock_holder_pid=$!
for _ in $(seq 1 3100); do
  grep -qx LOCKED "${lock_log}" 2>/dev/null && break
  kill -0 "${lock_holder_pid}" 2>/dev/null || {
    echo "Remote deployment lock holder exited." >&2
    exit 1
  }
  sleep 0.1
done
grep -qx LOCKED "${lock_log}" 2>/dev/null || {
  echo "Timed out acquiring ${deploy_lock_file}." >&2
  exit 1
}

topology="$(gcloud_ssh 'if sudo test -f /var/lib/subrouter/front-topology-prepared; then echo fresh-prepared; elif sudo test -S /var/lib/subrouter/front.sock || systemctl is-active --quiet subrouter-front.service; then echo front; else echo legacy; fi' | tail -n 1)"
if [[ "${topology}" != "legacy" && "${topology}" != "fresh-prepared" ]]; then
  if [[ "${topology}" == "front" ]]; then
    echo "Front/slot topology is active. Use the protected GCP Deploy workflow for release changes." >&2
  else
    echo "Could not determine the remote deployment topology." >&2
  fi
  exit 1
fi

expected_sha256=""
bootstrap_sha256=""
bootstrap_run_id=""
bootstrap_emitted_at=""
fresh_instance_identity_json=""
fresh_instance_id=""
fresh_instance_creation_timestamp=""
if [[ "${topology}" == "fresh-prepared" ]]; then
  [[ -f "${bootstrap_evidence}" && ! -L "${bootstrap_evidence}" ]] || {
    echo "Fresh VM bootstrap evidence is missing or unsafe: ${bootstrap_evidence}" >&2
    exit 1
  }
  bootstrap_snapshot="$(mktemp "${TMPDIR:-/tmp}/subrouter-vm-bootstrap.json.XXXXXX")"
  install -m 0600 "${bootstrap_evidence}" "${bootstrap_snapshot}"
  python3 "${script_dir}/validate-deploy-evidence.py" --expect vm-provision "${bootstrap_snapshot}" >/dev/null
  jq -e --arg project "${project_id}" --arg zone "${zone}" --arg instance "${instance_name}" \
    --arg tag "${subrouter_version}" '
      .mutation_performed == true and .instance.created == true and
      .topology.state == "prepared" and .topology.authenticated == false and
      .run.project == $project and .run.zone == $zone and .run.instance == $instance and
      .release.tag == $tag
    ' "${bootstrap_snapshot}" >/dev/null || {
      echo "Fresh VM bootstrap evidence does not prove a newly created prepared target." >&2
      exit 1
    }
  expected_sha256="$(jq -r '.release.sha256' "${bootstrap_snapshot}")"
  bootstrap_run_id="$(jq -r '.run.id' "${bootstrap_snapshot}")"
  bootstrap_emitted_at="$(jq -r '.evidence_emitted_at' "${bootstrap_snapshot}")"
  bootstrap_sha256="$(sha256_file "${bootstrap_snapshot}")"
  query_live_instance_identity
  fresh_instance_identity_json="${live_instance_identity_json}"
  python3 - "${bootstrap_snapshot}" "${fresh_instance_identity_json}" <<'PY'
from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
import sys

bootstrap = json.loads(Path(sys.argv[1]).read_text())
live = json.loads(sys.argv[2])
instance = bootstrap["instance"]
if instance["id"] != live["id"]:
    raise SystemExit("fresh VM bootstrap GCE instance ID does not match the live target")
bootstrap_created = datetime.fromisoformat(instance["creation_timestamp"].replace("Z", "+00:00"))
live_created = datetime.fromisoformat(live["creation_timestamp"].replace("Z", "+00:00"))
if bootstrap_created != live_created:
    raise SystemExit("fresh VM bootstrap creation timestamp does not match the live target")
age = datetime.now(timezone.utc) - live_created
if age < -timedelta(minutes=5):
    raise SystemExit("live GCE instance creation timestamp is too far in the future")
if age >= timedelta(hours=2):
    raise SystemExit("live GCE instance is too old for fresh VM acceptance")
PY
  fresh_instance_id="$(jq -r '.id' <<<"${fresh_instance_identity_json}")"
  fresh_instance_creation_timestamp="$(jq -r '.creation_timestamp' <<<"${fresh_instance_identity_json}")"
fi

"${sr_bin}" server add "${server_name}" \
  --url "${server_url}" \
  --gcp-instance "${instance_name}" \
  --gcp-zone "${zone}" \
  --gcp-project "${project_id}" \
  --default

"${sr_bin}" server install "${server_name}" \
  --version "${subrouter_version}"

if [[ "${topology}" == "fresh-prepared" ]]; then
  version="${subrouter_version#v}"
  binary_asset="subrouter_${version}_linux_amd64"
  gcloud_ssh "set -eu; metadata='/opt/subrouter/releases/${subrouter_version}/VM_RELEASE_METADATA.json'; sudo test -f \"\${metadata}\"; expected=\$(sudo jq -r --arg tag '${subrouter_version}' --arg asset '${binary_asset}' 'if .release_tag == \$tag then .assets[\$asset] // empty else empty end' \"\${metadata}\"); test \"\${#expected}\" -eq 64; case \"\${expected}\" in *[!0-9a-f]*) echo 'invalid fresh topology release metadata' >&2; exit 1;; esac; installed=\$(sudo sha256sum /usr/local/bin/subrouter | awk '{print \\$1}'); test \"\${installed}\" = \"\${expected}\"; sudo /usr/local/libexec/subrouter-install-front-slots activate-fresh-topology slot-a; systemctl is-active --quiet subrouter-slot@slot-a.service; systemctl is-active --quiet subrouter-front.service; ! systemctl is-active --quiet subrouter.service; ! systemctl is-active --quiet subrouter.socket; sudo test -f /var/lib/subrouter/front-topology-prepared.active; for path in /opt/subrouter/control/subrouter /opt/subrouter/front/subrouter /opt/subrouter/slots/slot-a/worker; do actual=\$(sudo sha256sum \"\${path}\" | awk '{print \\$1}'); test \"\${actual}\" = \"\${expected}\"; done"
fi

public_ready=false
for _ in $(seq 1 30); do
  if curl -fsS --max-time 5 "${server_url%/}/_subrouter/health" >/dev/null 2>&1 &&
      curl -fsS --max-time 5 "${server_url%/}/_subrouter/ready" >/dev/null 2>&1; then
    public_ready=true
    break
  fi
  sleep 1
done
[[ "${public_ready}" == true ]] || {
  echo "Public health or readiness did not pass for ${server_url%/}." >&2
  exit 1
}

if [[ "${topology}" == "fresh-prepared" ]]; then
  remote_probe="/tmp/subrouter-verify-fresh-vm-${run_label}.sh"
  topology_tmp="$(mktemp "${TMPDIR:-/tmp}/subrouter-fresh-topology.json.XXXXXX")"
  gcloud compute scp "${script_dir}/verify-fresh-vm.sh" "${instance_name}:${remote_probe}" \
    --project "${project_id}" --zone "${zone}" --tunnel-through-iap --quiet >/dev/null
  gcloud_ssh "sudo env SUBROUTER_EXPECTED_SHA256='${expected_sha256}' SUBROUTER_RELEASE_TAG='${subrouter_version}' bash '${remote_probe}'" \
    >"${topology_tmp}"
  gcloud_ssh "rm -f '${remote_probe}'" >/dev/null
  remote_probe=""
  jq -e '
    .state == "active" and .authenticated == true and
    .slot.service_active == true and .slot.service_enabled == true and
    .front.service_active == true and .front.service_enabled == true
  ' "${topology_tmp}" >/dev/null || {
    echo "Fresh VM did not prove authenticated active front/slot topology." >&2
    exit 1
  }

  query_live_instance_identity
  [[ "${live_instance_identity_json}" == "${fresh_instance_identity_json}" ]] || {
    echo "GCE instance identity changed during fresh VM publication." >&2
    exit 1
  }

  mkdir -p "$(dirname "${acceptance_evidence}")"
  acceptance_evidence="$(cd "$(dirname "${acceptance_evidence}")" && pwd)/$(basename "${acceptance_evidence}")"
  if [[ -e "${acceptance_evidence}" || -L "${acceptance_evidence}" ]]; then
    [[ -f "${acceptance_evidence}" && ! -L "${acceptance_evidence}" ]] || {
      echo "Fresh VM acceptance evidence target is unsafe: ${acceptance_evidence}" >&2
      exit 1
    }
  fi
  emitted_at="$(utc_now)"
  acceptance_tmp="$(mktemp "${acceptance_evidence}.tmp.XXXXXX")"
  jq -n --arg schema 'subrouter.gcp.deploy-evidence/v1' \
    --arg evidence_type fresh-vm-acceptance --arg run_id "${run_label}" \
    --arg project "${project_id}" --arg zone "${zone}" --arg instance "${instance_name}" \
    --arg bootstrap_sha "${bootstrap_sha256}" --arg bootstrap_run_id "${bootstrap_run_id}" \
    --arg bootstrap_emitted_at "${bootstrap_emitted_at}" --arg instance_id "${fresh_instance_id}" \
    --arg instance_creation_timestamp "${fresh_instance_creation_timestamp}" \
    --arg base_url "${server_url%/}" --arg emitted_at "${emitted_at}" \
    --argjson release "$(jq '.release' "${bootstrap_snapshot}")" \
    --argjson startup_metadata "$(jq '.startup_metadata' "${bootstrap_snapshot}")" \
    --argjson artifacts "$(jq '.artifacts' "${bootstrap_snapshot}")" \
    --argjson topology "$(cat "${topology_tmp}")" \
    '{schema:$schema,evidence_type:$evidence_type,mode:"post-publish",success:true,
      run:{id:$run_id,project:$project,zone:$zone,instance:$instance},release:$release,
      bootstrap_evidence:{sha256:$bootstrap_sha,evidence_type:"vm-provision",topology_state:"prepared",
        evidence_emitted_at:$bootstrap_emitted_at},
      instance:{created:true,id:$instance_id,creation_timestamp:$instance_creation_timestamp,
        bootstrap_run_id:$bootstrap_run_id},
      startup_metadata:$startup_metadata,artifacts:$artifacts,
      public:{base_url:$base_url,health:true,ready:true},topology:$topology,
      evidence_emitted_at:$emitted_at}' >"${acceptance_tmp}"
  python3 "${script_dir}/validate-deploy-evidence.py" --expect fresh-vm-acceptance "${acceptance_tmp}" >/dev/null
  chmod 0600 "${acceptance_tmp}"
  mv -f -- "${acceptance_tmp}" "${acceptance_evidence}"
  acceptance_tmp=""
  echo "Fresh VM acceptance evidence: ${acceptance_evidence}"
fi

echo "Public health and readiness passed for ${server_url%/}."
