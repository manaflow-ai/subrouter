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

The wrapper injects this global config override into the child Codex process:

```toml
openai_base_url = "http://127.0.0.1:31415/v1"
```

Subrouter supports Codex WebSocket requests, so the built-in provider can keep its normal transport behavior.
This includes Responses WebSockets at `/v1/responses` and realtime WebSockets at `/v1/realtime`.

Do not set a dummy `OPENAI_API_KEY` for normal subscription routing. Codex should stay logged in normally, ideally with ChatGPT auth. Subrouter replaces the outbound Authorization and `ChatGPT-Account-ID` headers with the selected `sr` account.

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

Codex does not allow overriding arbitrary headers on the built-in `openai` provider. When either variable is set, `subrouter codex` switches the child process to a custom `subrouter` provider with WebSockets enabled and sends `X-Subrouter-Agent: codex`, plus `X-Subrouter-User-Email` and/or `X-Subrouter-Account-ID`. Subrouter still replaces outbound credentials before forwarding upstream. `SUBROUTER_CODEX_USER_EMAIL` is only teammate observability metadata; account selection belongs in `SUBROUTER_CODEX_ACCOUNT_ID`.

## Models

There are two separate Codex concepts:

- `model`: the model slug selected by `/model`.
- `model_provider`: the backend/provider config.

Subrouter keeps `model_provider = "openai"` and does not rewrite the `model` field. `/model` continues to use Codex's own model catalog and auth-mode filtering. If Codex is logged in with ChatGPT auth, subscription-only models stay visible. If Codex is forced into API-key auth by `OPENAI_API_KEY`, Codex filters the picker to API-supported models.

Subrouter accepts `/v1/responses` and `/responses`. For OAuth subscription accounts it forwards to `https://chatgpt.com/backend-api/codex` and strips the `/v1` prefix when present. For API-key accounts it forwards to `https://api.openai.com` and adds `/v1` when needed.

## Custom Provider Fallback

If WebSocket support needs to be disabled for debugging, use a custom provider:

```bash
OPENAI_API_KEY=dummy codex exec \
  -c 'model_provider="subrouter"' \
  -c 'model_providers.subrouter.name="Subrouter"' \
  -c 'model_providers.subrouter.base_url="http://127.0.0.1:31415/v1"' \
  -c 'model_providers.subrouter.env_key="OPENAI_API_KEY"' \
  -c 'model_providers.subrouter.wire_api="responses"' \
  -c 'model_providers.subrouter.supports_websockets=false' \
  "your prompt"
```

## Env Vars

- `SUBROUTER_CODEX_BASE_URL`: base URL injected by `subrouter codex`; defaults to `http://127.0.0.1:31415/v1`.
- `SUBROUTER_CODEX_SERVER`: named server from `sr server add`; ignored when `SUBROUTER_CODEX_BASE_URL` is set.
- `SUBROUTER_CODEX_BIN`: Codex binary used by the wrapper; defaults to `codex`.
- `SUBROUTER_CODEX_USER_EMAIL`: optional self-reported user email. When set, the wrapper sends `X-Subrouter-Agent: codex` and `X-Subrouter-User-Email` through a custom Subrouter provider.
- `SUBROUTER_CODEX_ACCOUNT_ID`: optional Subrouter account id or API-key label. When set, the wrapper sends `X-Subrouter-Account-ID` and Subrouter forces that account for the session.
- `OPENAI_API_KEY`: only for real API-key mode or custom env-key providers. Avoid setting it when you want ChatGPT subscription model behavior.
- `CODEX_HOME`: optional. Use it to test an isolated Codex config.
- `OPENAI_ORGANIZATION` and `OPENAI_PROJECT`: Codex forwards these as OpenAI headers for the built-in OpenAI provider.
- `CODEX_OSS_BASE_URL` and `CODEX_OSS_PORT`: only affect OSS providers such as Ollama or LM Studio, not the OpenAI provider.

## Azure fallback

`SUBROUTER_AZURE_CODEX_ENDPOINT` plus `SUBROUTER_AZURE_CODEX_API_KEY` (or `SUBROUTER_AZURE_CODEX_CONFIG_FILE` for several Azure resources) lets Subrouter finish a Codex `/responses` request on Azure OpenAI after the pool has spent five retries or has no usable account. The session then stays on that Azure endpoint for 30 minutes of activity so its prompt cache keeps hitting. See the README for the full behavior and configuration.
