# Production readiness

Use this checklist before putting a shared Subrouter on a tailnet or a public-facing VM.

## Listener

- Bind public/shared servers to the tailnet address or a private network. Avoid raw internet exposure.
- Keep `/_subrouter/health` unauthenticated for liveness checks.
- Use `/_subrouter/ready` for readiness checks. It returns 503 while the process is draining.
- Set `SUBROUTER_ADMIN_TOKEN` for any non-loopback listener. Sensitive admin endpoints then require `Authorization: Bearer <token>` or `X-Subrouter-Admin-Token: <token>`.
- Set a provider-specific `SUBROUTER_*_GATEWAY_TOKEN` whenever its API gateway is enabled. Gateways stay disabled when either the provider key or client-facing token is missing.

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

`install-systemd` preserves existing admin, Gemini, Anthropic, and OpenAI provider/gateway credentials. When any secret is configured, `/etc/default/subrouter` is atomically replaced with mode `0600`.

## Team API gateways

Regenerate the current defaults, then use a protected editor to set the six `SUBROUTER_{ANTHROPIC,OPENAI,GEMINI}_{API_KEY,GATEWAY_TOKEN}` values. Do not put secrets in command arguments or shell history.

```bash
sudo sr install-systemd --start=false
sudo chmod 600 /etc/default/subrouter
sudoedit /etc/default/subrouter
sudo systemctl restart subrouter
```

Anthropic SDKs use `http://<server>:31415/anthropic`; OpenAI SDKs use `http://<server>:31415/api/v1` (`/openai/v1` is an alias). Gemini SDKs using default `v1beta` use `http://<server>:31415`; clients selecting `v1` or `v1alpha` use `http://<server>:31415/gemini`. Existing root `/v1/*` and `/responses` routes retain subscription/account routing. Override gateway destinations with `--anthropic-gateway-upstream`, `--openai-gateway-upstream`, and `--gemini-upstream` without changing root provider routing.

Gemini Live requires HTTPS termination because official Python clients dial `wss`. The terminating proxy must forward the provider gateway paths, including `/ws/*`, and set `X-Forwarded-Proto: https`; use its `https://` URL as the Gemini SDK base. It must reject `/_subrouter/*` instead of forwarding management routes through its trusted loopback connection. If the proxy replaces `Host`, start Subrouter with `--gemini-public-url https://<public-host>` so resumable upload continuations use the external origin without trusting client-supplied forwarding headers.

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

True zero-drop binary upgrades still need a stable supervisor listener that owns `:31415` and routes to versioned workers. The worker primitives are now present: `/_subrouter/ready`, `/_subrouter/drain`, and active proxy request accounting.

## Launch checks

Run these before announcing a shared server:

```bash
curl -fsS http://<server>:31415/_subrouter/health
curl -fsS http://<server>:31415/_subrouter/ready
sr server status team
```

Then check logs for refresh and routing failures:

```bash
ssh <server> 'journalctl -u subrouter --since "2 hours ago" --no-pager | grep -Ei "WARN|ERROR|failed|401|502|503|no usable|refresh_token" | tail -n 200'
```
