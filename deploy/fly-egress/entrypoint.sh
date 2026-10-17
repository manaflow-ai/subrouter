#!/bin/sh
# Join the tailnet as fly-egress-<region> with tag:egress, then serve
# CONNECT on <tailnet-ipv4>:3128 for the subrouter only.
#
# State lives on the ts_state volume, so a restart keeps the same node, name
# and tailnet IP (the subrouter host's pf table pins those IPs). TS_AUTHKEY is
# needed only for the first join; it is a short-lived one-off key.
set -eu

STATE_DIR=/var/lib/tailscale
HOSTNAME="fly-egress-${FLY_REGION:?FLY_REGION unset}"
mkdir -p "$STATE_DIR" /var/run/tailscale
# Move the bootstrap key into a file before any child inherits the env.
AUTHKEY_FILE=""
if [ -n "${TS_AUTHKEY:-}" ]; then
  umask 077
  printf '%s' "$TS_AUTHKEY" >/run/ts-authkey
  AUTHKEY_FILE=/run/ts-authkey
  umask 022
fi
unset TS_AUTHKEY

tailscaled --state="$STATE_DIR/tailscaled.state" \
  --socket=/var/run/tailscale/tailscaled.sock --tun=tailscale0 &
TAILSCALED=$!

i=0
until tailscale status --json >/dev/null 2>&1; do
  i=$((i + 1)); [ "$i" -gt 100 ] && { echo "tailscaled did not start" >&2; exit 1; }
  sleep 0.1
done

if [ -n "$AUTHKEY_FILE" ]; then
  tailscale up --auth-key="file:$AUTHKEY_FILE" --hostname="$HOSTNAME" \
    --advertise-tags=tag:egress --accept-dns=false --accept-routes=false
  rm -f "$AUTHKEY_FILE"
else
  tailscale up --hostname="$HOSTNAME" --advertise-tags=tag:egress \
    --accept-dns=false --accept-routes=false
fi

TS_IP="$(tailscale ip -4)"
cat >/etc/tinyproxy/tinyproxy.conf <<CONF
User tinyproxy
Group tinyproxy
Listen $TS_IP
Port 3128
Timeout 3600
MaxClients 512
LogLevel Warning
Allow 100.64.0.0/10
ConnectPort 443
DisableViaHeader Yes
FilterType ERE
FilterDefaultDeny Yes
Filter "/etc/tinyproxy/allow"
FilterURLs No
CONF
# CONNECT targets the proxy may reach: the Codex backend only.
cat >/etc/tinyproxy/allow <<'ALLOW'
^chatgpt\.com$
^(.+\.)?openai\.com$
ALLOW

echo "fly-egress: $HOSTNAME serving CONNECT on $TS_IP:3128"
tinyproxy -d -c /etc/tinyproxy/tinyproxy.conf &
PROXY=$!

trap 'kill "$PROXY" "$TAILSCALED" 2>/dev/null' TERM INT
# Exit (and let Fly restart the machine) when either process dies.
while kill -0 "$PROXY" 2>/dev/null && kill -0 "$TAILSCALED" 2>/dev/null; do
  sleep 5
done
echo "fly-egress: a child exited" >&2
exit 1
