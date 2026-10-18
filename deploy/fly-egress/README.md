# Codex regional egress on Fly.io

The team router on cmux-lawrence keeps every Codex credential and does every
token refresh. When the local pool answers a capacity failure
(`server_is_overloaded`, 408 or 5xx), `codexEgressFallbackTransport` replays
the same request, on the same account, through an HTTP CONNECT proxy in
another region. These Fly machines are those proxies. They hold no
credentials and store nothing but their Tailscale node state.

- App `subrouter-codex-egress` in the `manaflow` org. One `shared-cpu-1x`
  256 MB machine per Fly region, each with a 1 GB `ts_state` volume.
- Each machine joins the tailnet as `fly-egress-<region>` with `tag:egress`
  and runs tinyproxy on `<tailnet-ipv4>:3128`. No Fly services, so no public
  listener. CONNECT goes to port 443 of `chatgpt.com` and `*.openai.com` only.
- The existing ACL already grants `tag:dev-workstation` and `tag:server`
  access to `tag:egress:3128`. No ACL change is needed for new regions.
- On the router host, pf lets the `_subrouter` user reach the IPs in
  `/etc/pf.anchors/ai.manaflow.subrouter.egress` on port 3128 only
  (`deploy/macos/pf/`). Every other tailnet connection stays blocked.

## Operate

```bash
./fly-egress.sh build                 # image registry.fly.io/subrouter-codex-egress:v1
./fly-egress.sh up                    # every region; or: ./fly-egress.sh up ams fra
./fly-egress.sh status
./fly-egress.sh sync-router --dry-run
./fly-egress.sh sync-router           # pf table, then a hot `subrouter-deploy.sh reconfigure`
./fly-egress.sh destroy REGION
```

A new machine needs the `TS_AUTHKEY` app secret for its first join only.
Create a reusable, preauthorized `tag:egress` key with a one-hour expiry and
pipe it into the secret without printing it, then revoke the key and unset
the secret after the machines join:

```bash
tsadmin api POST /tailnet/-/keys '{"capabilities":{"devices":{"create":{"reusable":true,"ephemeral":false,"preauthorized":true,"tags":["tag:egress"]}}},"expirySeconds":3600,"description":"fly codex egress bootstrap"}' \
  | jq -r '"TS_AUTHKEY=" + .key' | fly secrets import -a subrouter-codex-egress --stage
```

A restart reuses the node state on the volume, so the node keeps its name and
tailnet IP after the key is revoked.

## Router tuning

`SUBROUTER_CODEX_EGRESS_MAX_ATTEMPTS` (default 4) caps how many regions one
request tries. A region that fails for capacity cools for one minute: new
requests try healthy regions first, then cooling ones.

## Region rotation and capacity tracking

The router keeps health per (model, region): `local` is its own uplink, the
rest are these egresses. A region that just answered a model with a capacity
failure cools for a minute. Every retry (same-account ladder, account
failover, transport replay) leaves from the next healthy region, websocket
dials go through the egress too, and a new session skips a cooling local
uplink. Responses carry `X-Subrouter-Region`.

Every capacity failure is a line in `codex-capacity-events.jsonl` next to the
session store (`attempt_failed` per region, then `absorbed`,
`client_visible` or `failed_other` per request or turn). Summary:

```bash
ssh cmux-lawrence 'curl -sS http://127.0.0.1:31415/_subrouter/codex-capacity' | jq '.events, .regions'
```

Live test: `live-regions.py` sends real turns from loopback with
`X-Subrouter-Test-Fault: capacity=<regions>`, which keeps those regions out of
the request (nothing is synthesized and nothing is sent from them). Enable
`SUBROUTER_CODEX_FAULT_INJECTION=1` with `subrouter-deploy.sh reconfigure`,
run it on cmux-lawrence, then reconfigure again without it.
