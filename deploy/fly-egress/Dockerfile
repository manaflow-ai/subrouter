# Codex regional egress node: Tailscale (tag:egress) plus a CONNECT-only
# tinyproxy that listens on the node's tailnet address and nothing else.
# The machine has no Fly services, so it has no public listener.
FROM alpine:3.22
RUN apk add --no-cache tailscale tinyproxy iptables ip6tables ca-certificates
COPY entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
