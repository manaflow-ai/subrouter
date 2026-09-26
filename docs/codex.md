# Codex CLI Integration

Current Codex does not use an `OPENAI_BASE_URL` environment variable for the built-in OpenAI provider. Use the Subrouter wrapper, or let `sr server use <name>` write the Codex config file.

## Recommended

Use `subrouter codex` anywhere you would use `codex`:

```bash
subrouter codex
subrouter codex exec "your prompt"
subrouter codex resume --last
subrouter codex --version
```

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

Subrouter supports Codex WebSocket requests, so the custom provider keeps the
normal transport behavior.
This includes Responses WebSockets at `/v1/responses` and realtime WebSockets at `/v1/realtime`.

Do not set a dummy `OPENAI_API_KEY`. The wrapper's non-secret local-hop token
decouples the client from `~/.codex/auth.json`, so a local ChatGPT logout or
refresh failure cannot stop the request before proxy-side failover runs.
Subrouter replaces the outbound Authorization and `ChatGPT-Account-ID` headers
with the selected `sr` account. Resume with `sr codex resume ...`; a bare
`codex resume ...` does not recreate wrapper-only overrides.

The two launch modes are intentionally independent: plain `codex` uses Codex's
normal direct OpenAI configuration, while `sr codex` opts that process into the
Subrouter pool. The launcher strips older Subrouter-owned `-c` routing values
and local-provider `--oss` settings from copied or saved commands before adding
the current provider settings, so another provider or a retired URL cannot
override the selected server by argument precedence. It
also exports `SUBROUTER_CODEX_LAUNCHER="<launcher> codex"` and
`SUBROUTER_CODEX_RESUME_COMMAND="<launcher> codex resume"` for session managers
that persist a resume command. The launcher name follows the invoked `sr`,
`subrouter`, or `cx` alias; unrelated clients can ignore these variables.

## Server Switching

Register and select a remote server:

```bash
sr server add team --url http://100.64.0.1:31415 --default
sr server use team
```

Both commands write the selected routing defaults to `CODEX_HOME/config.toml`, or `~/.codex/config.toml` when `CODEX_HOME` is unset:

```toml
openai_base_url = "http://100.64.0.1:31415/v1"
chatgpt_base_url = "http://100.64.0.1:31415/backend-api"
experimental_realtime_ws_base_url = "http://100.64.0.1:31415/v1"
```

Use `--no-codex-config` to change only Subrouter's selected server. Use `sr server use local` or `sr server clear-default` to restore local routing. When a remote server is selected, bare `sr` and `sr status` render that server's usage table.

## Codex Desktop

Codex Desktop has two outbound paths:

- The Rust `app-server` sends model traffic, including Responses WebSockets. It reads `CODEX_HOME/config.toml`, so `openai_base_url = "http://127.0.0.1:31415/v1"` routes that traffic through Subrouter.
- The same `app-server` reads account usage and sends add-credit nudges through `chatgpt_base_url`. Set `chatgpt_base_url = "http://127.0.0.1:31415/backend-api"` if Desktop UI state should reflect the selected Subrouter OAuth account.
- The Electron shell sends ChatGPT backend requests with `electron.net.fetch`. It reads `CODEX_API_BASE_URL` at process start. Use `CODEX_API_BASE_URL=http://127.0.0.1:31415/backend-api` so Electron requests for `/codex/...` reach Subrouter as `/backend-api/codex/...`.

`subrouter codex app` intentionally does not inject routing flags. Current Codex accepts `-c` on `codex app`, but the app launcher opens the installed desktop app through the OS and does not carry those overrides into the running app-server. To test desktop routing, start Codex Desktop in an isolated launch environment with the env var above and an isolated `CODEX_HOME` containing the config overrides. Subrouter maps `/backend-api/...` back to the normal ChatGPT backend for OAuth accounts and will not route those backend paths through API-key accounts.

Optional realtime voice routing can be pinned explicitly in the same config:

```toml
experimental_realtime_ws_base_url = "http://127.0.0.1:31415/v1"
```

## User Attribution

Use `SUBROUTER_CODEX_USER_EMAIL` when a teammate should be visible in Subrouter logs and session data:

```bash
SUBROUTER_CODEX_USER_EMAIL=alice@example.com subrouter codex exec "your prompt"
```

Use `SUBROUTER_CODEX_ACCOUNT_ID` when a run should use one explicit Subrouter account, including an API-key account:

```bash
SUBROUTER_CODEX_ACCOUNT_ID=team-codex-1 subrouter codex exec "your prompt"
SUBROUTER_CODEX_ACCOUNT_ID=apikey:team-codex-1 subrouter codex exec "your prompt"
```

`subrouter codex` always uses the custom `subrouter` provider with WebSockets
enabled and sends `X-Subrouter-Agent: codex`. These variables add
`X-Subrouter-User-Email` and/or `X-Subrouter-Account-ID`. Subrouter still
replaces outbound credentials before forwarding upstream.

## Models

There are two separate Codex concepts:

- `model`: the model slug selected by `/model`.
- `model_provider`: the backend/provider config.

Subrouter does not rewrite the `model` field. The wrapper sets
`model_provider = "subrouter"` so local ChatGPT authentication cannot fail
before the proxy sees the request.

Subrouter accepts `/v1/responses` and `/responses`. For OAuth subscription accounts it forwards to `https://chatgpt.com/backend-api/codex` and strips the `/v1` prefix when present. For API-key accounts it forwards to `https://api.openai.com` and adds `/v1` when needed.

## Manual Provider Configuration

If WebSocket support needs to be disabled for debugging, use a custom provider:

```bash
codex exec \
  -c 'model_provider="subrouter"' \
  -c 'model_providers.subrouter.name="Subrouter"' \
  -c 'model_providers.subrouter.base_url="http://127.0.0.1:31415/v1"' \
  -c 'model_providers.subrouter.experimental_bearer_token="subrouter"' \
  -c 'model_providers.subrouter.wire_api="responses"' \
  -c 'model_providers.subrouter.supports_websockets=false' \
  "your prompt"
```

## Env Vars

- `SUBROUTER_CODEX_BASE_URL`: base URL injected by `subrouter codex`; defaults to `http://127.0.0.1:31415/v1`.
- `SUBROUTER_SERVER`: named server from `sr server add` (or `local`) for this one command; ignored when `SUBROUTER_CODEX_BASE_URL` is set.
- `SUBROUTER_CODEX_SERVER`: older alias for `SUBROUTER_SERVER`, used only when `SUBROUTER_SERVER` is unset.
- `SUBROUTER_TAILSCALE_BIN`: optional path to the Tailscale CLI used to repair a named server carrying `--tailscale-node-id`; normal `PATH` and macOS app-bundle locations are detected automatically.
- `SUBROUTER_CODEX_BIN`: Codex binary used by the wrapper; defaults to `codex`.
- `SUBROUTER_CODEX_USER_EMAIL`: optional self-reported user email. When set, the wrapper sends `X-Subrouter-Agent: codex` and `X-Subrouter-User-Email` through a custom Subrouter provider.
- `SUBROUTER_CODEX_ACCOUNT_ID`: optional Subrouter account id or API-key label. When set, the wrapper sends `X-Subrouter-Account-ID` and Subrouter forces that account for the session.
- `OPENAI_API_KEY`: only for real API-key mode or custom env-key providers. Avoid setting it when you want ChatGPT subscription model behavior.
- `CODEX_HOME`: optional. Use it to test an isolated Codex config.
- `OPENAI_ORGANIZATION` and `OPENAI_PROJECT`: Codex forwards these as OpenAI headers for the built-in OpenAI provider.
- `CODEX_OSS_BASE_URL` and `CODEX_OSS_PORT`: only affect OSS providers such as Ollama or LM Studio, not the OpenAI provider.

## "Selected model is at capacity"

That message is OpenAI shedding load for one model and service tier; pressing retry usually gets through. Subrouter retries it for you, but only before any output reached Codex, so nothing is ever duplicated:

- Capacity errors should be rare and brief, so by default the request waits it out on its account, because the session's prompt cache lives there and another account would re-bill the whole conversation. Subrouter retries on that account with growing gaps (about 0.5s, 1s, 2s, 4s, 8s), then every 8-10 seconds, for up to 4 minutes of wall-clock time from the first attempt (inside Codex's 5-minute stream idle timeout), then Codex sees the error. `SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT` on the daemon changes the cap (a Go duration; `0` keeps retrying until Codex disconnects) and `SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL` the steady gap (at least 500ms; the ramp is capped at it, so `2s` retries after about 0.5s, 1s, 2s, 2s, ...). Where the daemon allows client headers (`SUBROUTER_CODEX_CAPACITY_RETRY_HEADER=1`, or the failover), a request can shape this same-account wait with `X-Subrouter-Retry: interval=2s,max-wait=20m` (either key optional; the interval is clamped to 500ms-60m and max-wait capped at 60m, and `max-wait=0` also means 60m: only the operator's `MAX_WAIT=0` makes the wait unbounded; a requested max-wait is not shortened by a configured fallback), which is what `sr codex --retry-interval 2s --retry-max-wait 20m` sends. The header shapes only the same-account wait, which runs with the failover off; with the failover on it is accepted but has no effect. Past about 5 minutes Codex's stream idle timeout may end the request first. When a regional egress (`SUBROUTER_CODEX_EGRESS_PROXIES`) or the Azure fallback is configured, the default is capped at about 10 seconds so the fallback you set up takes over sooner. A long wait is logged on its first retry and then about once a minute, and `/_subrouter/health` counts requests currently waiting under `overload_retry_held`. While a model is shedding for most requests across the pool, `sr status` prints a `Codex capacity` line and `/_subrouter/health` lists the pool under `codex_capacity_shedding`; that shortens the failover's budget to about 3 seconds, but not the same-account wait, whose steady gaps are already slow.
- Switching accounts is opt-in: with `SUBROUTER_CODEX_OVERLOAD_FAILOVER=1` on the daemon, Subrouter retries the same account once after 250-750ms, then tries other accounts 100-400ms apart (`SUBROUTER_CODEX_OVERLOAD_MAX_ACCOUNTS`, default 3), all within about 10 seconds. A WebSocket turn that hits capacity is then also closed so Codex reconnects on another account.
- Persist mode keeps retrying for a budget (default 2 minutes) from the first attempt. With the failover off it retries the same account every second for the longer of that budget and the daemon's own wait (4 minutes by default; an unbounded `MAX_WAIT=0` stays unbounded), not shortened by a configured fallback: it only makes the gaps faster. With the failover on it retries 0.5-2s apart across accounts after the failover ladder. Turn it on for every request with `SUBROUTER_CODEX_CAPACITY_RETRY=persist` on the daemon (`SUBROUTER_CODEX_CAPACITY_RETRY_BUDGET=5m` changes the budget, max 10m), or per request with the `X-Subrouter-Capacity-Retry: persist` header (and optionally `X-Subrouter-Capacity-Retry-Budget: 90s`), which is what `sr codex --persist-capacity …` sends. A request header of `default` opts out of a daemon-wide persist. The headers let a client hold a request for up to an hour (`X-Subrouter-Retry` max-wait is capped at 60m; the persist budget at 10m), so the daemon honors them only with `SUBROUTER_CODEX_OVERLOAD_FAILOVER=1` or `SUBROUTER_CODEX_CAPACITY_RETRY_HEADER=1`; otherwise they are ignored and the daemon's own setting applies. With the failover on, only one persisting request per session runs at a time; concurrent ones from that session get the default. Cancelling the request in Codex stops the loop.

To retry harder on the same account:

```bash
# daemon: every 2s after the ramp, for up to 4 minutes (the default cap)
SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL=2s subrouter serve
# or per launch, when the daemon sets SUBROUTER_CODEX_CAPACITY_RETRY_HEADER=1
sr codex --retry-interval 2s --retry-max-wait 4m
```

`sr codex --persist-capacity` remains as a preset of the same thing (1s gaps for the longer of the persist budget and the daemon's wait).

With the failover on, an account that shed a request ranks below the others for that model and tier for a few minutes, but its sessions stay on it (their prompt cache is there) unless it fails twice in a row. Its first success clears the mark. Without the failover no account is marked. Capacity is never counted as quota.

If you really want to move one conversation, the recommended way is to start or fork a new Codex session (it is placed fresh); you can also pin a launch with `SUBROUTER_CODEX_ACCOUNT_ID`, or have an admin drop the session's assignment with `DELETE /_subrouter/sessions?agent_type=codex&session_id=ID`.

## Azure fallback

`SUBROUTER_AZURE_CODEX_ENDPOINT` plus `SUBROUTER_AZURE_CODEX_API_KEY` (or `SUBROUTER_AZURE_CODEX_CONFIG_FILE` for several Azure resources) lets Subrouter finish a Codex `/responses` request on Azure OpenAI after the pool has spent five retries or has no usable account. It also absorbs the ChatGPT backend's in-stream `server_is_overloaded` failure ("Selected model is at capacity") on both the SSE and WebSocket transports, and `SUBROUTER_AZURE_CODEX_MODELS` limits which requested models it serves. The session then stays on that Azure endpoint for 30 minutes of activity so its prompt cache keeps hitting.

Force the route to test it:

```bash
sr az status          # which Azure endpoints the daemon armed
sr az test            # one forced request through the daemon
sr az codex exec "…"  # Codex with every request forced onto Azure
```

`sr az codex` pins Codex to a provider that sends `X-Subrouter-Azure: force` and disables WebSockets, because Azure has no WebSocket Responses surface. The daemon rejects a forced request it cannot serve rather than answering from the ChatGPT pool. See the README for the full behavior and configuration.
