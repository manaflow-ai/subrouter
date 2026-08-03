#!/usr/bin/env bash
# Create or verify a GCP VM whose startup metadata is bound to a fully verified,
# immutable release asset set. Emits canonical evidence only after the VM proves
# its stopped prepared or authenticated active front/slot topology.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: create-subrouter-vm.sh [--evidence-json PATH]

Requires SUBROUTER_RELEASE_TAG, SUBROUTER_RELEASE_ASSET_DIR, and
SUBROUTER_RELEASE_VERIFICATION_JSON from the local release/deploy orchestrator.
EOF
}

evidence_json=""
while (( $# > 0 )); do
  case "$1" in
    --evidence-json) (( $# >= 2 )) || { usage >&2; exit 2; }; evidence_json="$2"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_contract="$(bash "${script_dir}/resolve-release-contract.sh" "${script_dir}/deployment-contract.py")"
instance_name="${INSTANCE_NAME:-subrouter-team}"
zone="${ZONE:-us-south1-a}"
machine_type="${MACHINE_TYPE:-e2-micro}"
disk_size="${DISK_SIZE:-10GB}"
disk_type="${DISK_TYPE:-pd-standard}"
image_family="${IMAGE_FAMILY:-debian-12}"
image_project="${IMAGE_PROJECT:-debian-cloud}"
tags="${TAGS:-subrouter}"
network="${NETWORK:-default}"
subnet="${SUBNET:-}"
release_tag="${SUBROUTER_RELEASE_TAG:-}"
asset_dir="${SUBROUTER_RELEASE_ASSET_DIR:?set SUBROUTER_RELEASE_ASSET_DIR}"
verification_json="${SUBROUTER_RELEASE_VERIFICATION_JSON:?set SUBROUTER_RELEASE_VERIFICATION_JSON}"
artifact_dir="${SUBROUTER_DEPLOY_ARTIFACT_DIR:-${PWD}/artifacts/gcp-vm-provision}"
run_label="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-vm-$$"
run_label="${run_label//[^a-zA-Z0-9._-]/-}"

log() { printf 'create-subrouter-vm: %s\n' "$*"; }
die() { log "$*" >&2; exit 1; }
sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}
utc_now() { python3 -c 'from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))'; }

instance_identity_json=""
query_instance_identity() {
  local instance_json_file
  local parsed_identity
  instance_json_file="$(mktemp "${TMPDIR:-/tmp}/subrouter-gce-instance.json.XXXXXX")"
  if ! gcloud compute instances describe "${instance_name}" \
      --project "${project_id}" --zone "${zone}" --format=json >"${instance_json_file}"; then
    rm -f -- "${instance_json_file}"
    die "could not query GCE instance identity"
  fi
  if ! parsed_identity="$(python3 "${deployment_contract}" gce-instance-identity \
      "${instance_json_file}" "${instance_name}" "${zone}")"; then
    rm -f -- "${instance_json_file}"
    die "could not validate GCE instance identity"
  fi
  rm -f -- "${instance_json_file}"
  instance_identity_json="${parsed_identity}"
}

[[ "${release_tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] \
  || die "SUBROUTER_RELEASE_TAG must name an explicit release such as v0.1.52"
for command in gcloud gh go jq python3; do
  command -v "${command}" >/dev/null 2>&1 || die "${command} is required"
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
  die "sha256sum or shasum is required"
fi
[[ -d "${asset_dir}" && -f "${verification_json}" && ! -L "${verification_json}" ]] \
  || die "verified release asset directory or verification evidence is missing"
[[ -f "${deployment_contract}" && ! -L "${deployment_contract}" ]] \
  || die "deployment contract is missing or unsafe"
python3 "${deployment_contract}" validate-private-file "${verification_json}"

version="${release_tag#v}"
binary_asset="subrouter_${version}_linux_amd64"
required_assets=(SHA256SUMS SOURCE_PROVENANCE.json deployment-contract.py install.sh install-front-slots.sh "${binary_asset}")
jq -e --arg tag "${release_tag}" --arg binary "${binary_asset}" '
  (. | keys | sort) == (["schema","release_tag","source_revision","tag_on_main",
    "release_published","release_immutable","asset_digest_verified",
    "strict_build_attestation_verified","provenance_verified",
    "embedded_revision_verified","assets"] | sort) and
  .schema == "subrouter.release-verification/v1" and .release_tag == $tag and
  (.source_revision | type == "string" and test("^[0-9a-f]{40}$")) and
  .tag_on_main == true and .release_published == true and .release_immutable == true and
  .asset_digest_verified == true and .strict_build_attestation_verified == true and
  .provenance_verified == true and .embedded_revision_verified == true and
  ((.assets | keys | sort) == (["SHA256SUMS","SOURCE_PROVENANCE.json","install.sh",
    "deployment-contract.py","install-front-slots.sh",$binary] | sort)) and
  ([.assets[]] | all(type == "string" and test("^[0-9a-f]{64}$")))
' "${verification_json}" >/dev/null || die "release verification evidence is invalid"
release_revision="$(jq -r '.source_revision' "${verification_json}")"

for asset in "${required_assets[@]}"; do
  path="${asset_dir}/${asset}"
  [[ -f "${path}" && ! -L "${path}" ]] || die "release asset is missing or unsafe: ${asset}"
  expected="$(jq -r --arg asset "${asset}" '.assets[$asset]' "${verification_json}")"
  [[ "$(sha256_file "${path}")" == "${expected}" ]] || die "release verification digest mismatch: ${asset}"
done
for asset in SOURCE_PROVENANCE.json deployment-contract.py install.sh install-front-slots.sh "${binary_asset}"; do
  expected="$(jq -r --arg asset "${asset}" '.assets[$asset]' "${verification_json}")"
  manifest_matches="$(awk -v asset="${asset}" '$2 == asset || $2 == "*" asset {print $1}' "${asset_dir}/SHA256SUMS")"
  [[ "$(wc -l <<<"${manifest_matches}" | tr -d '[:space:]')" == 1 && "${manifest_matches}" == "${expected}" ]] \
    || die "SHA256SUMS does not bind ${asset} to release verification"
done
jq -e --arg tag "${release_tag}" --arg revision "${release_revision}" \
  '.tag == $tag and .source_revision == $revision and .tag_on_main == true' \
  "${asset_dir}/SOURCE_PROVENANCE.json" >/dev/null || die "source provenance does not match release verification"
binary_metadata="$(go version -m "${asset_dir}/${binary_asset}")"
grep -Fq "vcs.revision=${release_revision}" <<<"${binary_metadata}" || die "binary embedded revision mismatch"
grep -Fq 'vcs.modified=false' <<<"${binary_metadata}" || die "binary embedded metadata reports modified source"

# Re-run the remote release and strict attestation checks here so VM creation
# cannot trust a caller-authored boolean-only verification document.
release_state="$(gh release view "${release_tag}" --repo manaflow-ai/subrouter --json isDraft,isImmutable,tagName)"
jq -e --arg tag "${release_tag}" '.tagName == $tag and (.isDraft | not) and .isImmutable' \
  <<<"${release_state}" >/dev/null || die "release is missing, draft, or mutable"
compare_state="$(gh api "repos/manaflow-ai/subrouter/compare/${release_revision}...main")"
jq -e --arg revision "${release_revision}" \
  '.merge_base_commit.sha == $revision and (.status == "ahead" or .status == "identical")' \
  <<<"${compare_state}" >/dev/null || die "release tag commit is not on protected main"
for asset in "${required_assets[@]}"; do
  path="${asset_dir}/${asset}"
  gh release verify-asset "${release_tag}" "${path}" --repo manaflow-ai/subrouter --format json >/dev/null \
    || die "immutable release asset verification failed: ${asset}"
  attestation="$(gh attestation verify "${path}" --repo manaflow-ai/subrouter \
    --signer-workflow manaflow-ai/subrouter/.github/workflows/release.yml \
    --source-ref "refs/tags/${release_tag}" --source-digest "${release_revision}" \
    --deny-self-hosted-runners --format json)"
  jq -e 'length > 0' <<<"${attestation}" >/dev/null \
    || die "strict build attestation verification failed: ${asset}"
done

mkdir -p "${artifact_dir}"
artifact_dir="$(cd "${artifact_dir}" && pwd)"
evidence_json="${evidence_json:-${artifact_dir}/result.json}"
mkdir -p "$(dirname "${evidence_json}")"
evidence_json="$(cd "$(dirname "${evidence_json}")" && pwd)/$(basename "${evidence_json}")"
verification_sha256="$(sha256_file "${verification_json}")"
metadata_file="$(mktemp "${artifact_dir}/vm-release-metadata.json.XXXXXX")"
cleanup_files=("${metadata_file}")
cleanup() {
  rm -f -- "${cleanup_files[@]}"
}
trap cleanup EXIT INT TERM
jq -S -n --arg schema 'subrouter.gcp.vm-release-metadata/v1' \
  --arg repository manaflow-ai/subrouter --arg tag "${release_tag}" \
  --arg revision "${release_revision}" --arg verification_sha "${verification_sha256}" \
  --argjson assets "$(jq -S '.assets' "${verification_json}")" \
  '{schema:$schema,repository:$repository,release_tag:$tag,source_revision:$revision,
    tag_on_main:true,release_immutable:true,strict_build_attestation_verified:true,
    asset_digest_verified:true,provenance_verified:true,embedded_revision_verified:true,
    verification_evidence_sha256:$verification_sha,assets:$assets}' >"${metadata_file}"
chmod 0600 "${metadata_file}"
metadata_sha256="$(sha256_file "${metadata_file}")"

active_account="$(gcloud config get-value account 2>/dev/null || true)"
[[ -n "${active_account}" && "${active_account}" != "(unset)" ]] \
  || die "no active gcloud account; run gcloud auth login"
project_id="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}"
[[ -n "${project_id}" && "${project_id}" != "(unset)" ]] \
  || die "no GCP project configured; run gcloud config set project <project-id>"

gcloud services enable compute.googleapis.com --project "${project_id}" >/dev/null
instance_created=false
if instance_json="$(gcloud compute instances describe "${instance_name}" \
    --project "${project_id}" --zone "${zone}" --format=json 2>/dev/null)"; then
  existing_metadata_sha="$(jq -r '[.metadata.items[]? | select(.key == "subrouter-release-metadata-sha256")][0].value // empty' <<<"${instance_json}")"
  existing_content_sha="$(python3 -c 'import hashlib,json,sys; document=json.load(sys.stdin); values=[item.get("value","") for item in document.get("metadata",{}).get("items",[]) if item.get("key")=="subrouter-release-metadata"]; print(hashlib.sha256(values[0].encode()).hexdigest() if len(values)==1 else "")' <<<"${instance_json}")"
  [[ "${existing_metadata_sha}" == "${metadata_sha256}" ]] \
    || die "existing instance release metadata digest differs; rebuild explicitly instead of mutating it"
  [[ "${existing_content_sha}" == "${metadata_sha256}" ]] \
    || die "existing instance release metadata content differs"
  log "instance already exists with the exact verified release metadata"
else
  args=(
    compute instances create "${instance_name}"
    --project "${project_id}"
    --zone "${zone}"
    --machine-type "${machine_type}"
    --image-family "${image_family}"
    --image-project "${image_project}"
    --boot-disk-size "${disk_size}"
    --boot-disk-type "${disk_type}"
    --boot-disk-auto-delete
    --tags "${tags}"
    --labels "app=subrouter,managed-by=subrouter"
    --metadata-from-file "startup-script=${script_dir}/startup.sh,subrouter-release-metadata=${metadata_file}"
    --metadata "subrouter-release-tag=${release_tag},subrouter-release-metadata-sha256=${metadata_sha256}"
    --network "${network}"
  )
  [[ -z "${subnet}" ]] || args+=(--subnet "${subnet}")
  gcloud "${args[@]}"
  instance_created=true
fi

query_instance_identity
provisioned_instance_identity="${instance_identity_json}"

if ! gcloud compute firewall-rules describe subrouter-allow-lb --project "${project_id}" >/dev/null 2>&1; then
  gcloud compute firewall-rules create subrouter-allow-lb --project "${project_id}" --network "${network}" \
    --priority 700 --allow tcp:31415,tcp:31416 --source-ranges 130.211.0.0/22,35.191.0.0/16 \
    --target-tags "${tags}" --description "Google Cloud load balancer health checks and proxy traffic"
else
  gcloud compute firewall-rules update subrouter-allow-lb --project "${project_id}" --priority 700 \
    --allow tcp:31415,tcp:31416 --source-ranges 130.211.0.0/22,35.191.0.0/16 --target-tags "${tags}" >/dev/null
fi
if ! gcloud compute firewall-rules describe subrouter-allow-iap-ssh --project "${project_id}" >/dev/null 2>&1; then
  gcloud compute firewall-rules create subrouter-allow-iap-ssh --project "${project_id}" --network "${network}" \
    --priority 750 --allow tcp:22 --source-ranges 35.235.240.0/20 --target-tags "${tags}" \
    --description "Operator SSH through Google Cloud IAP"
else
  gcloud compute firewall-rules update subrouter-allow-iap-ssh --project "${project_id}" --priority 750 \
    --allow tcp:22 --source-ranges 35.235.240.0/20 --target-tags "${tags}" >/dev/null
fi
if ! gcloud compute firewall-rules describe subrouter-deny-public-ingress --project "${project_id}" >/dev/null 2>&1; then
  gcloud compute firewall-rules create subrouter-deny-public-ingress --project "${project_id}" --network "${network}" \
    --priority 900 --action DENY --rules tcp,udp,icmp --source-ranges 0.0.0.0/0 --target-tags "${tags}" \
    --description "Deny public ingress to Subrouter hosts except higher-priority explicit allows"
fi

remote_probe="/tmp/subrouter-verify-fresh-vm-${run_label}.sh"
cleanup_files+=("${artifact_dir}/topology.json")
topology_file="${artifact_dir}/topology.json"
topology_ready=false
for _ in $(seq 1 180); do
  if gcloud compute scp "${script_dir}/verify-fresh-vm.sh" "${instance_name}:${remote_probe}" \
      --project "${project_id}" --zone "${zone}" --tunnel-through-iap --quiet >/dev/null 2>&1 &&
      gcloud compute ssh "${instance_name}" --project "${project_id}" --zone "${zone}" \
        --tunnel-through-iap --quiet \
        --command "sudo env SUBROUTER_EXPECTED_SHA256='$(jq -r --arg asset "${binary_asset}" '.assets[$asset]' "${verification_json}")' SUBROUTER_RELEASE_TAG='${release_tag}' bash '${remote_probe}'" \
        >"${topology_file}" 2>/dev/null &&
      jq -e '.kind == "front-slots" and (.state == "prepared" or .state == "active")' \
        "${topology_file}" >/dev/null 2>&1; then
    topology_ready=true
    break
  fi
  sleep 2
done
gcloud compute ssh "${instance_name}" --project "${project_id}" --zone "${zone}" \
  --tunnel-through-iap --quiet --command "rm -f '${remote_probe}'" >/dev/null 2>&1 || true
[[ "${topology_ready}" == true ]] || die "fresh VM did not prove the canonical front/slot topology"

query_instance_identity
[[ "${instance_identity_json}" == "${provisioned_instance_identity}" ]] \
  || die "GCE instance identity changed while VM topology was being verified"
instance_id="$(jq -r '.id' <<<"${provisioned_instance_identity}")"
instance_creation_timestamp="$(jq -r '.creation_timestamp' <<<"${provisioned_instance_identity}")"

emitted_at="$(utc_now)"
evidence_tmp="$(mktemp "${evidence_json}.tmp.XXXXXX")"
cleanup_files+=("${evidence_tmp}")
jq -n --arg schema 'subrouter.gcp.deploy-evidence/v1' --arg evidence_type vm-provision \
  --arg run_id "${run_label}" --arg project "${project_id}" --arg zone "${zone}" \
  --arg instance "${instance_name}" --arg tag "${release_tag}" --arg revision "${release_revision}" \
  --arg binary_sha "$(jq -r --arg asset "${binary_asset}" '.assets[$asset]' "${verification_json}")" \
  --arg metadata_sha "${metadata_sha256}" --arg verification_sha "${verification_sha256}" \
  --argjson created "${instance_created}" --arg instance_id "${instance_id}" \
  --arg instance_creation_timestamp "${instance_creation_timestamp}" \
  --argjson assets "$(jq '.assets' "${verification_json}")" \
  --argjson topology "$(cat "${topology_file}")" --arg emitted_at "${emitted_at}" \
  '{schema:$schema,evidence_type:$evidence_type,mode:"fresh-front-slots",success:true,
    mutation_performed:$created,run:{id:$run_id,project:$project,zone:$zone,instance:$instance},
    release:{tag:$tag,sha256:$binary_sha,source_revision:$revision,tag_on_main:true,
      attestation_verified:true,immutable:true},
    startup_metadata:{schema:"subrouter.gcp.vm-release-metadata/v1",sha256:$metadata_sha,
      verification_evidence_sha256:$verification_sha},artifacts:$assets,
    instance:{created:$created,id:$instance_id,creation_timestamp:$instance_creation_timestamp},
    topology:$topology,evidence_emitted_at:$emitted_at}' \
  >"${evidence_tmp}"
python3 "${script_dir}/validate-deploy-evidence.py" --expect vm-provision "${evidence_tmp}" >/dev/null
chmod 0600 "${evidence_tmp}"
mv -f -- "${evidence_tmp}" "${evidence_json}"
cleanup_files=("${metadata_file}" "${topology_file}")
log "fresh VM front/slot topology verified"
jq -c . "${evidence_json}"
