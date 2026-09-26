#!/usr/bin/env bash

subrouter_systemd_active_state_is_waitable() {
  case "${1:-}" in
    active|activating|deactivating|inactive|maintenance|reloading|refreshing) return 0 ;;
    *) return 1 ;;
  esac
}

subrouter_systemd_socket_state_is_waitable() {
  [[ "${1:-}" == not-found ]] || subrouter_systemd_active_state_is_waitable "${1:-}"
}

subrouter_legacy_sampler_stop_is_reconcilable() {
  case "${1:-}" in
    inactive|deactivating) ;;
    *) return 1 ;;
  esac
  subrouter_systemd_socket_state_is_waitable "${2:-}"
}
