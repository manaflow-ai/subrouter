# Production readiness

Use this checklist before putting a shared Subrouter on a tailnet or a public-facing VM.

## Listener

- Bind public/shared servers to the tailnet address or a private network. Avoid raw internet exposure.
- Keep `/_subrouter/health` unauthenticated for liveness checks.
- Use `/_subrouter/ready` for readiness checks. It returns 503 while the process is draining.
- Set `SUBROUTER_ADMIN_TOKEN` for any non-loopback listener. Sensitive admin endpoints then require `Authorization: Bearer <token>` or `X-Subrouter-Admin-Token: <token>`.
- Account import always requires the configured admin token or a validated tenant key, even on loopback. Credential transfer uses authenticated HTTP `GET` and `POST`, never SSH, SCP, or gcloud.

## Linux install

```bash
TOKEN="$(openssl rand -hex 32)"
sudo sr install-systemd \
  --addr 0.0.0.0:31415 \
  --admin-token "$TOKEN"
```

Store the same token in local server config for CLI management:

```bash
sr server add team \
  --url http://100.64.0.1:31415 \
  --admin-token "$TOKEN" \
  --default
```

`install-systemd` preserves an existing `SUBROUTER_ADMIN_TOKEN` if `--admin-token` is omitted. When a token is configured, `/etc/default/subrouter` is written with mode `0600`.

## Transcripts

Transcript recording is off by default because it stores full request and response payloads and can grow quickly. For a shared server, only enable it with cloud upload and local cleanup:

```bash
sudo sed -i 's|^SUBROUTER_TRANSCRIPTS=.*|SUBROUTER_TRANSCRIPTS=/var/lib/subrouter/transcripts|' /etc/default/subrouter
sudo sed -i 's|^SUBROUTER_TRANSCRIPT_ARGS=.*|SUBROUTER_TRANSCRIPT_ARGS="--transcripts=/var/lib/subrouter/transcripts"|' /etc/default/subrouter
sudo sed -i 's|^SUBROUTER_EXTRA_ARGS=.*|SUBROUTER_EXTRA_ARGS="--transcript-gcs-uri=gs://<bucket>/<prefix> --transcript-gcs-sync-interval=5m --transcript-gcs-sync-timeout=30m --transcript-local-retention=24h --transcript-max-local-bytes=2GiB"|' /etc/default/subrouter
sudo install -d -o subrouter -g subrouter -m 0750 /var/lib/subrouter/transcripts
sudo systemctl restart subrouter
```

Local cleanup runs only after a successful GCS sync. Before deleting a local transcript, Subrouter copies it to an immutable object under the destination `_archive/` prefix.

## Draining

Before replacing a process, ask it to drain:

```bash
curl -fsS -X POST http://127.0.0.1:31415/_subrouter/drain
curl -fsS http://127.0.0.1:31415/_subrouter/drain-status
```

Drain mode rejects new proxy sessions but allows active sessions to continue. This is enough for a controlled shutdown, but it does not keep the listener available for new clients while the old process drains.

## Upgrades

Current production-safe behavior:

- SIGTERM/SIGINT switches the process into drain mode.
- The HTTP server waits up to `--shutdown-timeout` for in-flight proxy requests to finish.
- systemd units use `TimeoutStopSec=10min`.
- On macOS, `subrouter supervise` owns the stable client listener and starts workers on inherited private sockets. `POST /_subrouter/upgrade` starts and health-checks the replacement worker, routes new connections to it, and keeps every existing connection pinned to the old worker until it closes.
- The supervisor binary is installed separately and is not replaced by routine worker updates.
- SIGTERM/SIGINT closes the supervisor listener, waits up to `--drain-timeout` (default `10m`) for accepted connections, then stops its workers.

The macOS updater and one-time LaunchDaemon migration scripts live in `deploy/macos/`. Run the migration without `--activate` first to prepare and validate the supervised plist. Activation is the last upgrade that replaces the public listener; later worker upgrades do not restart the supervisor.

## Launch checks

Run these before announcing a shared server:

```bash
curl -fsS http://<server>:31415/_subrouter/health
curl -fsS http://<server>:31415/_subrouter/ready
sr server status team
```

These client-side checks are the launch gate. Infrastructure operators can inspect the service journal or Cloud Logging separately, but ordinary users never need shell access to the VM.
