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

The daemon uploads with the GCS JSON API on a background interval. Local transcript writes stay on the request path; GCS upload failures are logged and retried later. Local cleanup only runs after a successful GCS sync. Files selected for cleanup are copied to an immutable `_archive/` object before local deletion so future resumed sessions cannot overwrite the only cloud copy.

For best cache behavior, clients should send a stable header per conversation:

```text
X-Subrouter-Session: <conversation-or-thread-id>
```

If that header is missing, Subrouter checks Codex headers such as `x-codex-window-id` and `x-codex-turn-state`, common session headers, query params, and small JSON bodies for `session_id`, `conversation_id`, or `thread_id`.

Subrouter scopes sticky assignments and transcript files by agent type. It infers `codex`, `claude`, or `gemini` from provider session headers, and clients can set an explicit namespace:

```text
X-Subrouter-Agent: codex
```

For teammate-level graphs, clients can also send a self-reported user header:

```text
X-Subrouter-User-Email: alice@example.com
```

Subrouter stores the normalized email on the session assignment, includes it in proxy logs as `user`, and exposes it in `GET /_subrouter/sessions`. This is observability metadata, not authentication. To force a selected account, send `X-Subrouter-Account-ID`; API-key labels can omit the `apikey:` prefix. Subrouter strips `X-Subrouter-Session`, `X-Subrouter-Agent`, `X-Subrouter-User-Email`, `X-Subrouter-User`, `X-User-Email`, `X-Subrouter-Account-ID`, and `X-Subrouter-Account` before forwarding upstream.

## Codex CLI

`subrouter codex` is a direct Codex wrapper. Use it anywhere you would use `codex`:

```bash
subrouter codex
subrouter codex exec "your prompt"
subrouter codex --version
```

The wrapper injects this config override into the child Codex process:

```toml
openai_base_url = "http://127.0.0.1:31415/v1"
```

It does not edit Codex config or set auth environment variables. Do not set a dummy `OPENAI_API_KEY` for normal subscription routing. Leave Codex logged in the same way it already is. If Codex is in ChatGPT auth mode, `/model` keeps the subscription model picker. Subrouter replaces outbound credentials with the selected `sr` account before forwarding. Responses and realtime WebSocket requests are proxied through the same route.

Override the subrouter URL with `SUBROUTER_CODEX_BASE_URL` if needed. See [docs/codex.md](docs/codex.md) for details and the custom-provider fallback.

Run Codex against the Azure OpenAI GPT-5.6 family with Microsoft Entra credentials from Azure CLI:

```bash
az login
sr azure add work --endpoint https://RESOURCE.cognitiveservices.azure.com
sr az codex
```

The first Azure profile becomes the default, so `sr az codex` needs no profile argument. Use `/model` inside Codex to switch among Sol, Terra, and Luna, `sr az default <profile>` to change the saved default profile, or `--azure-profile <profile>` for one run. `sr azure` is canonical and `sr az` is its short alias. A profile defaults to deployments named `gpt-5.6-sol`, `gpt-5.6-terra`, and `gpt-5.6-luna`; repeat `--deployment model=name` during `add` when Azure uses custom deployment names. Subrouter keeps Codex's canonical model names in the picker and translates them to Azure deployment names at the proxy boundary. It stores only profile metadata, acquires renewable access tokens through `az account get-access-token`, keeps them in daemon memory, and routes the launcher through the explicit local `/azure/work/v1` path. See [Azure OpenAI setup and security details](docs/codex.md#azure-openai).

If `SUBROUTER_CODEX_BASE_URL` is not set, the wrapper uses local `127.0.0.1:31415/v1`. To make `sr codex`, Codex Desktop's app-server, and the default `sr` usage view use a remote Subrouter, register and select a named server:

```bash
sr server add team --url http://100.64.0.1:31415 --default
```

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

When either variable is set, the wrapper uses a custom `subrouter` provider with WebSockets enabled so Codex can send `X-Subrouter-User-Email` and `X-Subrouter-Account-ID`. Subrouter still replaces outbound credentials before forwarding upstream. `SUBROUTER_CODEX_USER_EMAIL` is only teammate observability metadata; account selection belongs in `SUBROUTER_CODEX_ACCOUNT_ID`.

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

The supported Codex commands include `add`, `add-key`, `import`, `list`, `switch`, `g`, `gui`, `gui-switch`, `remove`, `status`, `usage`, `server`, `azure`, `add-admin-key`, `admin-keys`, `remove-admin-key`, `attach-project`, `claude`, and `gemini`. The older `subrouter cx <command>` form remains as a compatibility alias.

`sr switch` also syncs compatible ChatGPT Codex credentials into:

```text
~/.codex/auth.json
~/.local/share/opencode/auth.json      # provider key: openai
~/.pi/agent/auth.json                  # provider key: openai-codex
```

OpenCode uses XDG data home, so `XDG_DATA_HOME` changes its auth path. pi uses `PI_CODING_AGENT_DIR` when set. Existing unrelated provider credentials in those files are preserved.

Claude profiles are also native Go and use the same Subrouter store:

```bash
sr claude list
sr claude switch <profile>
sr claude env
sr claude run <profile>
```

Claude Code can also proxy through Subrouter with Claude Code OAuth tokens. Generate a long-lived token with `claude setup-token`, then configure the Claude user settings env:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:31415",
    "CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-...",
    "ANTHROPIC_AUTH_TOKEN": "sk-ant-oat01-..."
  }
}
```

For a shared server, replace `127.0.0.1` with the server URL. Subrouter recognizes Claude Code traffic, selects a Claude OAuth account from its own store, strips API-key auth, and forwards to Anthropic with the OAuth beta header. Claude Code prompt caching does not require Subrouter-specific cache settings: Subrouter keeps the same Claude conversation pinned to the same Claude account when that account is still available, and forwards the client `Anthropic-Beta` values and request body `cache_control` blocks unchanged.

Gemini has its own `sr gemini` namespace and store scaffold so future routing cannot collide with Codex or Claude state.

## Multi-tenant mode

One hosted Subrouter can serve many isolated users, each with their own account pool under `<state-dir>/tenants/<id>/` (same layout as the single-tenant state dir). `sr tenant create <name>` registers a tenant against a named server's admin API (or the local state dir on the server host) and prints an `srt_<32 hex>` key once; only its SHA-256 hash is stored in `tenants.json`.

Clients authenticate by base URL prefix, because agent CLIs can only override base URLs: point Codex at `https://host/t/<key>/v1` and Claude Code at `ANTHROPIC_BASE_URL=https://host/t/<key>`. The key is also accepted as a Bearer token or `x-api-key` header (Claude Code's `ANTHROPIC_AUTH_TOKEN` lands there). Account selection, sticky sessions, usage scoring, and transcripts are all scoped to the tenant's pool; an unknown or revoked key gets a 401. Requests without a tenant key keep the legacy single-tenant behavior.

`sr server add <name> --url <url> --tenant-key srt_...` stores the key on a server entry, after which `sr codex`, `sr claude push`, `sr server login/sync`, and the status commands operate on that tenant's pool automatically. Tenant CRUD lives on the admin-gated `/_subrouter/tenants` endpoints; tenant-scoped reads and account import (`/t/<key>/_subrouter/{accounts,account-status,usage-status,sessions,account-import}`) are authorized by the tenant key itself.

## Selection policy

On startup, Subrouter fetches current Codex usage for OAuth accounts and scores each account by its most constrained usage window. The scheduler keeps existing sessions sticky. For a new session it protects low-headroom accounts, spends healthy quota that resets soonest, then breaks ties by live assigned-session counts.
If all else ties, subscription OAuth accounts are preferred before API-key accounts.

The daemon also refreshes usage and updates Codex, OpenCode, and pi auth every 10 minutes by default so local agents follow the same OAuth-only policy. Configure it with `subrouter serve --sr-switch-interval 5m`, or disable it with `--sr-switch-interval 0`. If `--fetch-usage=false`, auto-switch is disabled because fresh usage is required.

By default, OAuth accounts are forwarded to `https://chatgpt.com/backend-api/codex` and API-key accounts are forwarded to `https://api.openai.com`. Subrouter accepts either `/v1/responses` or `/responses` from clients and normalizes the path for the selected account type.

Live headroom comes from Codex subscription usage. API-key spend comes from the OpenAI organization usage endpoints through stored `sk-admin-*` keys. Claude profile usage comes from the Anthropic OAuth usage endpoint when profile credentials are readable.

See [docs/saturation.md](docs/saturation.md) for the 5h/7d placement strategy and simulation tests.

## Security defaults

- Bind to `127.0.0.1` unless explicitly exposed.
- Do not log tokens, refresh tokens, API keys, request bodies, or full Authorization headers.
- Keep Subrouter-managed credentials under `~/.subrouter/codex` locally and `/var/lib/subrouter/codex` on systemd servers.
