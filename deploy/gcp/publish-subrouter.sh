#!/usr/bin/env bash
set -euo pipefail

instance_name="${INSTANCE_NAME:-subrouter-team}"
server_name="${SERVER_NAME:-team}"
zone="${ZONE:-us-south1-a}"
server_url="${SERVER_URL:-}"
subrouter_version="${1:-${SUBROUTER_RELEASE_TAG:-${SUBROUTER_VERSION:-}}}"
sr_bin="${SR_BIN:-sr}"

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
if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required." >&2
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

deploy_lock_file="${SUBROUTER_DEPLOY_LOCK_FILE:-/run/lock/subrouter-deploy.lock}"
run_label="publish-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$"
run_label="${run_label//[^a-zA-Z0-9._-]/-}"
remote_lock_sentinel="/tmp/subrouter-deploy-lock-${run_label}"
lock_log="$(mktemp "${TMPDIR:-/tmp}/subrouter-publish-lock.XXXXXX")"
lock_holder_pid=""

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
  release_deploy_lock
  rm -f -- "${lock_log}"
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

topology="$(gcloud_ssh 'if sudo test -S /var/lib/subrouter/front.sock || systemctl is-active --quiet subrouter-front.service; then echo front; else echo legacy; fi' | tail -n 1)"
if [[ "${topology}" != "legacy" ]]; then
  if [[ "${topology}" == "front" ]]; then
    echo "Front/slot topology is active. Use the protected GCP Deploy workflow for release changes." >&2
  else
    echo "Could not determine the remote deployment topology." >&2
  fi
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

for _ in $(seq 1 30); do
  if curl -fsS --max-time 5 "${server_url%/}/_subrouter/health" >/dev/null 2>&1 &&
      curl -fsS --max-time 5 "${server_url%/}/_subrouter/ready" >/dev/null 2>&1; then
    echo "Public health and readiness passed for ${server_url%/}."
    exit 0
  fi
  sleep 1
done
echo "Public health or readiness did not pass for ${server_url%/}." >&2
exit 1
