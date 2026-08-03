#!/usr/bin/env bash
# Emit machine-readable proof that a rebuilt VM has the canonical front/slot
# topology and no bootable legacy serving path.
set -euo pipefail

EXPECTED_SHA256="${SUBROUTER_EXPECTED_SHA256:?set SUBROUTER_EXPECTED_SHA256}"
RELEASE_TAG="${SUBROUTER_RELEASE_TAG:?set SUBROUTER_RELEASE_TAG}"
RELEASE_ROOT="${SUBROUTER_RELEASE_ROOT:-/opt/subrouter/releases}"
SLOT_ROOT="${SUBROUTER_SLOT_ROOT:-/opt/subrouter/slots}"
FRONT_ROOT="${SUBROUTER_FRONT_ROOT:-/opt/subrouter/front}"
CONTROL_ROOT="${SUBROUTER_CONTROL_ROOT:-/opt/subrouter/control}"
STATE_DIR="${SUBROUTER_STATE_DIR:-/var/lib/subrouter}"
DEFAULTS_FILE="${SUBROUTER_DEFAULTS_FILE:-/etc/default/subrouter}"
SLOT_UNIT="${SUBROUTER_SLOT_UNIT:-/etc/systemd/system/subrouter-slot@.service}"
FRONT_UNIT="${SUBROUTER_FRONT_UNIT:-/etc/systemd/system/subrouter-front.service}"
INSTALLER="${SUBROUTER_FRONT_INSTALLER:-/usr/local/libexec/subrouter-install-front-slots}"
LEGACY_SERVICE="${SUBROUTER_LEGACY_SERVICE:-subrouter.service}"

die() { printf 'verify-fresh-vm: %s\n' "$*" >&2; exit 1; }
[[ "$(id -u)" == 0 ]] || die "run as root"
[[ "${EXPECTED_SHA256}" =~ ^[0-9a-f]{64}$ ]] || die "expected checksum is invalid"
[[ "${RELEASE_TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || die "release tag is invalid"
for command in jq python3 sha256sum systemctl; do
  command -v "${command}" >/dev/null 2>&1 || die "${command} is required"
done

prepared_marker="${STATE_DIR}/front-topology-prepared"
active_marker="${prepared_marker}.active"
if [[ -f "${prepared_marker}" && ! -e "${active_marker}" ]]; then
  state=prepared
  marker="${prepared_marker}"
elif [[ -f "${active_marker}" && ! -e "${prepared_marker}" ]]; then
  state=active
  marker="${active_marker}"
else
  die "exactly one fresh-topology marker must exist"
fi
[[ "$(cat "${marker}")" == slot-a ]] || die "fresh topology must select slot-a"

for path in \
  "${RELEASE_ROOT}/${RELEASE_TAG}/subrouter" \
  "${SLOT_ROOT}/slot-a/worker" \
  "${FRONT_ROOT}/subrouter" \
  "${CONTROL_ROOT}/subrouter"; do
  [[ -f "${path}" ]] || die "topology binary is missing: ${path}"
  [[ "$(sha256sum "${path}" | awk '{print $1}')" == "${EXPECTED_SHA256}" ]] \
    || die "topology binary checksum mismatch: ${path}"
done
[[ -x "${INSTALLER}" && -f "${SLOT_UNIT}" && -f "${FRONT_UNIT}" ]] \
  || die "front/slot installer or units are missing"

is_active() {
  if systemctl is-active --quiet "$1"; then printf 'true\n'; else printf 'false\n'; fi
}
is_enabled() {
  if systemctl is-enabled --quiet "$1"; then printf 'true\n'; else printf 'false\n'; fi
}
legacy_active="$(is_active "${LEGACY_SERVICE}")"
legacy_socket_active="$(is_active "${LEGACY_SERVICE%.service}.socket")"
legacy_enabled="$(is_enabled "${LEGACY_SERVICE}")"
legacy_socket_enabled="$(is_enabled "${LEGACY_SERVICE%.service}.socket")"
slot_active="$(is_active subrouter-slot@slot-a.service)"
slot_enabled="$(is_enabled subrouter-slot@slot-a.service)"
front_active="$(is_active subrouter-front.service)"
front_enabled="$(is_enabled subrouter-front.service)"
[[ "${legacy_active}" == false && "${legacy_socket_active}" == false &&
    "${legacy_enabled}" == false && "${legacy_socket_enabled}" == false ]] \
  || die "legacy service or socket remains active or enabled"

authenticated=false
if [[ "${state}" == prepared ]]; then
  [[ "${slot_active}" == false && "${slot_enabled}" == false &&
      "${front_active}" == false && "${front_enabled}" == false ]] \
    || die "prepared topology started or enabled a serving unit"
else
  [[ "${slot_active}" == true && "${slot_enabled}" == true &&
      "${front_active}" == true && "${front_enabled}" == true ]] \
    || die "active topology is not enabled and running"
  [[ -f "${DEFAULTS_FILE}" ]] || die "authenticated defaults are missing"
  python3 - "${DEFAULTS_FILE}" <<'PY'
from pathlib import Path
import re
import sys

values = {}
for line in Path(sys.argv[1]).read_text().splitlines():
    match = re.fullmatch(r"(SUBROUTER_ADMIN_TOKEN|SUBROUTER_ACCOUNT_IMPORT_TOKEN)=(.*)", line)
    if match:
        value = match.group(2).strip()
        if len(value) >= 2 and value[0] == value[-1] == '"':
            value = value[1:-1]
        values[match.group(1)] = value
if not all(values.get(key) for key in ("SUBROUTER_ADMIN_TOKEN", "SUBROUTER_ACCOUNT_IMPORT_TOKEN")):
    raise SystemExit("control tokens are incomplete")
if values["SUBROUTER_ADMIN_TOKEN"] == values["SUBROUTER_ACCOUNT_IMPORT_TOKEN"]:
    raise SystemExit("control tokens are not distinct")
PY
  authenticated=true
fi

slot_memory="$(systemctl show subrouter-slot@slot-a.service -p MemoryMax --value)"
front_memory="$(systemctl show subrouter-front.service -p MemoryMax --value)"
[[ "${slot_memory}" == 201326592 && "${front_memory}" == 134217728 ]] \
  || die "front/slot memory limits are incorrect"

jq -nc --arg state "${state}" --arg tag "${RELEASE_TAG}" --arg sha "${EXPECTED_SHA256}" \
  --argjson authenticated "${authenticated}" --argjson slot_active "${slot_active}" \
  --argjson slot_enabled "${slot_enabled}" --argjson front_active "${front_active}" \
  --argjson front_enabled "${front_enabled}" \
  '{kind:"front-slots",state:$state,release_tag:$tag,initial_slot:"slot-a",
    authenticated:$authenticated,legacy:{service_active:false,service_enabled:false,
      socket_active:false,socket_enabled:false},
    slot:{id:"slot-a",service_active:$slot_active,service_enabled:$slot_enabled,
      worker_checksum:$sha,memory_max_bytes:201326592},
    front:{service_active:$front_active,service_enabled:$front_enabled,
      binary_checksum:$sha,memory_max_bytes:134217728},
    control:{binary_checksum:$sha},retained_release:{binary_checksum:$sha}}'
