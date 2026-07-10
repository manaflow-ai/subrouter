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

The publish script configures the server with `sr server add`, then runs `sr server install`. The VM downloads the public release with the same curl installer and runs `sr install-systemd`; no locally built binary is copied to the server. If legacy `switchboard` or `gateway` services exist on the VM, the systemd installer disables them and migrates their state into `/var/lib/subrouter`.

Join or rejoin the host to Tailscale with an auth key:

```bash
export TAILSCALE_AUTH_KEY=<tailscale-auth-key>
deploy/gcp/publish-subrouter.sh
```

The publish script joins with `--accept-routes=false --accept-dns=false` so the VM does not use tailnet routes or tailnet DNS for its own outbound traffic.
The VM also installs a host firewall rule that rejects new outbound connections from `tailscale0` to tailnet IP ranges while still allowing replies to inbound requests.

Add a server-owned Codex OAuth account when the VM should route real Codex traffic:

```bash
sr server login team --device-auth
```

OAuth refresh tokens rotate on use, so do not copy an existing OAuth refresh-token file to the server. `sr server login` performs a fresh Codex login, uploads only that fresh account to `/var/lib/subrouter/codex/accounts`, asks the live Subrouter process to reload accounts in place, then restores your previous local auth so only the server owns the new refresh-token chain. Existing proxy and WebSocket connections keep running.

To compare local OAuth emails with the server and reauth every missing local email on the server, run:

```bash
sr server sync team --device-auth
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

For team API clients, regenerate the current defaults, generate a separate admin token, then use a protected editor to set `SUBROUTER_ADMIN_TOKEN` and the six `SUBROUTER_{ANTHROPIC,OPENAI,GEMINI}_{API_KEY,GATEWAY_TOKEN}` values. Paste the generated admin token into the defaults file. Do not put secrets in command arguments or shell history.

```bash
sudo sr install-systemd --start=false
sudo chmod 600 /etc/default/subrouter
openssl rand -hex 32
sudoedit /etc/default/subrouter
sudo systemctl restart subrouter
```

Anthropic SDK clients use `http://subrouter-team:31415/anthropic`, OpenAI SDK clients use `http://subrouter-team:31415/api/v1`, and default `v1beta` Gemini clients use `http://subrouter-team:31415`. Gemini clients selecting `v1` or `v1alpha` use `http://subrouter-team:31415/gemini`. Provider keys remain on the VM.

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
