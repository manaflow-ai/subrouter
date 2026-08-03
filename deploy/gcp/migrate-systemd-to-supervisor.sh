#!/usr/bin/env bash
# One-time migration of the Linux/GCP Subrouter service to supervised mode.
#
# Why: systemd socket activation keeps the *listener* alive across a restart, so
# new connections are never refused. It does not keep established connections
# alive, because those file descriptors belong to the worker process being
# replaced. A client mid-stream (SSE response, WebSocket, long POST) sees the
# connection close. For agent traffic that is a cancelled turn.
#
# Supervised mode fixes that: the supervisor owns the public listener, starts
# each worker on an inherited private socket, and pins accepted connections to a
# worker generation. An upgrade health-checks the replacement before routing new
# connections to it and lets the old worker finish its existing ones.
#
# This migration itself cannot preserve connections already accepted by the
# current unsupervised process, since that process owns their descriptors. Run
# it in a maintenance window. Every later worker upgrade preserves connections.
set -euo pipefail

SERVICE="${SUBROUTER_SERVICE:-subrouter}"
UNIT="/etc/systemd/system/${SERVICE}.service"
WORKER_BIN="${SUBROUTER_BIN:-/usr/local/bin/subrouter}"
SUPERVISOR_BIN="${SUBROUTER_SUPERVISOR_BIN:-/usr/local/libexec/subrouter-supervisor}"
STATE_DIR="${SUBROUTER_STATE_DIR:-/var/lib/subrouter}"
CONTROL_SOCKET="${SUBROUTER_CONTROL_SOCKET:-${STATE_DIR}/supervisor.sock}"

activate=0
[ "${1:-}" = "--activate" ] && activate=1

die() { echo "migrate-systemd-to-supervisor: $*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || die "run as root"
[ -f "$UNIT" ] || die "$UNIT not found"
[ -x "$WORKER_BIN" ] || die "$WORKER_BIN is not executable"
"$WORKER_BIN" help 2>/dev/null | grep -q ' supervise ' \
  || die "$WORKER_BIN does not support supervise; upgrade it first"

# The supervisor is deliberately a separate copy: routine releases replace the
# worker at $WORKER_BIN and must never replace the running supervisor.
install -d -m 0755 "$(dirname "$SUPERVISOR_BIN")"
install -m 0755 "$WORKER_BIN" "${SUPERVISOR_BIN}.new"
mv -f "${SUPERVISOR_BIN}.new" "$SUPERVISOR_BIN"

# Reuse the existing worker arguments, minus --addr, which the supervisor owns.
service_user="$(systemctl show "$SERVICE" -p User --value)"
service_group="$(systemctl show "$SERVICE" -p Group --value)"
service_home="$(systemctl show "$SERVICE" -p Environment --value \
  | tr ' ' '\n' | sed -n 's/^HOME=//p' | head -1)"
[ -n "$service_user" ] || die "could not read User= from $SERVICE"
[ -n "$service_group" ] || service_group="$service_user"
[ -n "$service_home" ] || service_home="$STATE_DIR"
install -d -m 0750 -o "$service_user" -g "$service_group" \
  "$service_home" "$STATE_DIR" /var/log/subrouter
current_exec="$(systemctl show "$SERVICE" -p ExecStart --value)"
worker_args="$(printf '%s\n' "$current_exec" \
  | sed -n 's/.*argv\[\]=\([^;]*\).*/\1/p' \
  | sed "s|^${WORKER_BIN} *||; s|^[^ ]*subrouter *||")"
public_addr="$(printf '%s\n' "$worker_args" | sed -n 's/.*--addr[= ]\([^ ]*\).*/\1/p')"
[ -n "$public_addr" ] || public_addr="\${SUBROUTER_ADDR}"
# The supervisor supplies the `serve` subcommand and the worker's private
# --addr itself, so pass neither through.
filtered_args="$(printf '%s\n' "$worker_args" \
  | sed 's/--addr[= ][^ ]*//' \
  | sed 's/^ *serve  *//' \
  | sed 's/  */ /g; s/^ *//; s/ *$//')"

prepared="${UNIT}.supervised"
cat > "$prepared" <<UNITFILE
[Unit]
Description=Subrouter (supervised)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${service_user}
Group=${service_group}
# The worker reads its config from HOME; without this it exits with
# "load cmux.com config: \$HOME is not defined" before readiness.
Environment=HOME=${service_home}
EnvironmentFile=-/etc/default/${SERVICE}
EnvironmentFile=-/etc/subrouter-fable.env
ExecStart=${SUPERVISOR_BIN} supervise \\
  --addr ${public_addr} \\
  --control-socket ${CONTROL_SOCKET} \\
  --worker-bin ${WORKER_BIN} \\
  -- ${filtered_args}
Restart=always
RestartSec=2
# The supervisor must outlive its draining workers, so allow a long stop.
TimeoutStopSec=15min
WorkingDirectory=${service_home}
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=read-only
ReadWritePaths=${service_home} ${STATE_DIR} /var/log/subrouter

[Install]
WantedBy=multi-user.target
UNITFILE

echo "prepared $prepared"
if [ "$activate" -eq 0 ]; then
  cat <<EOF

Review the unit above, then activate with:
  sudo $0 --activate

Activation stops the socket-activated service and hands the listener to the
supervisor. That single step drops connections currently in flight; every
upgrade after it does not.
EOF
  exit 0
fi

backup="${UNIT}.backup-$(date +%Y%m%d-%H%M%S)"
cp -a "$UNIT" "$backup"

rollback() {
  echo "rolling back to the unsupervised service" >&2
  cp -a "$backup" "$UNIT"
  systemctl daemon-reload
  # The unsupervised unit has Requires=<service>.socket, so it cannot start
  # until the socket is enabled and started again.
  systemctl enable "${SERVICE}.socket" 2>/dev/null || true
  systemctl start "${SERVICE}.socket" 2>/dev/null || true
  systemctl reset-failed "${SERVICE}.service" 2>/dev/null || true
  systemctl start "$SERVICE" || true
}

# Socket activation and the supervisor cannot both own :31415.
systemctl stop "${SERVICE}.socket" 2>/dev/null || true
systemctl disable "${SERVICE}.socket" 2>/dev/null || true
systemctl stop "$SERVICE" || true

cp -a "$prepared" "$UNIT"
systemctl daemon-reload

if ! systemctl start "$SERVICE"; then
  rollback
  die "supervised service failed to start"
fi

# $public_addr may be a literal systemd variable reference, so resolve the port
# from the environment file rather than parsing it.
port="$(sed -n 's/^SUBROUTER_ADDR=.*:\([0-9]*\).*/\1/p' "/etc/default/${SERVICE}" 2>/dev/null | head -1)"
case "$public_addr" in
  *:[0-9]*) port="${public_addr##*:}" ;;
esac
[ -n "$port" ] || port=31415
health_url="${SUBROUTER_HEALTH_URL:-http://127.0.0.1:${port}/_subrouter/health}"
for _ in $(seq 1 30); do
  if curl -fsS --max-time 2 "$health_url" >/dev/null 2>&1; then
    echo "supervised Subrouter healthy at $health_url"
    echo "control socket: $CONTROL_SOCKET"
    echo "backup: $backup"
    exit 0
  fi
  sleep 1
done

rollback
die "supervised service never became healthy"
