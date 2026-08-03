# GCP Subrouter Deployment

This deploys Subrouter as `subrouter` on a small Debian VM behind the Google Cloud HTTPS load balancer.

Defaults:

- Instance: `subrouter-team`
- Local server name: `team`
- Zone: `us-south1-a`
- Machine: `e2-micro`
- Disk: 10 GB standard persistent disk
- Legacy service port: `31415`
- Stable front port: `31416`
- Private supervisor slots: `127.0.0.1:31417` and `127.0.0.1:31418`

End users connect to `https://sr.cmux.com`. The firewall accepts ports `31415`
and `31416` only from Google load-balancer ranges and accepts SSH only from Google IAP.
Operator deployment uses IAP. Account login and proxy traffic use HTTPS.

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

Choose an existing release tag and create the VM with that exact release:

```bash
SUBROUTER_RELEASE_TAG=v0.1.52 deploy/gcp/create-subrouter-vm.sh
```

Bootstrap or upgrade the legacy listener before the front migration:

```bash
deploy/gcp/publish-subrouter.sh v0.1.52
```

After `migrate-front`, this command fails closed. Use the protected `slot`
workflow operation for every release change.

GCP deployment is manual and release-pinned. Dispatch `.github/workflows/gcp-deploy.yml`
with an environment, an existing `v...` release tag, and one operation:

- `migrate-front` is the explicit one-time migration. It starts slot A and the
  stable front without stopping the legacy service. It creates a separate
  fixed-port `31416` readiness check and front backend, preserves the IG named
  port `http:31415`, adds `front:31416`, sets one-hour connection draining,
  then switches only the environment's URL-map target. Rollback imports the
  exact previous URL map. The legacy backend stays available after success for
  pre-cutover connections and rollback. A failed LB cutover can resume the
  already verified front topology, while a completed cutover fails closed on a
  duplicate migration request.
- `slot` is the routine release path and fails when the front topology is not
  installed. It downloads the Linux binary and `SHA256SUMS` from the requested
  GitHub release, verifies the checksum and tag commit provenance, retains the
  binary under `/opt/subrouter/releases/<tag>/`, and starts it in the inactive
  loopback supervisor slot. The same bytes run the slot supervisor and worker.

Routine deployment starts unpaused real Codex WebSocket and HTTP sessions
through the public hostname. It enables the candidate slot, atomically persists
that slot as the front restart default, switches the front, validates new and
resumed turns, then retires the old slot supervisor. Retirement closes new
accepts but keeps its control socket and pinned traffic alive. The old slot is
stopped only after the front reports zero pinned connections, with no drain
timeout or forced close. Rollback persists and switches to the prior live slot
before retiring the candidate. Do not remove the legacy backend or port 31415
until separate observer evidence proves no pre-migration connections remain.

GitHub authenticates with Workload Identity Federation. It has no stored GCP
key and receives only temporary credentials for the deploy job. Account OAuth
credentials are never part of the workflow; account import remains the
authenticated HTTP `POST` flow described below.

The publish script configures the server with `sr server add`, then runs
`sr server install` for the explicit tag. The installer generates distinct
admin and account-import tokens, sends them over IAP standard input, and stores
both in the private local server configuration. Status uses the admin token;
account onboarding uses the narrower import token. Neither token is printed or
placed in process arguments. OAuth account import uses the authenticated public
HTTP API.

End users authenticate with cmux Stack login and receive a tenant-scoped route:

```bash
sr login
sr codex
```

The browser login uses the same Stack identity as cmux, exchanges it for a
tenant key at `/_subrouter/auth/stack`, and writes the tenant-scoped public URL
to the local Codex configuration. `sr remote use cmux-local` keeps the proxy on
the Mac while leasing short-lived access credentials from the same tenant.

Operators can add a fresh server-owned Codex OAuth account over authenticated
HTTP when needed:

```bash
sr server login team
```

This performs a fresh Codex login and sends the credential with authenticated
`POST`. It never uses SSH, SCP, or gcloud for credential transfer. Use
`--device-auth` only on a headless shell that cannot receive the localhost
browser callback.

The old account-file upload helper is kept only as a compatibility wrapper:

```bash
deploy/gcp/upload-codex-accounts.sh team --device-auth
```

It delegates to `sr server login` and rejects the previous `--move` and `--copy-unsafe` paths.

## Client usage

```bash
sr login
sr codex
```

Health check:

```bash
curl https://sr.cmux.com/_subrouter/health
curl https://sr.cmux.com/_subrouter/ready
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
