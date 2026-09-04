# Subrouter

Subrouter is a local AI coding-agent proxy. It routes traffic across Codex accounts with sticky conversation-to-account assignment so cached context stays useful.

## Goals

- Run fast on a Mac Mini.
- Forward requests with normal Go reverse-proxy behavior, including headers and streaming responses.
- Support subscription accounts first, API keys second.
- Keep each conversation pinned to one account.
- Pick a fresh account for a new conversation based on available rate-limit headroom.
- Provide the Codex account manager and daemon in one Go binary.

## Install

### Keeping the CLI current on macOS

A shared server autoupdates its worker. A laptop does not, so clients drift behind the servers they talk to and hit failures nobody can reproduce. Install the per-user updater once:

```bash
curl -fsSL https://raw.githubusercontent.com/manaflow-ai/subrouter/main/deploy/macos/install-cli-autoupdate.sh | bash
```

It installs a LaunchAgent that checks daily, compares the release tag against `~/.subrouter/cli-version`, and exits without downloading when they match. Updates go through `install.sh`, so the release checksum is verified, and a missing binary forces a reinstall even when the marker looks current. Logs land in `~/Library/Logs/subrouter-cli-autoupdate.log`. Remove it with `launchctl bootout gui/$(id -u)/ai.manaflow.subrouter-cli-autoupdate`.

### GCP setup prompt

Paste this into Claude, Codex, or another coding agent with GCP operator access and a local browser for OAuth:

```text
Set up Subrouter as a shared production service.

Inputs:
- GCP project, zone, and instance: <project> <zone> <instance>
- Public server URL: https://sr.cmux.com
- Local server nickname: team

Rules:
- Do not copy ~/.codex/auth.json or local ~/.subrouter/codex/accounts/*.json to the server.
- Server OAuth accounts must be created with fresh server-owned login flows.
- Do not print access tokens, refresh tokens, API keys, id tokens, or admin tokens.
- Never use SSH, SCP, or gcloud to transfer account credentials.
- Accept port 31415 only from Google load-balancer ranges and SSH only through IAP.
- End-user authentication and proxy traffic must use the public HTTPS hostname.
- Use the released Subrouter binary unless I explicitly ask you to build from source.

Steps:
1. Configure the GCP project and publish the released service with deploy/gcp/publish-subrouter.sh. The installer must generate and provision its protected account-import token without printing it.
2. Verify from this client machine:
   sr server status team
   curl -fsS https://sr.cmux.com/_subrouter/health
   curl -fsS https://sr.cmux.com/_subrouter/ready
3. Create server-owned Codex OAuth chains:
   sr server sync team
   Follow each OAuth flow. Do not upload local refresh tokens.
4. Verify:
   sr server status team
   curl -fsS https://sr.cmux.com/_subrouter/health
   curl -fsS https://sr.cmux.com/_subrouter/ready
5. Report:
   - systemd active/running status
   - health and readiness result
   - number of registered Codex OAuth accounts
   - the exact command I should use for Codex through Subrouter
```

For local-only use on macOS, paste this instead:

```text
Set up Subrouter locally for Codex.

Rules:
- Do not print tokens.
- Do not edit Codex config by hand unless Subrouter docs say so.

Steps:
1. Install:
   curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sh
2. Install and verify the LaunchAgent:
   sr install-daemon
   curl -fsS http://127.0.0.1:31415/_subrouter/health
   curl -fsS http://127.0.0.1:31415/_subrouter/ready
3. Add Codex accounts:
   sr add
   Repeat as needed.
4. Verify:
   sr status
5. Report the command I should use:
   sr codex
```

### Manual install

Install the released Go binary directly:

```bash
curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sh
```

On a Linux server, install to `/usr/local/bin`:

```bash
curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sudo sh
```

Install with npm:

```bash
npm install -g subrouter
```

Install with Python:

```bash
pipx install subrouter
```

All install paths provide `subrouter`, `sr`, and `cx`. The npm and Python wrappers download the matching Go release binary for macOS, Linux, Windows, FreeBSD, OpenBSD, or NetBSD on amd64, arm64, or supported 32-bit variants. Set `SUBROUTER_BIN` to use a local binary instead.

### Local macOS daemon

On macOS, install Subrouter as a localhost-only LaunchAgent:

```bash
make build
./bin/subrouter install-daemon
```

This installs the binary to `~/bin/subrouter`, installs `~/bin/sr` and `~/bin/cx` as symlinks to the same Go binary, writes `~/Library/LaunchAgents/ai.manaflow.subrouter.plist`, starts the service, and runs:

```bash
~/bin/subrouter serve --addr 127.0.0.1:31415 --sr-switch-interval 10m
```

Transcript recording is off by default. Enable it explicitly with `subrouter install-daemon --transcripts ~/.subrouter/transcripts`.

The 10 minute `sr` auto-switch interval is the default. Override it with `subrouter install-daemon --sr-switch-interval 5m`, or disable it with `--sr-switch-interval 0`. The old `--cx-switch-interval` flag remains a compatibility alias.

### Linux systemd service

On a Linux server, install the binary and service:

```bash
curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sudo sh
sudo sr install-systemd --addr 0.0.0.0:31415
```

This creates a `subrouter` system user, stores state under `/var/lib/subrouter`, writes `/etc/systemd/system/subrouter.service`, installs `subrouter`, `sr`, and `cx` in `/usr/local/bin`, and starts:

```bash
/usr/local/bin/subrouter serve --addr 0.0.0.0:31415 --sessions /var/lib/subrouter/sessions.json --sr-switch-interval 10m
```

Transcript recording is off by default. Enable it explicitly with `sudo sr install-systemd --transcripts /var/lib/subrouter/transcripts`.

If legacy `switchboard` or `gateway` services exist, `sr install-systemd` stops and disables them, merges their `/var/lib/...` state into `/var/lib/subrouter`, and preserves their extra service args.

Useful endpoints:

```text
GET /_subrouter/health
GET /_subrouter/ready
POST /_subrouter/drain
GET /_subrouter/drain-status
GET /_subrouter/accounts
GET /_subrouter/account-status
POST /_subrouter/account-status
GET /_subrouter/account-import
POST /_subrouter/account-import
GET /_subrouter/usage-status
GET /_subrouter/sessions
GET /_subrouter/dashboard
GET /_subrouter/transcripts
```

`/_subrouter/health` is liveness. `/_subrouter/ready` returns 503 while the process is draining. `/_subrouter/drain` is loopback-only and tells the process to reject new proxy sessions while allowing active sessions to continue. `GET /_subrouter/account-status` validates only expired OAuth tokens; `POST /_subrouter/account-status` force-refreshes token chains and should be reserved for explicit diagnostics. `GET /_subrouter/usage-status` returns the read-only account usage data rendered by `sr server status <name>`.

For servers that listen on a non-loopback address, set an admin token before exposing account, session, dashboard, or transcript endpoints:

```bash
TOKEN="$(openssl rand -hex 32)"
sudo sr install-systemd --addr 0.0.0.0:31415 --admin-token "$TOKEN"
sr server add team --url http://100.64.0.1:31415 --admin-token "$TOKEN" --default
```

When `SUBROUTER_ADMIN_TOKEN` or `--admin-token` is set, non-loopback requests to sensitive `/_subrouter/*` endpoints must send `Authorization: Bearer <token>` or `X-Subrouter-Admin-Token: <token>`. Loopback stays trusted for ordinary admin endpoints. Account onboarding uses a distinct `SUBROUTER_ACCOUNT_IMPORT_TOKEN`; that token authorizes only `GET` and `POST /_subrouter/account-import` and cannot access admin APIs or proxy traffic.

A server with neither credential configured rejects every account import, including `sr add`. That state is reported as `"account_import": "disabled"` by `/_subrouter/health` and logged as a warning at startup, and `sr doctor` runs the same preflight `sr add` runs against the selected server.

Serving processes accept and refresh only Codex OAuth credentials created by an isolated Subrouter login. This keeps proxy refresh-token chains independent from interactive `~/.codex/auth.json`. After upgrading a local store that contains older accounts without provenance metadata, run `sr codex migrate-isolation` (or add `--device-auth`) once; it enumerates the affected accounts, requires each login to match the expected identity, and leaves the active Codex login unchanged. For a shared server use `sr server sync <name> --all`, or repair an individual hosted credential with `sr account repair <account-id>`. Complete this migration before starting the upgraded serving process: upgraded serving rejects affected credentials immediately rather than continuing to use them until their access tokens expire. `sr doctor` and `sr status` report the remaining local migration count without exposing credential material.

For a sensitive migration, choose one credential layout deliberately:

- A full isolated production cutover uses `sr codex enroll-isolated` without
  `--only`. It stages the complete account inventory and keeps the legacy
  service independently usable until the routed canaries pass.
- For offline or canary validation only, repeat `--only ACCOUNT` to enroll
  selected accounts. A partial candidate cannot pass the full activation
  preflight.
- An ordinary in-place upgrade keeps the original store and uses
  `sr codex migrate-isolation` when needed. It does not provide an independent
  credential rollback guarantee.

See the optional
[rollback-isolated transactional cutover](docs/upgrades.md#transactional-per-user-launchagent-migration)
for the full production procedure.

### Tailnet authentication for self-hosted servers

A server whose port is already restricted to a tailnet by ACL does not need a second credential system on top of it. Start it with `--tailscale-auth` (or `SUBROUTER_TAILSCALE_AUTH=1`) and non-loopback callers are authenticated by their tailnet identity instead:

```bash
subrouter serve --addr 0.0.0.0:31415 --tailscale-auth
sr server add mac-mini --url http://mac-mini.tailnet.ts.net:31415   # no tokens
sr add                                                              # just works
```

Identity comes from this machine's own tailscaled through `tailscale whois`, so it is an assertion about a WireGuard-authenticated peer rather than a claim carried in the request, and account imports are logged with the tailnet user or tags that made them. Narrow it further with `--tailscale-auth-users lawrence@example.com` or `--tailscale-auth-tags tag:dev-workstation`; with neither, every tailnet peer is accepted, which is the point of the mode.

Enabling it closes the unsecured legacy default: a caller the tailnet does not recognize gets only the token path, never open access. Configured tokens keep working alongside it. It is refused together with `--multi-tenant`, because a shared cloud deployment authenticates tenants rather than network peers, and `/_subrouter/health` reports the active mode as `"auth": "tailnet" | "token" | "tenant" | "open"`.

`sr server install <name>` provisions those credentials for you and keeps both sides in step. It reaches a GCP instance through gcloud, and any other machine through SSH:

```bash
sr server add mac-mini --url http://100.64.0.9:31415 --ssh-host worker@mac-mini
sr server install mac-mini
```

On the host that resolves to `sudo sr install-systemd` on Linux and `sudo sr install-launchd` on macOS. `install-launchd` provisions credentials into an existing LaunchDaemon rather than creating the service: it writes the tokens to 0600 files owned by the service user, points `SUBROUTER_ADMIN_TOKEN_FILE` and `SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE` at them, and reloads the job. Every other key in the plist is left alone, so a host keeps its supervisor layout, service user, and per-host flags across a credential rotation. Build the service itself with [deploy/macos/migrate-launchdaemon-to-supervisor.sh](deploy/macos/migrate-launchdaemon-to-supervisor.sh) first.

## GCP deployment

See [deploy/gcp/README.md](deploy/gcp/README.md) for the small GCP + Tailscale Subrouter deployment flow.
See [docs/production.md](docs/production.md) for the production checklist before running a shared server.
See [deploy/docker/README.md](deploy/docker/README.md) for hardened local-account and cmux.com team containers.

Transcript recording is off by default. To persist raw Subrouter transcripts, pass a transcript directory:

```bash
subrouter serve --transcripts ~/.subrouter/transcripts
```

Transcripts are JSONL files keyed by agent type and session id under `by-agent/<agent-type>/by-session/<agent-session-id>.jsonl`. They include Subrouter metadata, redacted headers, HTTP/SSE body chunks, HTTP/SSE body summaries, and WebSocket message payloads as base64 with byte counts and SHA-256 hashes. Each event includes `agent_type` and `agent_session_id`; Codex events also include `codex_session_id` for matching `~/.codex/sessions` JSONL files. This is intentionally storage-heavy and can contain sensitive request/response payloads. Authorization-style headers are redacted, but bodies are stored in full.

The synthetic model-catalog session is never recorded. Every Codex client polls `/models` continuously, all of it lands under one session id, and each poll would write the request metadata plus the whole catalog body: on the team server that single file reached 30 GB in two days, dwarfed every real transcript, saturated the upload, and filled the disk. Its responses are still inspected for quota signals; they are just not written down.

When transcript recording is enabled, `/_subrouter/dashboard` serves an internal HTML dashboard over the same Subrouter listener. It shows token usage over time, usage by user email, usage by selected account, session assignments, transcript summaries, and links to sanitized transcript event JSON under `/_subrouter/transcripts/<agent-type>/<session-id>`. Raw internal trajectory JSON with decoded body text is available under `/_subrouter/transcripts/<agent-type>/<session-id>/raw`.

To mirror transcripts to GCS without blocking proxy requests, also pass a `gs://` destination:

```bash
subrouter serve \
  --transcripts ~/.subrouter/transcripts \
  --transcript-gcs-uri gs://bucket/prefix \
  --transcript-gcs-sync-timeout 30m \
  --transcript-local-retention 24h \
  --transcript-max-local-bytes 2GiB
```

Azure blob storage is the other destination, and the only one that works off GCP: the GCS syncer authenticates through the GCE metadata server, so a Mac or a laptop can reach a bucket only by shelling out to `gsutil`.

```bash
export SUBROUTER_TRANSCRIPT_AZURE_KEY_FILE=/var/lib/subrouter/transcript-azure-key   # or SUBROUTER_TRANSCRIPT_AZURE_SAS
subrouter serve \
  --transcripts /var/lib/subrouter/transcripts \
  --transcript-azure-url https://<account>.blob.core.windows.net/<container>/<prefix> \
  --transcript-local-retention 24h \
  --transcript-max-local-bytes 2GiB
```

The Azure syncer writes append blobs, so each byte uploads once. Transcripts are append-only and large (a busy session reaches gigabytes within hours), and re-sending the whole file every interval is how the first version earned "503 The server is busy" from Azure. Before a local file is deleted, the blob is snapshotted server side, which preserves exactly the retired bytes with no upload at all; only a file that was never uploaded is copied the slow way, under `_archive/<path>/<time>-<size>-<hash>.jsonl`. A throttled or failed request is retried with backoff, and one failing file no longer stops the rest of the pass. Uploads are paced at 2 MiB/s by default (`--transcript-azure-max-bytes-per-second`, `0` for no cap). The first backlog upload saturated the host's uplink for hours, and the proxy shares that link: health probes from the tailnet timed out and SSH stalled while gigabytes of transcript went out. Transcripts are background data and the proxy is the product, so the upload yields; the cap still clears more per hour than a busy day of sessions produces. Retention runs before and after the upload pass, because a backlog can occupy the whole interval and a spool that only prunes afterwards keeps growing past its cap the entire time. A file whose blob has not caught up is never retired: the append pass finishes it first.

Both destinations follow the same rules: upload on the background interval, never on the request path, and delete a local file only after the exact bytes exist remotely. The Azure syncer signs its own requests with the storage account key (Shared Key), so the host needs no Azure CLI. A configured destination with no usable credential logs a warning at startup rather than starting quietly and copying nothing.

The daemon uploads with the GCS JSON API on a background interval. Local transcript writes stay on the request path; GCS upload failures are logged and retried later. Local cleanup only runs after a successful GCS sync. Files selected for cleanup are copied to an immutable `_archive/` object before local deletion so future resumed sessions cannot overwrite the only cloud copy.

For best cache behavior, clients should send a stable header per conversation:

```text
X-Subrouter-Session: <conversation-or-thread-id>
```

If that header is missing, Subrouter checks Codex headers such as `x-codex-window-id` and `x-codex-turn-state`, common session headers, query params, and small JSON bodies for `session_id`, `conversation_id`, or `thread_id`.

Subrouter scopes sticky assignments and transcript files by agent type. It infers `codex`, `claude`, or `gemini` from provider session headers, uses the provider name for the API-key providers below (a request to `/qwen-token/...` is scoped to `qwen-token`), and clients can set an explicit namespace:

```text
X-Subrouter-Agent: codex
```

For teammate-level graphs, clients can also send a self-reported user header:

```text
X-Subrouter-User-Email: alice@example.com
```

Subrouter stores the normalized email on the session assignment for up to 30 days after the session's last activity. Proxy logs contain only a truncated SHA-256 `user_hash`, not the email. The full value is available through admin-authorized `GET /_subrouter/sessions`; hosted tenant keys additionally need `manage_accounts`. An administrator can delete the complete assignment and its email with `DELETE /_subrouter/sessions?agent_type=TYPE&session_id=ID`. This is observability metadata, not authentication. To force a selected account, send `X-Subrouter-Account-ID`; Codex API-key labels can omit the `apikey:` prefix, and an API-key account for another provider is identified as `<provider>:<label>`, such as `qwen-token:work`. Subrouter strips `X-Subrouter-Session`, `X-Subrouter-Agent`, `X-Subrouter-User-Email`, `X-Subrouter-User`, `X-User-Email`, `X-Subrouter-Account-ID`, and `X-Subrouter-Account` before forwarding upstream.

Only send this header when storing the normalized email in Subrouter session state and a truncated hash in proxy logs is acceptable under your privacy policy. Protect the admin-gated sessions endpoint and log access accordingly; the value is self-reported and must not be treated as verified identity.

## Codex CLI

`subrouter codex` is a direct Codex wrapper. Use it anywhere you would use `codex`:

```bash
subrouter codex
subrouter codex exec "your prompt"
subrouter codex --version
```

The wrapper is intentionally opt-in. Plain `codex` keeps Codex's normal direct
OpenAI configuration and does not depend on a running Subrouter; `sr codex`
routes that process through the configured Subrouter account pool. A session
started or migrated through the wrapper should be reopened with the directly
copyable `sr codex resume <session-id>` form.

The wrapper launches the child with an authenticated custom provider pointed at
Subrouter:

```toml
model_provider = "subrouter"
[model_providers.subrouter]
base_url = "http://127.0.0.1:31415/v1"
experimental_bearer_token = "subrouter"
wire_api = "responses"
supports_websockets = true
```

It does not edit Codex config or depend on `~/.codex/auth.json`. This is
intentional: an expired or logged-out local ChatGPT credential must not prevent
a request from reaching Subrouter, where the selected pool account is applied.
Do not set a dummy `OPENAI_API_KEY`; the wrapper supplies a non-secret provider
token only for the local hop. Subrouter replaces it with the selected account
before forwarding. Responses and realtime WebSocket requests use the same
route. Resume through `sr codex resume ...` so the provider overrides are
present on the resumed process.

`sr codex` owns its routing overrides. It removes older Subrouter provider,
backend `-c`, and local-provider `--oss` values from a copied or saved command
before adding the selected server, preventing another provider or a retired URL
from overriding the current configuration. Session managers can preserve the
opt-in launcher identity via the exported `SUBROUTER_CODEX_LAUNCHER` and
`SUBROUTER_CODEX_RESUME_COMMAND` variables; their value follows the invoked
`sr`, `subrouter`, or `cx` alias.

Override the subrouter URL with `SUBROUTER_CODEX_BASE_URL` if needed. See [docs/codex.md](docs/codex.md) for details and the custom-provider fallback.

If `SUBROUTER_CODEX_BASE_URL` is not set, the wrapper uses local `127.0.0.1:31415/v1`. To make `sr codex`, Codex Desktop's app-server, and the default `sr` usage view use a remote Subrouter, register and select a named server:

```bash
sr server add team --url http://100.64.0.1:31415 --default
```

For a server reached through Tailscale, record its exact node ID so a MagicDNS
rename does not strand clients:

```bash
sr server add team \
  --url http://current-host.example.ts.net:31415 \
  --tailscale-node-id nEXAMPLE11CNTRL \
  --default
```

Subrouter loads `tailscale status --json`, requires an exact node-ID match, and
only trusts the stored URL when its host is still one of that node's advertised
addresses and its health check passes. Otherwise it rebuilds only the URL host,
tries the node's current MagicDNS name and Tailscale IPs, health-checks the
candidate, and atomically updates the server registry. It never matches a
similar hostname, and discovery is time-bounded and fail-closed. The CLI is
found through `PATH` or the standard macOS Tailscale app bundle; set
`SUBROUTER_TAILSCALE_BIN` only for a non-standard installation.
Replacing a named server with a different `--url` clears its prior node binding
unless `--tailscale-node-id` is supplied again. If discovery for the unpinned
default fails, `sr codex` may use a healthy local daemon under the normal local
fallback policy; an explicit environment pin remains fail-closed.

`sr server add --default` and `sr server use <name>` write these top-level keys in `CODEX_HOME/config.toml`, or `~/.codex/config.toml` when `CODEX_HOME` is unset:

```toml
openai_base_url = "http://100.64.0.1:31415/v1"
chatgpt_base_url = "http://100.64.0.1:31415/backend-api"
experimental_realtime_ws_base_url = "http://100.64.0.1:31415/v1"
```

Use `--no-codex-config` to change only Subrouter's selected server. Use `sr server use local` or `sr server clear-default` to return to the local daemon and rewrite Codex config to `127.0.0.1:31415`.

The server name is only a local nickname. Use whatever matches your setup, such as `team`, `prod`, or `staging`. For a one-off command, set `SUBROUTER_CODEX_SERVER=team`.
Rename a local server nickname with `sr server rename <old> <new>`.

Top-level `sr` account commands follow the selected target. If `sr server use team` is active, `sr add`, `sr add-key`, `sr list`, `sr status`, `sr usage`, and `sr pick` talk to that server. If the selected target is local, those same commands use the local account store. Commands without a remote-safe implementation fail before editing local auth when a server is selected. Use `SUBROUTER_CODEX_SERVER=local sr <command>` for a one-off local command.

Set `SUBROUTER_CODEX_USER_EMAIL` to attribute Codex traffic to a teammate:

```bash
SUBROUTER_CODEX_USER_EMAIL=alice@example.com subrouter codex exec "your prompt"
```

Force a specific Subrouter account, including an API-key account, with `SUBROUTER_CODEX_ACCOUNT_ID`:

```bash
SUBROUTER_CODEX_ACCOUNT_ID=team-codex-1 subrouter codex exec "your prompt"
SUBROUTER_CODEX_ACCOUNT_ID=apikey:team-codex-1 subrouter codex exec "your prompt"
```

The wrapper always uses the custom `subrouter` provider with WebSockets enabled
and sends `X-Subrouter-Agent: codex`. These variables add
`X-Subrouter-User-Email` and `X-Subrouter-Account-ID`. Subrouter still replaces
outbound credentials before forwarding upstream.

Codex Desktop is separate from the CLI wrapper. Its app-server reads `CODEX_HOME/config.toml`, and its Electron shell reads `CODEX_API_BASE_URL` at process start. See [docs/codex.md](docs/codex.md) for the desktop routing setup.

## Codex accounts

Subrouter has a native Go implementation of the Codex account manager. It reads and writes its account store under Subrouter's data directory:

```text
~/.subrouter/codex/accounts/*.json
```

On first run, Subrouter migrates legacy `~/.codex-accounts` state into `~/.subrouter/codex`. Codex's own active auth file remains `~/.codex/auth.json`.

Server-owned OAuth accounts must be created with fresh logins because Codex refresh tokens rotate. Do not copy local OAuth account files to a server. To compare local OAuth emails with a configured server, validate server refresh-token chains, and reauth missing or invalid accounts on the server, run:

```bash
sr server sync team
```

To only show the diff:

```bash
sr server diff team
```

`sr server sync` prints the plan and asks before opening login. Use `--yes` for unattended sync, `--email you@example.com` to reauth one email, or `--all` to replace every local OAuth email on the server with a new server-owned refresh-token chain. The server status check may refresh valid server-owned OAuth chains in place because Codex refresh tokens rotate.

Account login first checks the protected endpoint with `GET`, then sends the new credential with authenticated `POST`. It never transfers credentials with SSH, SCP, or gcloud. The server validates and atomically stores the credential, hot-reloads the account pool, and leaves existing HTTP and WebSocket proxy connections running. Use `--device-auth` only when the browser and CLI cannot share a localhost callback, such as a headless or remote shell.

Account-management commands are built into the `subrouter` binary:

```bash
go run ./cmd/subrouter add
go run ./cmd/subrouter import
go run ./cmd/subrouter list
go run ./cmd/subrouter status
sr status
```

The supported Codex commands include `add`, `add-key`, `import`, `list`, `switch`, `g`, `gui`, `gui-switch`, `remove`, `status`, `usage`, `server`, `add-admin-key`, `admin-keys`, `remove-admin-key`, `attach-project`, `claude`, and `gemini`. The older `subrouter cx <command>` form remains as a compatibility alias.

`sr switch` also syncs compatible ChatGPT Codex credentials into:

```text
~/.codex/auth.json
~/.local/share/opencode/auth.json      # provider key: openai
~/.pi/agent/auth.json                  # provider key: openai-codex
```

OpenCode uses XDG data home, so `XDG_DATA_HOME` changes its auth path. pi uses `PI_CODING_AGENT_DIR` when set. Existing unrelated provider credentials in those files are preserved.

Claude profiles are also native Go and use the same Subrouter store:

```bash
sr claude add <profile>                 # 1-year setup token (default)
sr claude add <profile> --token -       # paste an existing setup token on stdin
sr claude login <profile>               # classic browser OAuth login (refresh token)
sr claude list
sr claude switch <profile>
sr claude env
sr claude run <profile>
sr claude proxy [claude args...]
```

`sr claude add` runs `claude setup-token`, which mints a Claude subscription
access token that is valid for one year and has no refresh token. Paste the
printed token at the prompt (or pass it with `--token <token>` / `--token -`);
Subrouter verifies it against Anthropic, records the expiry, and stores it
without ever calling the OAuth refresh endpoint for that profile. `sr claude
list`, `sr claude add`, and server status print the expiry date, warn inside
the last 30 days, and name the re-add command once the token has expired,
because a setup token cannot renew itself. `sr claude login` (or `sr claude add
--oauth`) is the earlier flow: Claude Code's browser OAuth writes a refreshable
credential and the profile name defaults to the account email. Profiles created
that way keep refreshing exactly as before; the two kinds coexist in one store
and one server pool.

`sr claude list` reports only isolated local managed profiles and their local
login state. `sr claude run <profile>` launches one of those profiles directly
and refuses to start Claude when its local login is unavailable. Bare
`sr claude` chooses an initial account preference and launches through the
selected Subrouter server's pooled Claude accounts; `sr claude proxy` does the
same without the picker, and `sr claude proxy --account <profile>` pins one
server-pool account with no failover. Server-pool availability is separate from
local managed-profile login state, so the same label can be usable in
`sr status` while `sr claude list` says its local profile is not logged in.
Remote server-pool launches need neither local Claude profiles nor a local
Subrouter daemon; Claude arguments such as `--resume <session-id>` pass through
unchanged.

For manual client configuration, authenticate to the Subrouter proxy rather
than exposing an upstream Claude OAuth token. A trusted local or legacy
single-tenant server accepts the non-secret placeholder `subrouter`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:31415",
    "ANTHROPIC_AUTH_TOKEN": "subrouter",
    "ANTHROPIC_CUSTOM_HEADERS": "X-Subrouter-Agent: claude"
  }
}
```

For a tenant-scoped shared server, put the tenant key in the URL path and use
the same key as the auth token:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://router.example/t/srt_REDACTED",
    "ANTHROPIC_AUTH_TOKEN": "srt_REDACTED",
    "ANTHROPIC_CUSTOM_HEADERS": "X-Subrouter-Agent: claude"
  }
}
```

Remote tenant servers must use HTTPS unless they are reached over a verified
Tailscale address. Plain HTTP is allowed only on loopback or after Subrouter
verifies and pins the destination to a loopback or tailnet IP; a `*.ts.net`
name alone is not trusted. Because `/t/<key>` contains a credential, URL logs
and diagnostics must redact that path segment. The
`X-Subrouter-Agent: claude` marker remains required as the agent discriminator;
it does not replace the tenant path or token. `sr claude proxy` sets all three
values correctly from the selected server.
Subrouter then selects a Claude OAuth account from its own store, strips client
auth before forwarding, and adds the OAuth beta header. Claude Code prompt
caching does not require Subrouter-specific cache settings: Subrouter keeps the
same Claude conversation pinned to the same Claude account when that account is
still available, and forwards the client `Anthropic-Beta` values and request
body `cache_control` blocks unchanged.

Gemini has its own `sr gemini` namespace and store scaffold so future routing
cannot collide with Codex or Claude state. It is not currently a routed
provider or a native launcher.

## API-key providers

Beyond Codex and Claude, Subrouter routes a set of providers that authenticate
with an API key. Each one owns a path prefix, so a client picks a provider by
the URL it calls and Subrouter replaces the outbound credential with whichever
account it selects:

The [provider adapter matrix](docs/provider-adapters.md) distinguishes a server
route from a dedicated native-CLI launcher and records which paths have live
account validation. A server adapter does not by itself change how the vendor's
CLI is launched.

| Prefix | Provider | Default upstream |
|---|---|---|
| `/kimi` | Kimi For Coding | `https://api.kimi.com/coding/v1` |
| `/zai` | Z.AI coding | `https://api.z.ai/api/coding/paas/v4` |
| `/openrouter` | OpenRouter | `https://openrouter.ai/api/v1` |
| `/deepseek` | DeepSeek | `https://api.deepseek.com` |
| `/together` | Together AI | `https://api.together.ai/v1` |
| `/fireworks` | Fireworks AI | `https://api.fireworks.ai/inference/v1` |
| `/opencode-zen` | OpenCode Zen | `https://opencode.ai/zen/v1` |
| `/grok` | xAI Grok | `https://api.x.ai/v1` |
| `/qwen` | Model Studio Coding Plan | `https://coding-intl.dashscope.aliyuncs.com/v1` |
| `/qwen-token` | Model Studio Token Plan | `https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1` |
| `/qwen-anthropic` | Token Plan, Anthropic protocol | `https://token-plan.ap-southeast-1.maas.aliyuncs.com/apps/anthropic` |

Each has a `--<name>-upstream` flag for a different region or a gateway; the
Anthropic-protocol Token Plan route uses `--qwen-anthropic-upstream`. Add a
key with the provider named, or it is stored as a Codex account and forwarded to
OpenAI:

```bash
sr add-key --provider qwen-token
```

Aliases are accepted where a provider is named: `glm` for Z.AI, `xai` for Grok,
`dashscope` for the Coding Plan, `tokenplan` for the Token Plan, `together-ai`
for Together, `fireworks-ai` for Fireworks, and `zen` for OpenCode Zen.

DeepSeek, Together, Fireworks, and OpenCode Zen use the same account lifecycle
as the other API-key providers. Add more than one key under the same provider
to enable sticky-session failover when a key returns 429:

```bash
sr add-key --provider deepseek
sr add-key --provider together
sr add-key --provider fireworks
sr add-key --provider opencode-zen
```

Their client base URLs are respectively `/deepseek/v1`, `/together/v1`,
`/fireworks/v1`, and `/opencode-zen/v1` beneath the Subrouter origin. Cursor
and GitHub Copilot subscription credentials are not included in this API-key
support. Cursor's public API manages Cloud Agents rather than exposing raw
inference, and its CLI has its own authentication flow
([API](https://cursor.com/docs/cloud-agent/api/endpoints),
[CLI auth](https://docs.cursor.com/en/cli/reference/authentication)). The
Copilot SDK speaks JSON-RPC to a Copilot CLI agent/session server rather than an
OpenAI-compatible subscription endpoint
([compatibility](https://docs.github.com/en/copilot/how-tos/copilot-sdk/troubleshooting/compatibility),
[authentication](https://docs.github.com/en/copilot/how-tos/copilot-sdk/auth/authenticate),
[SDK](https://github.com/github/copilot-sdk)). Subrouter therefore does not
claim it can import or fail over either product's subscription.

`sr status` groups these under their own provider rather than under Codex. Kimi
OAuth subscriptions report their independent 5-hour and weekly windows and
reset times from Kimi's usage endpoint. The condensed API-key rows report key
health and only quota data the provider actually exposes.

The Antigravity CLI exposes one fixed Keychain login and no account selector.
See [the native AGY runbook](docs/antigravity.md) for the safe profile and
acceptance procedure.
Subrouter turns that slot into an explicit import source: sign plain `agy` into
an account, run `sr agy add <label>`, and repeat for each account. Each import
is validated by an OAuth refresh and stored as an isolated, independently
refreshable router profile; the vendor Keychain item is never changed by import.
`sr agy` launches native AGY using a pooled local profile, while
`sr agy --account <label-or-email>` pins one process. Direct
`GEMINI_API_KEY` and Application Default Credentials are separate supported
authentication paths, not additional selectable OAuth profiles
([install](https://antigravity.google/docs/cli/install/),
[headless auth](https://antigravity.google/docs/cli/headless/),
[enterprise](https://antigravity.google/docs/enterprise/)).

Plain `agy` remains the vendor's supported direct CLI. The current AGY release
does not expose a reliable transparent proxy hook: explicitly setting its only
endpoint override changes vendor behavior even when that override names
Google's normal endpoint. Native `sr agy` therefore uses process-scoped
Keychain profile switching rather than endpoint rewriting. The selected
identity is verified after switching, and the prior slot is restored on exit;
pooled selection applies between launches, never by changing an existing
process. Only the explicit `sr agy add` command transfers a validated
credential to the selected self-hosted router through its protected
account-import endpoint. `sr status` reports each managed profile as
`ready`, `active`, or `error`. When Google's read-only OAuth services expose
telemetry for that exact profile, it also reports the provider-verified email
and plan plus independent Gemini and Claude/GPT quota pools. Named 5-hour and
weekly buckets retain their own remaining percentage and reset; missing or
disabled buckets stay unknown, and one exhausted family never collapses the
other. Older accounts that expose only per-model quota retain each exact model
as its own routing pool; compact Use names the most constrained known model
without guessing it into a 5-hour or weekly cadence. Telemetry is
bounded and account-specific; Subrouter does not scrape the AGY TUI or attach a
managed profile to an unrelated host language-server login. The server adapter
retains isolated account selection, family-aware scheduling, OAuth refresh, and
hard-pin semantics for compatible clients. Plain `agy` uses the current
Keychain login directly. If the host or process is hard-killed during a native
launch, rerun `sr agy recover` before launching again; the swap journal restores
the prior Keychain slot without touching the live server.

For backward compatibility, a router with no managed Antigravity profiles
continues serving its historical host Keychain login. The first successful
`sr agy add` retires that singleton from new routing and makes the explicit
managed inventory authoritative; this avoids adding the same refresh chain to
the pool twice.

Subrouter rejects the same refresh grant under two labels and preserves that
grant identity when Google rotates the stored token. It deliberately does not
trust unsigned JWT claims to decide account identity. Two independently
authorized grants for the same Google user cannot be proven equivalent from
AGY's credential format and may therefore be stored under distinct labels.

Native proxy launchers preflight the selected router before starting the vendor
CLI. Hosted or otherwise lease-required routers are rejected until Subrouter has
a native-launcher session-lease client; local and ordinary self-hosted routers
remain supported. New sessions keep provider-and-working-directory affinity.
Kimi's workspace-relative `--continue` operations keep that same affinity;
Qwen deliberately rejects continue and resume operations, as detailed below.
An explicit Kimi session ID instead keeps provider-and-session affinity across
working directories, without reading vendor session files.

Kimi's CLI owns one global OAuth login, while Subrouter can keep additional
subscription logins in isolated profiles without switching or rewriting that
global credential. The global CLI login appears in `sr kimi list` as
`not routed`; `sr status` shows only independently authorized profiles that the
proxy can safely refresh. This prevents a serving daemon from redeeming the
CLI's rotating refresh-token chain and silently signing the interactive client
out. Kimi does not expose the account email, so give each managed profile a
recognizable local label:

```bash
sr kimi login work
sr kimi login personal
sr kimi list
sr kimi remove personal
sr kimi -p 'Summarize the current changes'
sr kimi --account work -p 'Review this workspace'
sr kimi --session <session-id> -p 'Continue the routed session'
```

The labels are the management and status names; Subrouter does not infer an
email or account name from undocumented token contents. Each profile refreshes
atomically and is independently schedulable; `active` means a persistent session
is assigned, `rec` is the next eligible profile, and `ready` means authenticated
with quota but currently idle.
Plain `kimi` remains direct and interactive. Each routed `sr kimi -p` launch
gets a fresh private `KIMI_CODE_HOME` containing only a routed model config, so
Kimi does not automatically load the user's provider catalog, OAuth token, or
other global Kimi settings. This is configuration isolation, not an OS sandbox:
the child still runs as the user and has the user's ordinary filesystem access.
The child links only the validated `sessions/` directory and
`session_index.jsonl`, so existing and newly written sessions remain resumable
without copying or rewriting the rest of `~/.kimi-code`. Cleanup removes only
the child-local tree and never follows those links. Routed Kimi uses the common
`kimi-for-coding` model with a 262144-token context for both managed OAuth and
verified coding-key accounts, forces the same routed model for subagents, and
disables auto-update for the child. Credential/provider control,
migration/update, and long-lived server modes (`login`, `provider`, `migrate`,
`upgrade`, `update`, `acp`, `web`, and `server`) are rejected;
use plain `kimi` for those direct operations.
Kimi 0.39.0 registers credential/provider and server-launching slash commands
inside the interactive TUI and exposes no supported config or environment
denylist for them. Routed interactive launches therefore fail closed. Prompt
mode does not start that TUI dispatcher, so `-p` remains supported for new
sessions and with an explicit `--session`/`--resume` ID or `--continue`.
New sessions and `--continue` use the working-directory assignment; an explicit
session ID uses the same pooled assignment from any working directory.
This session-link boundary currently requires macOS or Linux; `sr kimi` fails
closed on Windows while plain `kimi` remains available there.
The default routed prompt is pooled and may fail over. `sr kimi --account work -p ...` pins
that child to the exact routed profile or key with no account failover; bare
`--account` opens a pinned-account picker. This per-launch pin does not change
the global recommendation or the corresponding pooled session assignment. `sr kimi
proxy` remains an explicit alias. Put
vendor arguments after `--` when they could be confused with wrapper options.
Kimi Code subscription API keys can also be added with
`sr add-key --provider kimi`. Kimi documents that every device and API key under
one membership shares the same quota, so extra keys are failover credentials,
not additional quota pools. The current API-key response does not expose a
verified membership owner identity; use distinct labels and do not assume two
keys represent separate subscriptions.

For each Qwen Token Plan account, authorize the Alibaba console once:

```bash
sr qwen login --console-account you@example.com qwen-token:large-plan
```

Subrouter supplies the selected key to Bailian CLI, opens the international
Alibaba browser flow, and stores the resulting console credential in an
account-isolated profile. The optional sign-in label becomes the account name
because Alibaba's console API exposes a stable billing instance ID but not the
login email. If two keys share that label, their saved labels are appended to
keep the rows distinct. Browser approval is the only manual step. Afterwards
`sr status` reports the vendor-owned Lite/Pro plan and every rolling-window
percentage and reset time Alibaba actually returns. A window omitted by Alibaba
is omitted from the table rather than displayed as empty or inferred. `auth ok` means
the vendor accepted that key for its authenticated model-list endpoint; it does
not claim that a generation was spent or that quota remains. Other vendors
remain `not exposed` when no quota API is available.

The console credential is used only for optional quota telemetry and currently
contains an Alibaba access token, not a refresh-token chain. If Alibaba returns
`BailianGateway.Login.NotLogined`, routing with the stored model key remains
valid while the status row says `login needed`; repeat `sr qwen login` for that
account to restore telemetry. This is separate from model-key health and does
not disable the account for routing.

Store multiple Qwen accounts with distinct labels; each key remains a separate
schedulable account while the Token Plan's two protocol routes share that pool:

```bash
sr add-key --provider qwen-token  # enter label "large-plan"
sr add-key --provider qwen-token  # enter label "small-plan"
```

Those saved labels remain the unique management identifiers; reusing one
updates that account instead of creating a second one. `sr status` normally
shows the friendlier console login, while list/remove commands use the same
provider-qualified saved labels as other Subrouter accounts:

```bash
sr list
```

Adding a key targets the selected serving server. Removing an account from a
selected server is not implemented yet; `sr remove` is only available when the
operator explicitly binds the command to the exact local state with
`SUBROUTER_STATE_DIR`. This avoids reporting success after editing a different
store from the running daemon.

Launch Qwen Code through the selected Token Plan pool with:

```bash
sr qwen
sr qwen --model qwen3.8-max
sr qwen --account large-plan
```

Plain `qwen` remains direct. The proxy launcher keeps the normal Qwen session
store but forces Qwen's `--bare` mode for the routed child, so saved settings,
extensions, skills, and MCP servers are not loaded. This is stronger than
setting `OPENAI_BASE_URL`: Qwen deep-merges arbitrary custom `modelProviders`
catalogs through `providerProtocol` and has separate fast, advisor, vision,
compaction, image, voice, and fallback selectors. Bare mode removes that saved
catalog and selector surface; the routed launcher also rejects
`--fallback-model` and `--proxy` and disables the in-session `/auth` and
`/model` provider switches. Qwen can restore a recorded provider when a session
resumes, including its built-in OAuth catalog, so `sr qwen` rejects `--continue`
and `--resume`; use a new routed session or plain `qwen` for that existing
direct session. Direct Alibaba keys are replaced with non-secret process
sentinels so Qwen cannot restore them from `.env`. The process overlay is
removed when Qwen exits and contains no real plan key. If Qwen system-policy
files or their path environment variables are present, the launcher fails
closed instead of hiding administrator policy. Qwen's `serve`, ACP, `review`, and
model-bearing channel-service modes can reload saved environment routing while
they run, and container sandbox relaunches cannot reach the loopback relay or
retain all routing guards. The routed wrapper therefore refuses those modes,
container sandbox flags, and provider-qualified model selectors; use plain
`qwen` when that direct behavior is required.
When Alibaba returns a 429 for one plan account, Subrouter replays the
generation request with another stored Qwen account and moves that sticky
session to the successful account. A launch with `--account` instead pins the
exact plan account and returns that account's error rather than failing over;
bare `--account` opens a clearly labeled pinned picker. `sr qwen proxy` remains
an explicit alias. Account-scoped pinned session identities keep parallel pins
from replacing the pooled working-directory assignment.

Standalone local status probes use the documented vendor default upstream;
custom serving upstreams are not persisted into the CLI configuration.

To put a new provider on an already-running macOS daemon, follow
[docs/upgrades.md](docs/upgrades.md#replacing-the-binary-in-place-on-macos).
Replacing a live executable with `cp` invalidates its code signature and macOS
kills every respawn.

### Two protocols against one subscription

Some vendors serve the same subscription over both the OpenAI and the Anthropic
wire protocol. Alibaba's Token Plan is the current example, which is why it has
two entries: `/qwen-token` speaks OpenAI-compatible JSON, and
`/qwen-anthropic` speaks Anthropic Messages. Subrouter forwards bodies
unchanged, so an Anthropic-shaped client can run on that subscription without
any translation in the proxy — the vendor does the adaptation:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:31415/qwen-anthropic \
  ANTHROPIC_AUTH_TOKEN=subrouter claude
```

The two entries differ in one detail worth knowing if you add a provider like
this. The OpenAI base already ends in `/v1`, so a client's own `/v1` is
collapsed to avoid `/v1/v1`. The Anthropic base stops at `/apps/anthropic` and
the client appends `/v1/messages` itself, so there the version segment is
preserved — collapsing it produces the duplicated path the vendor documents as a
404.

### Declaring a provider without a release

A provider whose only distinguishing feature is its base URL — a subscription
plan on its own host, a self-hosted gateway, a relay — does not need code:

```bash
sr serve --openai-compatible acme=https://gateway.acme.test/v1
sr serve --openai-compatible 'acme|acme-relay=https://gateway.acme.test/v1'
```

A declared provider gets the same routing, auth, lease, import, and CLI handling
as a built-in one. A standalone local or remote `sr add-key --provider <name>`
carries the validated custom name to the serving process, which accepts the
account only when that process declared the provider. Declarations are rejected if they claim a name or path
segment that Codex, Claude, or a built-in provider already routes on, since that
would silently redirect traffic which already had a home. Providers are read on
every request and declared once at startup, so they cannot change while the
server is serving.

## Multi-tenant mode

One hosted Subrouter can serve many isolated users, each with their own account pool under `<state-dir>/tenants/<id>/` (same layout as the single-tenant state dir). `sr tenant create <name>` registers a tenant against a named server's admin API (or the local state dir on the server host) and prints an `srt_<32 hex>` key once; only its SHA-256 hash is stored in `tenants.json`.

Clients authenticate by base URL prefix, because agent CLIs can only override base URLs: point Codex at `https://host/t/<key>/v1` and Claude Code at `ANTHROPIC_BASE_URL=https://host/t/<key>`. The key is also accepted as a Bearer token or `x-api-key` header (Claude Code's `ANTHROPIC_AUTH_TOKEN` lands there). Account selection, sticky sessions, usage scoring, and transcripts are all scoped to the tenant's pool; an unknown or revoked key gets a 401. Requests without a tenant key keep the legacy single-tenant behavior.

`sr server add <name> --url <url> --tenant-key srt_...` stores the key on a server entry, after which `sr codex`, `sr claude push`, `sr server login/sync`, and the status commands operate on that tenant's pool automatically. Tenant CRUD lives on the admin-gated `/_subrouter/tenants` endpoints; tenant-scoped reads and account import (`/t/<key>/_subrouter/{accounts,account-status,usage-status,sessions,account-import}`) are authorized by the tenant key itself.

## Selection policy

On startup, Subrouter fetches current Codex usage for OAuth accounts and scores each account by its most constrained usage window. The scheduler keeps existing sessions sticky. For a new session it protects low-headroom accounts, spends healthy quota that resets soonest, then breaks ties by live assigned-session counts.
If all else ties, subscription OAuth accounts are preferred before API-key accounts.

The daemon refreshes usage scores used for routing every 10 minutes by default. It never rewrites interactive Codex, OpenCode, or pi auth; only an explicit account-manager command such as `sr switch` does that. Configure score refresh with `subrouter serve --sr-switch-interval 5m`, or disable it with `--sr-switch-interval 0`. If `--fetch-usage=false`, scheduled score refresh is disabled because fresh usage is required.

By default, OAuth accounts are forwarded to `https://chatgpt.com/backend-api/codex` and API-key accounts are forwarded to `https://api.openai.com`. Subrouter accepts either `/v1/responses` or `/responses` from clients and normalizes the path for the selected account type.

Live headroom comes from Codex subscription usage. API-key spend comes from the OpenAI organization usage endpoints through stored `sk-admin-*` keys. Claude profile usage comes from the Anthropic OAuth usage endpoint when profile credentials are readable.

See [docs/saturation.md](docs/saturation.md) for the 5h/7d placement strategy and simulation tests.

## Azure fallback for Codex

When the Codex pool cannot serve a Responses request, Subrouter can finish it on Azure OpenAI instead of returning the error. The pool stays primary: Azure runs only after the request has spent five pool retries (account failover and transport retries draw on one shared budget), or when account selection fails outright. Quota (429), broken credentials (401/403), upstream faults (408/5xx), and transport failures qualify; a 400 does not, because Azure would reject it the same way for money.

After Azure answers, the session is pinned to that endpoint for 30 minutes of activity, and the following turns skip the pool entirely. The pin is written to `azure-codex-pins.json` beside the session store, so a restart does not strand a thread whose history only Azure can read. Session identity is the Codex thread id, not its window index: `X-Codex-Window-ID` arrives as `<thread>:<window>`, the window number changes while the conversation does not, and treating each window as a new session reset both the pin and the account stickiness. Prompt caching is per-deployment and keyed on an identical prefix, so alternating between ChatGPT and Azure turn by turn would re-upload the whole conversation as a cache miss on both sides. Subrouter forwards the `prompt_cache_key` Codex sends, and derives a stable one from the session when it is absent. With several endpoints configured, a session hashes to one of them and stays there.

One Azure resource:

```bash
export SUBROUTER_AZURE_CODEX_ENDPOINT="https://YOUR_RESOURCE.openai.azure.com/openai/v1"
export SUBROUTER_AZURE_CODEX_API_KEY="<azure-key>"          # or SUBROUTER_AZURE_CODEX_API_KEY_FILE
export SUBROUTER_AZURE_CODEX_DEPLOYMENTS="gpt-5.6-codex=my-codex-deployment"   # only if the deployment name differs from the model
export SUBROUTER_AZURE_CODEX_MODELS="gpt-5.6*"   # optional allow list; empty serves every model
```

Several resources go in a file named by `SUBROUTER_AZURE_CODEX_CONFIG_FILE`:

```json
{"models":["gpt-5.6*"],
 "endpoints":[
  {"name":"eastus2","base_url":"https://east.openai.azure.com/openai/v1","api_key":"..."},
  {"name":"openai","base_url":"https://api.openai.com/v1","api_key_file":"/var/lib/subrouter/openai-api-key"}
]}
```

An endpoint may be `https://api.openai.com/v1` with an OpenAI API key: it speaks the same Responses surface with real model names, so it backs the pool when Azure itself is out. `api_key_file` reads the key from a file instead of carrying it inline. `models` limits which requested models the fallback serves; an entry matches exactly or as a prefix when it ends with `*`. A model outside the list keeps the pool's own failure, because paying a metered provider to answer with a different model than the one requested is worse.

`SUBROUTER_AZURE_CODEX_DISABLED=1` switches the whole route off while leaving the endpoint and its key in place, so turning it back on is one environment variable rather than a credential to find again. The switch wins over every other Azure setting, and the daemon says so at startup, because a route that is off looks identical to one that was never set up right up until the pool runs out of quota. Sessions already pinned to Azure fall back to the pool, which drops the reasoning Azure sealed and retries, so they keep working rather than stranding.

Set `SUBROUTER_AZURE_CODEX_DEFAULT_DEPLOYMENT` to catch models Azure has not shipped yet. Azure trails the ChatGPT model list, and the request that needs the fallback is the one the pool refused, so an unmapped model lands on that deployment instead of 404ing. Prefer the `models` allow list plus same-named deployments when Azure does host the requested models, so the fallback never answers with a silently different model.

The fallback also catches turn failures that arrive inside an otherwise-200 stream rather than as a status code, on both transports. Failures classify by who can fix them. Client-caused ones (context length, invalid prompt, policy, unsupported parameters) pass through untouched, because every provider refuses them the same way. Quota failures (`usage_limit_reached`, `insufficient_quota`, `usage_not_included`) mark the account exhausted like a 429 would; on SSE the turn is then served from Azure, and on WebSocket the connection closes with 1012 so the reconnect lands on another pool account, falling to Azure only when nothing in the pool can start it. Everything else, including any failure code this proxy has never seen, is provider-side by default and is absorbed: Codex treats an unrecognized `response.failed` as the end of the turn (`server_is_overloaded` renders as "Selected model is at capacity. Please try a different model."), so an unknown code forwarded is a turn lost, while an unknown code absorbed costs one Azure attempt and falls back to the original error if Azure refuses it. On SSE the stream is sniffed before any byte reaches the client; on WebSocket the event pins the session to Azure, closes with 1012, and the reconnect is refused with 426, the one status Codex answers by switching to the HTTP transport, where the pin serves the turn.

Prove the route without waiting for an outage:

```bash
sr az status          # which endpoints the daemon armed
sr az test            # one forced request; run twice, the second reports cached tokens
sr az cost            # what the fallback has spent
sr az codex exec "…"  # run Codex with every request forced onto Azure
```

`sr az test` sends a fixed prompt long enough to be cacheable, so the second run's `cached=` count is real evidence that the prompt cache is being reused. Forced requests skip the pool and never pin the session; a broken endpoint surfaces as an error instead of a silent ChatGPT answer.

Codex bodies carry fields the Responses API does not take (`session_id` on every turn), and a Codex release can send a value an older Azure model version refuses (`reasoning.context="all_turns"`). Known ChatGPT-only fields are stripped, and a 400 that names one field is retried once without it, up to three times. That includes a settings key inside a single input item (`input[39].namespace`), but never a whole turn or the keys that carry one (`content`, `role`, `arguments`, and the like): dropping those would quietly send a different conversation. Reasoning moves freely inside OpenAI and not at all across providers. Measured on gpt-5.6-sol: a reasoning blob produced by a ChatGPT subscription account is accepted by any other subscription account and by an OpenAI API key, and the reverse holds; Azure refuses every OpenAI blob and OpenAI refuses every Azure blob. So subscription-to-subscription and subscription-to-API-key moves are lossless, and only the Azure boundary costs the model's private reasoning. Both directions of that boundary are repaired the same way: the sealed items are dropped and the turn is retried, on the Azure route going in and on the pool route coming back.

A conversation that starts on the ChatGPT pool and then falls back carries reasoning blobs sealed by the provider that made them, and Azure answers `invalid_encrypted_content` with no field named. Those blobs are stripped and the turn is retried, keeping the user's messages, tool results, and any readable reasoning summary. The turn loses the model's earlier private reasoning trace, which is the cost of crossing providers mid-conversation, and it is the difference between a served answer and a hard failure. Each rejection is remembered per endpoint and deployment for six hours, so a long session does not re-upload its whole conversation every turn to rediscover the same refusal, and an Azure model upgrade that starts accepting the field is picked up on its own.

Azure is metered, unlike the subscription pool, so every served request is priced into `azure-codex-cost.jsonl` next to the session store and summarized at `/_subrouter/azure-codex-cost`. `sr az cost` prints it, and `sr` status grows a spend line once the fallback has run. Cached input is billed at the cached rate rather than the full one, and a model with no price entry contributes zero rather than a guess.

`curl -s http://127.0.0.1:31415/_subrouter/health` lists the armed endpoints by name under `azure_codex`. Only `/responses` falls back; `/responses/compact`, the model catalog, and `/alpha/search` are ChatGPT-backend endpoints with no Azure equivalent. Team credential storage (`sr storage team`) refuses this fallback, like the other personal-credential routes.

## Security defaults

- Bind to `127.0.0.1` unless explicitly exposed.
- Do not log tokens, refresh tokens, API keys, request bodies, or full Authorization headers.
- Keep Subrouter-managed credentials under `~/.subrouter/codex` locally and `/var/lib/subrouter/codex` on systemd servers.
