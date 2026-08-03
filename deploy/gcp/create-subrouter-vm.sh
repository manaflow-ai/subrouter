#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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

if [[ ! "${release_tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  echo "SUBROUTER_RELEASE_TAG must name an explicit release such as v0.1.52." >&2
  exit 1
fi

if ! command -v gcloud >/dev/null 2>&1; then
  echo "gcloud is required. Install Google Cloud CLI first." >&2
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

gcloud services enable compute.googleapis.com --project "${project_id}" >/dev/null

if gcloud compute instances describe "${instance_name}" \
  --project "${project_id}" \
  --zone "${zone}" >/dev/null 2>&1; then
  echo "Instance already exists: ${instance_name} (${zone})"
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
    --metadata-from-file "startup-script=${script_dir}/startup.sh"
    --metadata "subrouter-release-tag=${release_tag}"
    --network "${network}"
  )
  if [[ -n "${subnet}" ]]; then
    args+=(--subnet "${subnet}")
  fi
  gcloud "${args[@]}"
fi

if ! gcloud compute firewall-rules describe subrouter-allow-lb \
  --project "${project_id}" >/dev/null 2>&1; then
  gcloud compute firewall-rules create subrouter-allow-lb \
    --project "${project_id}" \
    --network "${network}" \
    --priority 700 \
    --allow tcp:31415,tcp:31416 \
    --source-ranges 130.211.0.0/22,35.191.0.0/16 \
    --target-tags "${tags}" \
    --description "Google Cloud load balancer health checks and proxy traffic"
else
  gcloud compute firewall-rules update subrouter-allow-lb \
    --project "${project_id}" \
    --priority 700 \
    --allow tcp:31415,tcp:31416 \
    --source-ranges 130.211.0.0/22,35.191.0.0/16 \
    --target-tags "${tags}" >/dev/null
fi

if ! gcloud compute firewall-rules describe subrouter-allow-iap-ssh \
  --project "${project_id}" >/dev/null 2>&1; then
  gcloud compute firewall-rules create subrouter-allow-iap-ssh \
    --project "${project_id}" \
    --network "${network}" \
    --priority 750 \
    --allow tcp:22 \
    --source-ranges 35.235.240.0/20 \
    --target-tags "${tags}" \
    --description "Operator SSH through Google Cloud IAP"
else
  gcloud compute firewall-rules update subrouter-allow-iap-ssh \
    --project "${project_id}" \
    --priority 750 \
    --allow tcp:22 \
    --source-ranges 35.235.240.0/20 \
    --target-tags "${tags}" >/dev/null
fi

if ! gcloud compute firewall-rules describe subrouter-deny-public-ingress \
  --project "${project_id}" >/dev/null 2>&1; then
  gcloud compute firewall-rules create subrouter-deny-public-ingress \
    --project "${project_id}" \
    --network "${network}" \
    --priority 900 \
    --action DENY \
    --rules tcp,udp,icmp \
    --source-ranges 0.0.0.0/0 \
    --target-tags "${tags}" \
    --description "Deny public ingress to Subrouter hosts except higher-priority explicit allows"
fi

echo "Instance:"
gcloud compute instances describe "${instance_name}" \
  --project "${project_id}" \
  --zone "${zone}" \
  --format='table(name,zone.basename(),machineType.basename(),networkInterfaces[0].accessConfigs[0].natIP,status)'

echo
echo "Next:"
echo "  curl -fsSL https://github.com/manaflow-ai/subrouter/releases/download/${release_tag}/install.sh | SUBROUTER_VERSION=${release_tag} sh"
echo "  deploy/gcp/publish-subrouter.sh ${release_tag}"
echo
echo "Legacy port 31415 and front port 31416 accept traffic only from the Google Cloud load balancer."
