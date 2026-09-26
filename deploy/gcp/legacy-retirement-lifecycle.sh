#!/usr/bin/env bash

# Finalize an already-drained legacy service while its sampler is still live.
# The caller provides utc_now, epoch_millis, disable_legacy_units,
# wait_for_legacy_absence, stop_legacy_sampler, and die.
subrouter_finalize_legacy_after_drain() {
  local last_connection_closed_ms="${1:-}"
  local maximum_absence_latency_ms="${2:-}"
  local absent_ms

  [[ "${last_connection_closed_ms}" =~ ^[0-9]+$ ]] \
    || die "legacy retirement has no valid last-connection timestamp"
  [[ "${maximum_absence_latency_ms}" =~ ^[1-9][0-9]*$ ]] \
    || die "legacy retirement has no valid absence limit"

  stop_requested_at="$(utc_now)"
  disable_legacy_units
  wait_for_legacy_absence \
    || die "legacy service remained present before its absence deadline"
  absent_at="$(utc_now)"
  absent_ms="$(epoch_millis)"
  absence_latency_ms=$((absent_ms - last_connection_closed_ms))
  (( absence_latency_ms >= 0 && absence_latency_ms < maximum_absence_latency_ms )) \
    || die "legacy absence was not strictly below 30 seconds"

  # Sampler teardown can cross IAP many times. It must not consume the service
  # absence budget after systemd and the control socket already proved absence.
  stop_legacy_sampler
}
