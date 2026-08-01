#!/usr/bin/env bash
set -euo pipefail

instance_name="${INSTANCE_NAME:-subrouter-team}"
server_name="${SERVER_NAME:-team}"
zone="${ZONE:-us-south1-a}"
server_url="${SERVER_URL:-}"
subrouter_version="${SUBROUTER_VERSION:-latest}"
sr_bin="${SR_BIN:-sr}"

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

if ! command -v "${sr_bin}" >/dev/null 2>&1; then
  echo "sr is required. Install it with:" >&2
  echo "  curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sh" >&2
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

"${sr_bin}" server add "${server_name}" \
	--url "${server_url}" \
	--gcp-instance "${instance_name}" \
	--gcp-zone "${zone}" \
	--gcp-project "${project_id}" \
	--default

"${sr_bin}" server install "${server_name}" \
  --version "${subrouter_version}"

echo "Public health check:"
echo "  curl ${server_url}/_subrouter/health"
