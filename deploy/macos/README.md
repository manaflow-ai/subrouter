# macOS Subrouter Deployment

Runs the shared "team" Subrouter on a macOS host (a Mac mini) with full parity
to the GCP deployment in `../gcp`. Use this when you want the router on an
office Mac instead of a cloud VM.

Defaults:

- Label: `ai.manaflow.subrouter-team` (system LaunchDaemon)
- Service user: `_subrouter` (dedicated, hidden role account)
- Listen: `0.0.0.0:31415`, restricted to the tailnet + loopback by pf
- State: `/var/lib/subrouter` (mirrors the GCP `/var/lib/subrouter` layout)
- Binary: `/usr/local/bin/subrouter` (public release, auto-updated)

## Why a dedicated user + LaunchDaemon (not `sr install-daemon`)

`sr install-daemon` installs a per-user LaunchAgent bound to loopback, which is
right for a personal `sr`/`cx` daemon but wrong for a shared server: it dies on
logout, only serves loopback, and (running as your login user) can't be
egress-isolated with pf on a machine that runs other things. The team router
instead runs as its own `_subrouter` user under a system LaunchDaemon, which is
what makes the pf `user`-scoped egress block possible without touching other
workloads on the host (e.g. a co-hosted cmux-feed).

## Parity with GCP

| Concern | GCP (systemd) | macOS (launchd) |
| --- | --- | --- |
| Service | `subrouter.service` + `subrouter.socket` | `ai.manaflow.subrouter-team` LaunchDaemon |
| Auto-update (2 min) | `subrouter-autoupdate.timer` | `ai.manaflow.subrouter-autoupdate` (`StartInterval`) |
| Self-verify (5 min) | `subrouter-verify.timer` | `ai.manaflow.subrouter-verify` (`StartInterval`) |
| Egress isolation | iptables block to tailnet on `tailscale0` | pf anchor scoped to `user _subrouter` |
| Zero-downtime restart | systemd socket handoff | none — a restart drops in-flight requests briefly |

The one thing macOS can't match is systemd socket activation, so auto-update
restarts cause a short blip. `subrouter` drains on SIGTERM and
`launchctl kickstart -k` gives a clean stop/start.

pf note: Tailscale MagicDNS (`100.100.100.100`) is inside `100.64.0.0/10`, so the
anchor explicitly allows `_subrouter -> 100.100.100.100:53`; without it the
router loses DNS and can't reach the upstream APIs.

## Install

```bash
# from a checkout on the host, or scp just this directory:
sudo ./install-macos.sh
```

Override the bind or version with env, e.g. `SUBROUTER_ADDR=0.0.0.0:31415`,
`SUBROUTER_VERSION=v0.1.33`. The installer creates the user, installs the
binary + scripts + pf anchor, writes the LaunchDaemons, and waits for health.
It does **not** install account state.

Verify:

```bash
curl -fsS http://127.0.0.1:31415/_subrouter/health         # {"ok":true}
sudo launchctl print system/ai.manaflow.subrouter-team | grep state
sudo pfctl -a ai.manaflow.subrouter -sr                     # anchor rules
```

## Migrating account state from the GCP router

Account OAuth refresh tokens **rotate on use**, so only one live router may own
them at a time. Migrate during a short window, do not run both against the same
accounts:

```bash
# 1. stop the GCP router so its token chains freeze
gcloud compute ssh subrouter-team --zone <zone> --command 'sudo systemctl stop subrouter'

# 2. archive + copy /var/lib/subrouter to the mac
gcloud compute ssh subrouter-team --zone <zone> --command 'sudo tar -C /var/lib -czf /tmp/sr.tgz subrouter'
gcloud compute scp subrouter-team:/tmp/sr.tgz /tmp/sr.tgz --zone <zone>
scp /tmp/sr.tgz <mac>:/tmp/sr.tgz

# 3. on the mac: extract into place, fix ownership, reload accounts
ssh <mac> 'sudo tar -C /var/lib -xzf /tmp/sr.tgz \
  && sudo chown -R _subrouter:_subrouter /var/lib/subrouter \
  && sudo launchctl kickstart -k system/ai.manaflow.subrouter-team \
  && curl -fsS -X POST http://127.0.0.1:31415/_subrouter/reload-accounts'

# 4. confirm all accounts are present and the verifier is clean
ssh <mac> 'curl -fsS http://127.0.0.1:31415/_subrouter/usage-status | python3 -c "import sys,json;d=json.load(sys.stdin);print(len(d if isinstance(d,list) else d[\"accounts\"]))"'
```

Then repoint clients' `team` server URL at the mac and keep the GCP VM stopped
(not deleted) as instant rollback. To roll back, reverse step 2-3 (mac -> GCP)
and restart `subrouter` on the VM.

## Bedrock (Claude Fable)

To serve Claude Fable from AWS Bedrock, follow [BEDROCK.md](./BEDROCK.md). In
short, pass a SigV4 IAM key and `SUBROUTER_ENABLE_BEDROCK=1` (optionally
`SUBROUTER_FABLE_BEDROCK_PRIMARY=1`) to `install-macos.sh`.

## Uninstall

```bash
for l in verify autoupdate team pf; do sudo launchctl bootout system/ai.manaflow.subrouter-$l 2>/dev/null; done
sudo rm -f /Library/LaunchDaemons/ai.manaflow.subrouter-*.plist
sudo pfctl -a ai.manaflow.subrouter -F rules
# state in /var/lib/subrouter and the _subrouter user are left in place on purpose
```
