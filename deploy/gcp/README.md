# GCP Subrouter Deployment

This deploys Subrouter as `subrouter` on a small Debian VM and exposes the service over Tailscale.

Defaults:

- Instance: `subrouter-team`
- Local server name: `team`
- Zone: `us-central1-a`
- Machine: `e2-micro`
- Disk: 10 GB standard persistent disk
- Service port: `31415`

The scripts do not open port `31415` publicly. Use Tailscale for teammate access.
They also add a target-tagged deny rule for public ingress, with only a source-limited bootstrap SSH allow rule above it.

## Local setup

Install and authenticate the Google Cloud CLI:

```bash
gcloud auth login
gcloud config set project <project-id>
```

Install `sr` locally:

```bash
curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sh
```

Create the VM:

```bash
deploy/gcp/create-subrouter-vm.sh
```

Install or upgrade Subrouter on the VM:

```bash
deploy/gcp/publish-subrouter.sh
```

The publish script configures the server with `sr server add`, then runs `sr server install`. The installer generates a private account-import token, sends it to the systemd installer over standard input, and stores it locally without printing it or placing it in process arguments. The VM downloads the public release; no locally built binary or account credential is copied to the server. If legacy `switchboard` or `gateway` services exist on the VM, the systemd installer disables them and migrates their state into `/var/lib/subrouter`.

Join or rejoin the host to Tailscale with an auth key:

```bash
export TAILSCALE_AUTH_KEY=<tailscale-auth-key>
deploy/gcp/publish-subrouter.sh
```

The publish script joins with `--accept-routes=false --accept-dns=false` so the VM does not use tailnet routes or tailnet DNS for its own outbound traffic.
The VM also installs a host firewall rule that rejects new outbound connections from `tailscale0` to tailnet IP ranges while still allowing replies to inbound requests.

Add a server-owned Codex OAuth account when the VM should route real Codex traffic:

```bash
sr server login team
```

OAuth refresh tokens rotate on use, so do not copy an existing OAuth refresh-token file to the server. `sr server login` performs a fresh Codex login, checks the protected import endpoint with `GET`, then sends the credential with authenticated `POST`. The server validates the OAuth identity and token freshness, stores the account atomically, and reloads it in place. SSH, SCP, and gcloud are never used for credential transfer. Existing HTTP and WebSocket proxy connections keep running. Use `--device-auth` only from a headless or remote shell that cannot receive the localhost browser callback.

To compare local OAuth emails with the server and reauth every missing local email on the server, run:

```bash
sr server sync team
```

This validates the server refresh-token chains, shows missing or invalid accounts, asks for confirmation, then walks through one fresh login per selected email. Use `sr server diff team` to inspect the diff without logging in, `--email you@example.com` for a single account, `--all` to replace every server copy, or `--yes` to skip the confirmation prompt. The status check may refresh valid server-owned OAuth chains in place because Codex refresh tokens rotate.

The old account-file upload helper is kept only as a compatibility wrapper:

```bash
deploy/gcp/upload-codex-accounts.sh team --device-auth
```

It delegates to `sr server login` and rejects the previous `--move` and `--copy-unsafe` paths.

## Client usage

Use the Tailscale IP or MagicDNS name:

```bash
export SUBROUTER_CODEX_BASE_URL=http://<tailscale-ip>:31415/v1
export SUBROUTER_CODEX_USER_EMAIL=alice@example.com
subrouter codex
```

Or select the named server as your default Codex route:

```bash
sr server use team
sr codex
```

Traffic attribution is self-reported with `X-Subrouter-User-Email`, or through `SUBROUTER_CODEX_USER_EMAIL` when using `subrouter codex`.

Health check:

```bash
curl http://<tailscale-ip>:31415/_subrouter/health
```

Sessions:

```bash
curl http://<tailscale-ip>:31415/_subrouter/sessions
```

Trajectory dashboard:

```bash
open http://<tailscale-ip>:31415/_subrouter/dashboard
curl http://<tailscale-ip>:31415/_subrouter/transcripts
```

The dashboard reads transcript JSONL files from `/var/lib/subrouter/transcripts`
on the VM. It shows token usage over time, usage by user email, usage by selected
account, sanitized detail JSON, and raw internal trajectory JSON with decoded
body text under `/raw`.

Transcript recording is off by default. To enable it on a shared VM, configure both a local spool and GCS mirror with local cleanup:

```bash
sudo sed -i 's|^SUBROUTER_TRANSCRIPTS=.*|SUBROUTER_TRANSCRIPTS=/var/lib/subrouter/transcripts|' /etc/default/subrouter
sudo sed -i 's|^SUBROUTER_TRANSCRIPT_ARGS=.*|SUBROUTER_TRANSCRIPT_ARGS="--transcripts=/var/lib/subrouter/transcripts"|' /etc/default/subrouter
sudo sed -i 's|^SUBROUTER_EXTRA_ARGS=.*|SUBROUTER_EXTRA_ARGS="--transcript-gcs-uri=gs://<bucket>/<prefix> --transcript-gcs-sync-interval=5m --transcript-gcs-sync-timeout=30m --transcript-local-retention=24h --transcript-max-local-bytes=2GiB"|' /etc/default/subrouter
sudo install -d -o subrouter -g subrouter -m 0750 /var/lib/subrouter/transcripts
sudo systemctl restart subrouter
```

The mirror runs inside the Subrouter daemon through the GCS JSON API.
Upload failures are logged and retried; request proxying never waits for GCS. Local cleanup only runs after a successful GCS sync and archives each pruned file under `_archive/` first.

## Claude rate-limit reroute self-verifier

`subrouter-verify.{sh,service,timer}` is a durable self-check for the Claude
rate-limit reroute (a depleted account answers 200 via overage with
`anthropic-ratelimit-unified-status: rejected`; subrouter must reroute to a
healthy account). The systemd timer runs every 5 minutes and, all from the VM:

- asserts the passive invariant (a rate-limit reaching the client after failover
  gave up while healthy accounts existed is an ALERT),
- watches for contract drift (Claude HTTP 429s reappearing, or any unknown
  `anthropic-ratelimit-unified-status` value),
- checks service health/version, and
- runs an hourly canary that pins one tiny request to a cooked account and
  asserts the client is not `rejected`.

`startup.sh` installs and enables it on provision, so VM rebuilds keep it.
Verdicts go to `journalctl -u subrouter-verify` and
`/var/lib/subrouter-verify/{status.json,alerts.log}`. Watch alerts with:

```bash
journalctl -u subrouter-verify -f | grep '\[ALERT\]'
```
