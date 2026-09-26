#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
image="${SUBROUTER_DOCKER_IMAGE:-subrouter:docker-smoke}"
run_id="${GITHUB_RUN_ID:-local}-$$"
run_id="${run_id//[^a-zA-Z0-9_.-]/-}"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/subrouter-docker-smoke.XXXXXX")"
mock_pid=""
container=""
volumes=()
compose_profile=""
compose_project="subrouter-smoke-compose-${run_id}"
compose_secret_dir="${work_dir}/compose-secrets"
compose_state_dir="${work_dir}/compose-state"
compose_port=""
context_probe_image="subrouter:docker-context-${run_id}"
state_probe_dir="${repo_root}/deploy/docker/state"
state_probe_file=""
state_probe_dir_created=false

run_compose() {
  local profile="$1"
  shift
  COMPOSE_PROJECT_NAME="${compose_project}" \
    SUBROUTER_DOCKER_IMAGE="${image}" \
    SUBROUTER_DOCKER_SECRET_DIR="${compose_secret_dir}" \
    SUBROUTER_DOCKER_STATE_DIR="${compose_state_dir}" \
    SUBROUTER_PORT="${compose_port}" \
    "${repo_root}/deploy/docker/compose.sh" "${profile}" "$@"
}

cleanup() {
  set +e
  if [[ -n "${compose_profile}" ]]; then
    run_compose "${compose_profile}" down -v >/dev/null 2>&1
  fi
  [[ -z "${container}" ]] || docker rm -fv "${container}" >/dev/null 2>&1
  for volume in "${volumes[@]:-}"; do
    [[ -z "${volume}" ]] || docker volume rm "${volume}" >/dev/null 2>&1
  done
  [[ -z "${mock_pid}" ]] || kill "${mock_pid}" >/dev/null 2>&1
  [[ -z "${state_probe_file}" ]] || rm -f -- "${state_probe_file}"
  if [[ "${state_probe_dir_created}" == true ]]; then
    rmdir "${state_probe_dir}" >/dev/null 2>&1 || true
  fi
  docker image rm "${context_probe_image}" >/dev/null 2>&1 || true
  case "${work_dir}" in
    "${TMPDIR:-/tmp}"/subrouter-docker-smoke.*) rm -rf "${work_dir}" ;;
  esac
}
trap cleanup EXIT INT TERM

for command in docker curl jq python3; do
  command -v "${command}" >/dev/null 2>&1 || {
    echo "docker-smoke: required command not found: ${command}" >&2
    exit 1
  }
done

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

mock_port="$(free_port)"
python3 "${repo_root}/deploy/docker/smoke-mock.py" --port "${mock_port}" \
  >"${work_dir}/mock.log" 2>&1 &
mock_pid=$!
for _ in $(seq 1 100); do
  curl -fsS --max-time 1 "http://127.0.0.1:${mock_port}/health" >/dev/null 2>&1 && break
  kill -0 "${mock_pid}" 2>/dev/null || {
    echo "docker-smoke: mock server exited" >&2
    exit 1
  }
  sleep 0.05
done
curl -fsS --max-time 1 "http://127.0.0.1:${mock_port}/health" >/dev/null

printf '%s\n' docker-proxy-token >"${work_dir}/proxy-token"
printf '%s\n' docker-admin-token >"${work_dir}/admin-token"
printf '%s\n' docker-import-token >"${work_dir}/account-import-token"
chmod 0444 "${work_dir}"/*-token

jq -n \
  --arg hosted_url "http://127.0.0.1:${mock_port}" \
  '{
    version: 1,
    baseUrl: "https://cmux.com",
    accessToken: "docker-stack-access",
    refreshToken: "docker-stack-refresh",
    localProxyToken: "docker-proxy-token",
    teamId: "docker-smoke-team",
    teamName: "Docker Smoke",
    credentialSource: "team",
    hostedUrl: $hosted_url,
    tenantKey: "srt_0123456789abcdef0123456789abcdef"
  }' >"${work_dir}/team-cloud.json"
chmod 0444 "${work_dir}/team-cloud.json"

# A generated state canary must never enter an intermediate build layer or a
# remote/shared Docker build cache.
if [[ ! -e "${state_probe_dir}" ]]; then
  mkdir -p "${state_probe_dir}"
  state_probe_dir_created=true
elif [[ ! -d "${state_probe_dir}" ]]; then
  echo "docker-smoke: state probe path is not a directory" >&2
  exit 1
fi
state_probe_file="${state_probe_dir}/.docker-context-probe-${run_id}"
[[ ! -e "${state_probe_file}" ]] || {
  echo "docker-smoke: state probe file already exists" >&2
  exit 1
}
printf '%s\n' 'must-not-enter-docker-build-context' >"${state_probe_file}"
docker build --pull --target build -t "${context_probe_image}" "${repo_root}"
docker run --rm --entrypoint /bin/sh "${context_probe_image}" -ec \
  'test ! -e /src/deploy/docker/state/'"$(basename "${state_probe_file}")"
rm -f -- "${state_probe_file}"
state_probe_file=""
if [[ "${state_probe_dir_created}" == true ]]; then
  rmdir "${state_probe_dir}"
  state_probe_dir_created=false
fi
if [[ "${SUBROUTER_DOCKER_CONTEXT_PROBE_ONLY:-}" == 1 ]]; then
  exit 0
fi

docker build --pull -t "${image}" "${repo_root}"

# Exercise the documented initializer followed by the actual Compose service.
# These host files remain 0600; the non-root runtime must still be able to read
# them without weakening their host permissions.
SUBROUTER_DOCKER_SECRET_DIR="${compose_secret_dir}" \
  SUBROUTER_DOCKER_STATE_DIR="${compose_state_dir}" \
  "${repo_root}/deploy/docker/init-secrets.sh" >/dev/null
compose_proxy_token="$(<"${compose_secret_dir}/proxy-token")"
jq -n \
  --arg hosted_url "http://127.0.0.1:${mock_port}" \
  --arg proxy_token "${compose_proxy_token}" \
  '{
    version: 1,
    baseUrl: "https://cmux.com",
    accessToken: "docker-stack-access",
    refreshToken: "docker-stack-refresh",
    localProxyToken: $proxy_token,
    teamId: "docker-smoke-team",
    teamName: "Docker Smoke",
    credentialSource: "team",
    hostedUrl: $hosted_url,
    tenantKey: "srt_0123456789abcdef0123456789abcdef"
  }' >"${compose_secret_dir}/team-cloud.json"
chmod 0600 "${compose_secret_dir}/team-cloud.json"
compose_port="$(free_port)"
compose_profile="local"
run_compose local up --no-build -d >/dev/null
compose_ready=false
for _ in $(seq 1 100); do
  if curl -fsS --max-time 1 "http://127.0.0.1:${compose_port}/_subrouter/health" >/dev/null 2>&1; then
    compose_ready=true
    break
  fi
  sleep 0.1
done
if [[ "${compose_ready}" != true ]]; then
  run_compose local logs --no-color >&2
  exit 1
fi
compose_container="$(run_compose local ps -q subrouter-local)"
[[ "$(docker inspect -f '{{.Config.User}}' "${compose_container}")" == "$(id -u):$(id -g)" ]] || {
  echo "docker-smoke: Compose runtime user does not match the secret owner" >&2
  exit 1
}
compose_admin_token="$(<"${compose_secret_dir}/admin-token")"
curl -fsS --max-time 1 -H "Authorization: Bearer ${compose_admin_token}" \
  "http://127.0.0.1:${compose_port}/_subrouter/accounts" >/dev/null
run_compose local down -v >/dev/null
compose_profile=""

compose_port="$(free_port)"
compose_profile="team"
run_compose team up --no-build -d >/dev/null
compose_ready=false
for _ in $(seq 1 100); do
  if curl -fsS --max-time 1 "http://127.0.0.1:${compose_port}/_subrouter/health" >/dev/null 2>&1; then
    compose_ready=true
    break
  fi
  sleep 0.1
done
if [[ "${compose_ready}" != true ]]; then
  run_compose team logs --no-color >&2
  exit 1
fi
[[ "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 1 \
  -H 'Content-Type: application/json' --data '{"model":"gpt-5.4","input":"auth probe"}' \
  "http://127.0.0.1:${compose_port}/v1/responses")" == "401" ]] || {
  echo "docker-smoke: team Compose proxy did not load its config-backed proxy secret" >&2
  exit 1
}
run_compose team down -v >/dev/null
compose_profile=""

# A bare image must fail closed without its control secrets.
standalone_name="subrouter-smoke-standalone-${run_id}"
standalone_port="$(free_port)"
container="${standalone_name}"
docker run -d --name "${standalone_name}" \
  -p "127.0.0.1:${standalone_port}:31415" "${image}" >/dev/null
for _ in $(seq 1 100); do
  docker inspect -f '{{.State.Running}}' "${container}" | grep -qx false && break
  sleep 0.1
done
if docker inspect -f '{{.State.Running}}' "${container}" | grep -qx true; then
  echo "docker-smoke: standalone image started without control secrets" >&2
  docker logs "${container}" >&2 || true
  exit 1
fi
docker rm -fv "${container}" >/dev/null
container=""

# Mounting all default control secrets must make the default image reachable
# through an explicitly loopback-published port while keeping admin APIs gated.
container="${standalone_name}"
docker run -d --name "${standalone_name}" \
  --mount "type=bind,src=${work_dir}/proxy-token,dst=/run/secrets/proxy_token,readonly" \
  --mount "type=bind,src=${work_dir}/admin-token,dst=/run/secrets/admin_token,readonly" \
  --mount "type=bind,src=${work_dir}/account-import-token,dst=/run/secrets/account_import_token,readonly" \
  -p "127.0.0.1:${standalone_port}:31415" "${image}" >/dev/null
for _ in $(seq 1 100); do
  curl -fsS --max-time 1 "http://127.0.0.1:${standalone_port}/_subrouter/health" >/dev/null 2>&1 && break
  docker inspect -f '{{.State.Running}}' "${container}" | grep -qx true || {
    docker logs "${container}" >&2 || true
    exit 1
  }
  sleep 0.1
done
curl -fsS --max-time 1 "http://127.0.0.1:${standalone_port}/_subrouter/health" >/dev/null
[[ "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 1 "http://127.0.0.1:${standalone_port}/_subrouter/accounts")" == "401" ]] || {
  echo "docker-smoke: standalone admin endpoint was not protected" >&2
  exit 1
}
curl -fsS --max-time 1 -H 'Authorization: Bearer docker-admin-token' \
  "http://127.0.0.1:${standalone_port}/_subrouter/accounts" >/dev/null
docker rm -fv "${container}" >/dev/null
container=""

wait_for_proxy() {
  local port="$1"
  for _ in $(seq 1 100); do
    curl -fsS --max-time 1 "http://127.0.0.1:${port}/_subrouter/health" >/dev/null 2>&1 && return 0
    docker inspect -f '{{.State.Running}}' "${container}" 2>/dev/null | grep -qx true || {
      docker logs "${container}" >&2 || true
      return 1
    }
    sleep 0.1
  done
  docker logs "${container}" >&2 || true
  return 1
}

run_load() {
  local port="$1" token="$2"
  seq 1 160 | xargs -P 32 -I '{}' \
    curl -fsS --max-time 20 \
      -H "Authorization: Bearer ${token}" \
      -H 'Content-Type: application/json' \
      --data '{"model":"gpt-5.4","input":"docker smoke"}' \
      "http://127.0.0.1:${port}/v1/responses" \
      -o /dev/null
}

assert_container_healthy() {
  local mode="$1"
  local state
  state="$(docker inspect -f '{{.State.OOMKilled}} {{.RestartCount}} {{.State.Status}}' "${container}")"
  [[ "${state}" == "false 0 running" ]] || {
    echo "docker-smoke: ${mode} state is ${state}" >&2
    docker logs "${container}" >&2 || true
    exit 1
  }
  printf 'docker-smoke: %s %s memory=%s\n' \
    "${mode}" "${state}" \
    "$(docker stats --no-stream --format '{{.MemUsage}}' "${container}")"
}

local_name="subrouter-smoke-local-${run_id}"
local_volume="subrouter-smoke-local-${run_id}"
local_port="$(free_port)"
container="${local_name}"
volumes+=("${local_volume}")
docker volume create "${local_volume}" >/dev/null
docker run -d \
  --name "${local_name}" --network host --init --read-only --user 65532:65532 \
  --cap-drop ALL --security-opt no-new-privileges:true --pids-limit 256 \
  --memory 256m --memory-swap 256m --cpus 2 \
  --tmpfs /tmp:rw,noexec,nosuid,size=16m,mode=1777 \
  --mount "type=volume,src=${local_volume},dst=/var/lib/subrouter" \
  --mount "type=bind,src=${work_dir}/proxy-token,dst=/run/secrets/proxy_token,readonly" \
  --mount "type=bind,src=${work_dir}/admin-token,dst=/run/secrets/admin_token,readonly" \
  --mount "type=bind,src=${work_dir}/account-import-token,dst=/run/secrets/account_import_token,readonly" \
  -e HOME=/var/lib/subrouter -e SUBROUTER_STATE_DIR=/var/lib/subrouter \
  -e GOMEMLIMIT=192MiB -e GOMAXPROCS=2 \
  -e SUBROUTER_PROXY_TOKEN_FILE=/run/secrets/proxy_token \
  -e SUBROUTER_ADMIN_TOKEN_FILE=/run/secrets/admin_token \
  -e SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE=/run/secrets/account_import_token \
  "${image}" serve --addr "127.0.0.1:${local_port}" \
    --sessions /var/lib/subrouter/sessions.json \
    --api-upstream "http://127.0.0.1:${mock_port}" \
    --fetch-usage=false --sr-switch-interval=0 >/dev/null
container="${local_name}"
wait_for_proxy "${local_port}"
curl --fail-with-body -sS --max-time 5 \
  -H 'Authorization: Bearer docker-import-token' \
  -H 'Content-Type: application/json' \
  --data '{"provider":"codex","codex":{"email":"apikey:docker","addedAt":"2026-08-01T00:00:00Z","auth":{"auth_mode":"apikey","OPENAI_API_KEY":"sk-docker-upstream-token"}}}' \
  "http://127.0.0.1:${local_port}/_subrouter/account-import" >/dev/null
run_load "${local_port}" docker-proxy-token
assert_container_healthy local
docker rm -f "${container}" >/dev/null
container=""
docker volume rm "${local_volume}" >/dev/null
volumes=()

team_name="subrouter-smoke-team-${run_id}"
team_volume="subrouter-smoke-team-${run_id}"
team_port="$(free_port)"
standalone_team_proxy_token="$(jq -r '.localProxyToken' "${work_dir}/team-cloud.json")"
container="${team_name}"
volumes+=("${team_volume}")
docker volume create "${team_volume}" >/dev/null
docker run -d \
  --name "${team_name}" --network host --init --read-only --user 65532:65532 \
  --cap-drop ALL --security-opt no-new-privileges:true --pids-limit 256 \
  --memory 256m --memory-swap 256m --cpus 2 \
  --tmpfs /tmp:rw,noexec,nosuid,size=16m,mode=1777 \
  --mount "type=volume,src=${team_volume},dst=/var/lib/subrouter" \
  --mount "type=bind,src=${work_dir}/team-cloud.json,dst=/run/secrets/team_cloud_config,readonly" \
  --mount "type=bind,src=${work_dir}/admin-token,dst=/run/secrets/admin_token,readonly" \
  --mount "type=bind,src=${work_dir}/account-import-token,dst=/run/secrets/account_import_token,readonly" \
  -e HOME=/var/lib/subrouter -e SUBROUTER_STATE_DIR=/var/lib/subrouter \
  -e GOMEMLIMIT=192MiB -e GOMAXPROCS=2 \
  -e SUBROUTER_PROXY_TOKEN_FILE= \
  -e SUBROUTER_CLOUD_CONFIG=/run/secrets/team_cloud_config \
  -e SUBROUTER_ADMIN_TOKEN_FILE=/run/secrets/admin_token \
  -e SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE=/run/secrets/account_import_token \
  "${image}" serve --addr "127.0.0.1:${team_port}" \
    --sessions /var/lib/subrouter/sessions.json \
    --cloud-credential-source team \
    --api-upstream "http://127.0.0.1:${mock_port}" \
    --fetch-usage=false --sr-switch-interval=0 >/dev/null
wait_for_proxy "${team_port}"
run_load "${team_port}" "${standalone_team_proxy_token}"
assert_container_healthy team
