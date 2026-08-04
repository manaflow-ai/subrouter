#!/usr/bin/env bash
# Shared deployment lock whose lifetime is tied to the owning local shell.

lock_holder_pid="${lock_holder_pid:-}"
lock_heartbeat_pid="${lock_heartbeat_pid:-}"
lock_pipe_path=""
lock_pipe_dir=""

subrouter_release_deploy_lock() {
  local attempt
  if [[ -n "${lock_heartbeat_pid}" ]]; then
    kill -TERM "${lock_heartbeat_pid}" >/dev/null 2>&1 || true
    wait "${lock_heartbeat_pid}" >/dev/null 2>&1 || true
    lock_heartbeat_pid=""
  fi
  if [[ -n "${lock_holder_pid}" ]]; then
    for ((attempt = 0; attempt < 100; attempt++)); do
      kill -0 "${lock_holder_pid}" >/dev/null 2>&1 || break
      sleep 0.1
    done
    if kill -0 "${lock_holder_pid}" >/dev/null 2>&1; then
      kill -TERM "${lock_holder_pid}" >/dev/null 2>&1 || true
      for ((attempt = 0; attempt < 20; attempt++)); do
        kill -0 "${lock_holder_pid}" >/dev/null 2>&1 || break
        sleep 0.1
      done
    fi
    if kill -0 "${lock_holder_pid}" >/dev/null 2>&1; then
      kill -KILL "${lock_holder_pid}" >/dev/null 2>&1 || true
    fi
    wait "${lock_holder_pid}" >/dev/null 2>&1 || true
    lock_holder_pid=""
  fi
  if [[ -n "${lock_pipe_path}" && -p "${lock_pipe_path}" ]]; then
    unlink "${lock_pipe_path}" >/dev/null 2>&1 || true
  fi
  if [[ -n "${lock_pipe_dir}" && -d "${lock_pipe_dir}" ]]; then
    rmdir "${lock_pipe_dir}" >/dev/null 2>&1 || true
  fi
  lock_pipe_path=""
  lock_pipe_dir=""
}

subrouter_acquire_deploy_lock() {
  local log_file="$1"
  local gcloud_binary="$2"
  local instance="$3"
  local project_id="$4"
  local zone="$5"
  local deploy_lock_file="$6"
  local owner_cleanup_label="${7:-}"
  local heartbeat_interval="${SUBROUTER_DEPLOY_LOCK_HEARTBEAT_INTERVAL_SECONDS:-10}"
  local heartbeat_ack_timeout="${SUBROUTER_DEPLOY_LOCK_ACK_TIMEOUT_SECONDS:-30}"
  local heartbeat_timeout="${SUBROUTER_DEPLOY_LOCK_HEARTBEAT_TIMEOUT_SECONDS:-300}"
  local owner_pid="$$"
  local attempt heartbeat_ack_attempts owner_cleanup_command required_command

  [[ -z "${lock_holder_pid}" && -z "${lock_heartbeat_pid}" ]] || {
    echo "deployment lock is already held by this process" >&2
    return 1
  }
  [[ "${deploy_lock_file}" =~ ^/[A-Za-z0-9._/-]+$ ]] || {
    echo "deployment lock path is invalid" >&2
    return 1
  }
  [[ -z "${owner_cleanup_label}" || "${owner_cleanup_label}" =~ ^[A-Za-z0-9._-]+$ ]] || {
    echo "deployment lock owner cleanup label is invalid" >&2
    return 1
  }
  [[ "${heartbeat_interval}" =~ ^[0-9]+([.][0-9]+)?$ &&
     ! "${heartbeat_interval}" =~ ^0+([.]0+)?$ ]] || {
    echo "deployment lock heartbeat interval is invalid" >&2
    return 1
  }
  [[ "${heartbeat_timeout}" =~ ^[0-9]+$ && "${heartbeat_timeout}" != 0 ]] || {
    echo "deployment lock heartbeat timeout is invalid" >&2
    return 1
  }
  [[ "${heartbeat_ack_timeout}" =~ ^[0-9]+$ && "${heartbeat_ack_timeout}" != 0 ]] || {
    echo "deployment lock acknowledgement timeout is invalid" >&2
    return 1
  }
  for required_command in "${gcloud_binary}" awk chmod grep kill mkfifo mktemp rmdir sleep unlink; do
    command -v "${required_command}" >/dev/null 2>&1 || {
      echo "deployment lock command is unavailable: ${required_command}" >&2
      return 1
    }
  done
  awk -v interval="${heartbeat_interval}" -v ack_timeout="${heartbeat_ack_timeout}" \
    -v heartbeat_timeout="${heartbeat_timeout}" \
    'BEGIN { exit !(interval < ack_timeout && ack_timeout < heartbeat_timeout) }' || {
    echo "deployment lock timing must satisfy heartbeat interval < acknowledgement timeout < remote timeout" >&2
    return 1
  }
  heartbeat_ack_attempts=$((heartbeat_ack_timeout * 10))

  if [[ -L "${log_file}" ]]; then
    echo "deployment lock log must not be a symlink: ${log_file}" >&2
    return 1
  fi
  if ! : >"${log_file}" || ! chmod 0600 "${log_file}"; then
    echo "deployment lock log is not writable: ${log_file}" >&2
    return 1
  fi

  lock_pipe_dir="$(mktemp -d "${TMPDIR:-/tmp}/subrouter-deploy-lock.XXXXXX")" || return 1
  lock_pipe_path="${lock_pipe_dir}/heartbeat"
  if ! mkfifo "${lock_pipe_path}" || ! chmod 0600 "${lock_pipe_path}"; then
    subrouter_release_deploy_lock
    return 1
  fi

  owner_cleanup_command=""
  if [[ -n "${owner_cleanup_label}" ]]; then
    owner_cleanup_command="trap \"rm -f -- /tmp/subrouter-rss-${owner_cleanup_label}-*.running /tmp/install-front-slots-${owner_cleanup_label}.sh /tmp/deployment-contract-${owner_cleanup_label}.py\" EXIT;"
  fi

  "${gcloud_binary}" compute ssh "${instance}" \
    --project "${project_id}" --zone "${zone}" --tunnel-through-iap --quiet \
    --command "sudo flock -x -w 300 '${deploy_lock_file}' bash -c '${owner_cleanup_command} echo LOCKED; while IFS= read -r -t ${heartbeat_timeout} heartbeat; do echo ACK \"\${heartbeat}\"; done'" \
    <"${lock_pipe_path}" >"${log_file}" 2>&1 &
  lock_holder_pid=$!
  exec 9>"${lock_pipe_path}"
  (
    trap '' PIPE
    heartbeat_sleep_pid=""
    # shellcheck disable=SC2329 # Invoked by the signal trap below.
    stop_heartbeat() {
      trap - TERM INT
      if [[ -n "${heartbeat_sleep_pid}" ]]; then
        kill -TERM "${heartbeat_sleep_pid}" >/dev/null 2>&1 || true
        wait "${heartbeat_sleep_pid}" >/dev/null 2>&1 || true
      fi
      exit 0
    }
    trap stop_heartbeat TERM INT
    while ! grep -qx LOCKED "${log_file}" 9>&- 2>/dev/null; do
      if ! kill -0 "${owner_pid}" 2>/dev/null; then
        exit 0
      fi
      if ! kill -0 "${lock_holder_pid}" 2>/dev/null; then
        kill -TERM "${owner_pid}" >/dev/null 2>&1 || true
        exit 1
      fi
      sleep 0.1 9>&- >/dev/null 2>&1 &
      heartbeat_sleep_pid=$!
      wait "${heartbeat_sleep_pid}" >/dev/null 2>&1 || true
      heartbeat_sleep_pid=""
    done
    heartbeat_sequence=0
    while kill -0 "${owner_pid}" 2>/dev/null; do
      if ! kill -0 "${lock_holder_pid}" 2>/dev/null; then
        kill -TERM "${owner_pid}" >/dev/null 2>&1 || true
        exit 1
      fi
      heartbeat_sequence=$((heartbeat_sequence + 1))
      if ! printf '%s\n' "${heartbeat_sequence}" >&9; then
        kill -TERM "${owner_pid}" >/dev/null 2>&1 || true
        exit 1
      fi
      heartbeat_acknowledged=false
      for ((ack_attempt = 0; ack_attempt < heartbeat_ack_attempts; ack_attempt++)); do
        if grep -qx "ACK ${heartbeat_sequence}" "${log_file}" 9>&- 2>/dev/null; then
          heartbeat_acknowledged=true
          break
        fi
        if ! kill -0 "${lock_holder_pid}" 2>/dev/null; then
          break
        fi
        sleep 0.1 9>&- >/dev/null 2>&1 &
        heartbeat_sleep_pid=$!
        wait "${heartbeat_sleep_pid}" >/dev/null 2>&1 || true
        heartbeat_sleep_pid=""
      done
      if [[ "${heartbeat_acknowledged}" != true ]]; then
        kill -TERM "${owner_pid}" >/dev/null 2>&1 || true
        exit 1
      fi
      sleep "${heartbeat_interval}" 9>&- >/dev/null 2>&1 &
      heartbeat_sleep_pid=$!
      wait "${heartbeat_sleep_pid}" >/dev/null 2>&1 || true
      heartbeat_sleep_pid=""
    done
  ) &
  lock_heartbeat_pid=$!
  exec 9>&-
  unlink "${lock_pipe_path}"
  lock_pipe_path=""
  rmdir "${lock_pipe_dir}"
  lock_pipe_dir=""

  for ((attempt = 0; attempt < 3100; attempt++)); do
    if grep -qx LOCKED "${log_file}" 2>/dev/null; then
      break
    fi
    if ! kill -0 "${lock_holder_pid}" 2>/dev/null; then
      echo "remote deployment lock holder exited before acquisition" >&2
      subrouter_release_deploy_lock
      return 1
    fi
    sleep 0.1
  done
  if ! grep -qx LOCKED "${log_file}" 2>/dev/null; then
    echo "timed out acquiring ${deploy_lock_file}" >&2
    subrouter_release_deploy_lock
    return 1
  fi

}
