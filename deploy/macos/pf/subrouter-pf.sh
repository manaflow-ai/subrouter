#!/bin/sh
# Load the Subrouter pf anchor on a shared macOS host.
#
# Enables pf and evaluates the "ai.manaflow.subrouter" anchor by reloading the
# system ruleset (/etc/pf.conf) with an appended anchor reference, so we never
# permanently edit /etc/pf.conf (which macOS updates reset). Best-effort: a pf
# failure must never take down the host, so this always exits 0.
set -u

ANCHOR="ai.manaflow.subrouter"
ANCHOR_FILE="/etc/pf.anchors/${ANCHOR}"
EGRESS_FILE="${ANCHOR_FILE}.egress"

log() { echo "subrouter-pf: $*"; }

if [ ! -f "${ANCHOR_FILE}" ]; then
  log "anchor file missing: ${ANCHOR_FILE} (nothing to load)"
  exit 0
fi

# The anchor's egress table reads this file. A missing file fails the whole
# ruleset load below, which would leave the router port open to the LAN, so
# an absent list means no egress, never no pf.
[ -f "${EGRESS_FILE}" ] || install -m 644 -o root -g wheel /dev/null "${EGRESS_FILE}" 2>/dev/null || true

# Enable pf if it is not already on. -E bumps a reference count and is safe to
# call when pf is already enabled by something else.
pfctl -E >/dev/null 2>&1 || pfctl -e >/dev/null 2>&1 || true

tmp="$(mktemp /tmp/subrouter-pf.XXXXXX)" || { log "mktemp failed"; exit 0; }
# Preserve the system ruleset (Apple anchors, any local includes) and append
# our filter anchor + its rules.
if [ -f /etc/pf.conf ]; then
  cat /etc/pf.conf >"${tmp}" 2>/dev/null || true
fi
printf '\nanchor "%s"\nload anchor "%s" from "%s"\n' \
  "${ANCHOR}" "${ANCHOR}" "${ANCHOR_FILE}" >>"${tmp}"

if pfctl -f "${tmp}" >/dev/null 2>&1; then
  log "loaded anchor ${ANCHOR}"
else
  log "pfctl reload failed (non-fatal); leaving existing ruleset in place"
fi
rm -f "${tmp}"
exit 0
