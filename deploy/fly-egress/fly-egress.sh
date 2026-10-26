#!/usr/bin/env bash
# Provision and inspect the Codex regional egress nodes on Fly.io.
#
# One Fly app, one shared-cpu-1x/256 MB machine per region, each with a 1 GB
# volume for Tailscale state. Every machine joins the tailnet as
# fly-egress-<region> with tag:egress and serves CONNECT on <tailnet-ip>:3128.
# Credentials and refresh stay on the subrouter host; these nodes only carry
# the replayed request.
#
#   fly-egress.sh build                     build and push the image
#   fly-egress.sh up REGION...              create missing machines (needs the
#                                           TS_AUTHKEY app secret for new ones)
#   fly-egress.sh update [REGION...]        move existing machines to $IMAGE
#   fly-egress.sh status                    machines and tailnet nodes
#   fly-egress.sh proxies                   SUBROUTER_CODEX_EGRESS_PROXIES value
#                                           for every online fly-egress node
#   fly-egress.sh pf-hosts                  tailnet IPv4 of every tag:egress node
#   fly-egress.sh sync-router [--dry-run]   push the pf table and the worker's
#                                           SUBROUTER_CODEX_EGRESS_PROXIES to
#                                           the router host (hot reconfigure)
#   fly-egress.sh destroy REGION            remove one region's machine + volume
set -euo pipefail

APP="${FLY_EGRESS_APP:-}"
ORG="${FLY_EGRESS_ORG:-manaflow}"
IMAGE_TAG="${FLY_EGRESS_IMAGE_TAG:-v1}"
IMAGE="registry.fly.io/${APP}:${IMAGE_TAG}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROUTER_HOST="${FLY_EGRESS_ROUTER_HOST:-}"
PF_TABLE_FILE=/etc/pf.anchors/ai.manaflow.subrouter.egress
ALL_REGIONS="ams arn cdg dfw ewr fra gru iad jnb lax lhr nrt ord sin sjc syd yyz"

die() { echo "fly-egress: $*" >&2; exit 1; }

ensure_app() {
  fly apps list -o "$ORG" --json | jq -e --arg a "$APP" 'any(.[]; .Name == $a)' >/dev/null \
    || fly apps create "$APP" -o "$ORG" >/dev/null
}

machine_in_region() {
  fly machines list -a "$APP" --json | jq -r --arg r "$1" '[.[] | select(.region == $r)][0].id // empty'
}

cmd_build() {
  ensure_app
  fly deploy "$HERE" -a "$APP" --build-only --push --image-label "$IMAGE_TAG" \
    --config /dev/null --dockerfile "$HERE/Dockerfile"
}

cmd_up() {
  [ "$#" -gt 0 ] || set -- $ALL_REGIONS
  ensure_app
  for region in "$@"; do
    if [ -n "$(machine_in_region "$region")" ]; then
      echo "fly-egress: $region already has a machine"; continue
    fi
    volume="$(fly volumes list -a "$APP" --json | jq -r --arg r "$region" \
      '[.[] | select(.region == $r and .name == "ts_state" and (.attached_machine_id // "") == "")][0].id // empty')"
    if [ -z "$volume" ]; then
      volume="$(fly volumes create ts_state -a "$APP" -r "$region" -s 1 -y --json | jq -r .id)"
    fi
    fly machine run "$IMAGE" -a "$APP" -r "$region" --name "egress-$region" \
      --vm-size shared-cpu-1x --vm-memory 256 --restart always \
      -v "$volume:/var/lib/tailscale" --detach >/dev/null
    echo "fly-egress: started $region"
  done
}

# tag:egress nodes as JSON lines {name, ip, online}. Reads the tailnet through
# tsadmin so the caller needs no Tailscale API token of its own.
egress_nodes() {
  tsadmin devices | jq -c '.devices[] | select((.tags // []) | index("tag:egress"))
    | {name: (.name | split(".")[0]), ip: ([.addresses[] | select(test("^100\\."))][0] // null),
       seen: .lastSeen, connected: (.connectedToControl // false)}'
}

cmd_status() {
  fly machines list -a "$APP" --json | jq -r '.[] | [.region, .id, .state, .config.guest.cpu_kind, .config.guest.memory_mb] | @tsv'
  echo "--- tailnet tag:egress"
  egress_nodes
}

cmd_proxies() {
  egress_nodes | jq -rs '[.[] | select(.name | startswith("fly-egress-")) | select(.connected)
    | "http://\(.name):3128"] | sort | join(",")'
}

cmd_pf_hosts() {
  egress_nodes | jq -r '.ip // empty' | sort -t. -k1,1n -k2,2n -k3,3n -k4,4n
}

# Every online tag:egress node, GCP and Fly, as the worker's proxy list.
# Session pins are stored as list indexes, so the order already on the router
# is kept and new nodes are appended.
all_proxies() {
  local current="$1"
  egress_nodes | jq -rs --arg current "$current" '
    ($current | split(",") | map(select(. != ""))) as $have
    | ([.[] | select(.connected) | "http://\(.name):3128"] | sort) as $want
    | ([$have[] | select(. as $p | $want | index($p))] + [$want[] | select(. as $p | $have | index($p) | not)])
    | join(",")'
}

cmd_sync_router() {
  [ -n "$ROUTER_HOST" ] || die "set FLY_EGRESS_ROUTER_HOST to the router host"
  local dry=0
  [ "${1:-}" = "--dry-run" ] && dry=1
  local hosts proxies current
  ssh "$ROUTER_HOST" "sudo pfctl -a ai.manaflow.subrouter -sr 2>/dev/null" | grep -q '<subrouter_egress>' \
    || die "the live pf anchor on $ROUTER_HOST does not use <subrouter_egress>; install deploy/macos/pf first"
  current="$(ssh "$ROUTER_HOST" "sudo python3 -c 'import json; print(json.load(open(\"/var/lib/subrouter/worker-config.json\")).get(\"env\", {}).get(\"SUBROUTER_CODEX_EGRESS_PROXIES\", \"\"))'")"
  hosts="$(cmd_pf_hosts)"
  proxies="$(all_proxies "$current")"
  [ -n "$hosts" ] && [ -n "$proxies" ] || die "no tag:egress nodes found"
  echo "pf table:"; echo "$hosts" | sed 's/^/  /'
  echo "SUBROUTER_CODEX_EGRESS_PROXIES=$proxies"
  [ "$dry" -eq 1 ] && return 0
  # pf first: a proxy the worker cannot reach only wastes an attempt, but a
  # reachable proxy the worker does not list is harmless.
  printf '%s\n' "$hosts" | ssh "$ROUTER_HOST" "sudo sh -c 'umask 022; cat >$PF_TABLE_FILE.new && mv $PF_TABLE_FILE.new $PF_TABLE_FILE' \
    && sudo pfctl -a ai.manaflow.subrouter -t subrouter_egress -T replace -f $PF_TABLE_FILE"
  if [ "$proxies" = "$current" ]; then
    echo "fly-egress: worker config already lists these proxies"; return 0
  fi
  # The candidate is written by root into a fresh root-only directory, never a
  # shared /tmp path another local user could pre-create.
  ssh "$ROUTER_HOST" "dir=\$(sudo mktemp -d /var/lib/subrouter/.fly-egress.XXXXXX) || exit 1
sudo env PROXIES='$proxies' OUT=\"\$dir/worker-config.json\" python3 - <<'PY' || { sudo rm -rf \"\$dir\"; exit 1; }
import json, os
doc = json.load(open('/var/lib/subrouter/worker-config.json'))
doc.setdefault('env', {})['SUBROUTER_CODEX_EGRESS_PROXIES'] = os.environ['PROXIES']
fd = os.open(os.environ['OUT'], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
with os.fdopen(fd, 'w') as out:
    json.dump(doc, out, indent=2)
PY
sudo /usr/local/bin/subrouter-deploy.sh reconfigure \"\$dir/worker-config.json\"; rc=\$?
sudo rm -rf \"\$dir\"; exit \$rc"
}

cmd_update() {
  [ "$#" -gt 0 ] || set -- $ALL_REGIONS
  for region in "$@"; do
    id="$(machine_in_region "$region")"
    [ -n "$id" ] || { echo "fly-egress: no machine in $region"; continue; }
    fly machine update "$id" -a "$APP" --image "$IMAGE" --yes >/dev/null
    echo "fly-egress: $region now runs $IMAGE"
  done
}

cmd_destroy() {
  region="${1:?region}"
  id="$(machine_in_region "$region")"
  [ -n "$id" ] || die "no machine in $region"
  fly machine destroy "$id" -a "$APP" --force
  fly volumes list -a "$APP" --json | jq -r --arg r "$region" '.[] | select(.region == $r) | .id' \
    | while read -r v; do fly volumes destroy "$v" -a "$APP" -y; done
}

case "${1:-}" in
  build|up|update|status|proxies|pf-hosts|sync-router|destroy)
    [ -n "$APP" ] || die "set FLY_EGRESS_APP to the Fly app name" ;;
esac

case "${1:-}" in
  build) shift; cmd_build "$@" ;;
  up) shift; cmd_up "$@" ;;
  update) shift; cmd_update "$@" ;;
  status) shift; cmd_status "$@" ;;
  proxies) shift; cmd_proxies "$@" ;;
  pf-hosts) shift; cmd_pf_hosts "$@" ;;
  sync-router) shift; cmd_sync_router "$@" ;;
  destroy) shift; cmd_destroy "$@" ;;
  *) sed -n '2,20p' "$0"; exit 2 ;;
esac
