# Deploying a hosted Subrouter

Three supported paths: systemd on a Linux VM, Docker/compose, and Fly.io. All of them run `subrouter serve` on port 31415 with state under `/var/lib/subrouter`. Read [production.md](production.md) before sharing a server with a team.

**Warning: the data plane has no per-request auth yet.** `SUBROUTER_ADMIN_TOKEN` protects the `/_subrouter/*` admin endpoints, but any client that can reach the listener can proxy LLM traffic through your accounts. Keep the listener on a private network (Tailscale, VPC, or loopback) until multi-tenant client keys land. This applies doubly to Fly.io, where the app URL is public TLS.

## Admin token bootstrap

Every non-loopback deployment needs an admin token. Generate one once and reuse it on the client side:

```bash
TOKEN="$(openssl rand -hex 32)"
```

After the server is up, register it locally without printing the token:

```bash
sr server add team --url https://<server>:31415 --admin-token "$TOKEN" --default
sr server sync team --device-auth
```

`sr server sync` creates fresh server-owned OAuth chains. Do not copy local `~/.subrouter/codex/accounts/*.json` or `~/.codex/auth.json` to a server; Codex refresh tokens rotate and shared chains invalidate each other.

## Sentry error reporting

All three paths accept `SENTRY_DSN`. When set, `subrouter serve` reports handler panics and high-signal failures (upstream 502s, OAuth refresh chain deaths, account selection failures) to Sentry. Events are scrubbed before send: no request or response bodies, no Authorization or API-key headers, and token-shaped strings (`sk-`, `srt_`, `eyJ`, Bearer values) are redacted. Unset, nothing initializes and behavior is unchanged. `SENTRY_ENVIRONMENT` names the environment.

## systemd (Linux VM)

The existing path, documented in [production.md](production.md) and the [README](../README.md#linux-systemd-service):

```bash
curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sudo sh
sudo sr install-systemd --addr 0.0.0.0:31415 --admin-token "$TOKEN" --sentry-dsn "$SENTRY_DSN"
```

Re-running `install-systemd` preserves `SUBROUTER_ADMIN_TOKEN`, `SENTRY_DSN`, `SUBROUTER_TRANSCRIPTS`, and `SUBROUTER_EXTRA_ARGS` from `/etc/default/subrouter` when the flags are omitted. When a token or DSN is present the file is written mode 0600.

On macOS, `subrouter install-daemon --sentry-dsn <dsn>` bakes `SENTRY_DSN` into the LaunchAgent plist. The flag defaults to `SENTRY_DSN` from the invoking shell, so re-running the installer with the variable exported keeps it.

## Docker / compose

The repo root has a multi-stage `Dockerfile` (static Go build, Alpine runtime, non-root user) and a `docker-compose.yml` example with a named state volume:

```bash
export SUBROUTER_ADMIN_TOKEN="$TOKEN"
export SENTRY_DSN="https://...@sentry.io/..."   # optional
docker compose up -d
curl -fsS http://127.0.0.1:31415/_subrouter/health
```

Release tags publish `ghcr.io/manaflow-ai/subrouter:<version>` for amd64 and arm64; `:latest` moves only on stable versions, never on prerelease tags (anything with a hyphen, like `v1.2.3-rc1`). State (accounts, sessions) lives in the `subrouter-state` volume at `/var/lib/subrouter`; account files written there survive image upgrades. To add accounts, use `sr server add` + `sr server sync` from your workstation rather than exec-ing into the container.

The compose file publishes 31415 on all interfaces. On a shared host, change the mapping to `127.0.0.1:31415:31415` or a tailnet address.

## Fly.io

[deploy/fly/fly.toml](../deploy/fly/fly.toml) runs a single machine with a volume and Fly edge TLS. Fly gives the app a public HTTPS URL, so this is the setup that most needs the auth warning above.

```bash
fly launch --no-deploy --copy-config -c deploy/fly/fly.toml
fly volumes create subrouter_state --size 1
fly secrets set SUBROUTER_ADMIN_TOKEN="$TOKEN"
fly secrets set SENTRY_DSN="https://...@sentry.io/..."   # optional
fly deploy -c deploy/fly/fly.toml
fly scale count 1
curl -fsS https://<app>.fly.dev/_subrouter/health
```

Keep it at one machine. Session-to-account stickiness, the account store, and usage scores are process-local; a second machine would split them. The volume mount at `/var/lib/subrouter` holds accounts and sessions across deploys.

Clients then point at the public URL:

```bash
sr server add fly --url https://<app>.fly.dev --admin-token "$TOKEN" --default
sr server sync fly --device-auth
```
