# Design Notes

Subrouter should be an account router, not another API-key vault.

## Cloudmux session leases

`POST /internal/v1/session-leases` gives an authenticated Cloudmux actor a
short-lived, invocation-scoped broker token and a concrete account/model
assignment. Network callers must use the configured Subrouter admin token.
The request uses camel-case Cloudmux IDs:

```json
{
  "organizationId": "organization-1",
  "workspaceId": "workspace-1",
  "conversationId": "conversation-1",
  "invocationId": "invocation-1",
  "agentSessionId": "agent-session-1",
  "agent": "pi",
  "provider": "codex",
  "model": "gpt-5.4",
  "proxyBaseUrl": "http://subrouter:31415"
}
```

The response includes `leaseId`, `sessionKey`, `expiresAt`, an ephemeral
environment, safe assignment metadata, and a `pi` block describing the
isolated `models.json` provider. The actor must keep the environment in memory,
set the named API-key environment variable, and select the returned Pi model.
For Codex, `pi.baseUrl` is `<proxyBaseUrl>/backend-api` because Pi appends
`/codex/responses`; `OPENAI_BASE_URL` is
`<proxyBaseUrl>/backend-api/codex` for generic Responses clients.
The broker token works as a normal bearer token for OpenAI-compatible requests
or `X-Api-Key` for Anthropic-compatible requests. Subrouter replaces it with
the selected account credential before forwarding. Provider credentials never
cross the Subrouter boundary. A lease can call only its provider's model
endpoint and, when assigned, its exact model.

Cloudmux deployments run Subrouter with `--require-session-leases`. In this
mode every proxy request must resolve an exact capability from the in-memory
lease store. Omitting a token or changing its public token shape cannot fall
through to normal account routing.

Codex Pi leases select only OAuth subscription accounts because Pi's
`openai-codex-responses` adapter and `/backend-api` route are ChatGPT-specific.
If no Codex OAuth account is available, lease creation returns `503` without
creating a lease or sticky session assignment. Claude, Kimi, ZAI,
OpenRouter, Grok, and Qwen leases may use API-key accounts supported by their
returned Pi adapter configuration. A provider is leased with the adapter its
protocol needs rather than its vendor: the Qwen Token Plan is reachable both as
`openai-completions` and, on its Anthropic endpoint, as `anthropic-messages`,
and each hands the sandbox the environment variables its own client reads.

A model-bound lease requires a top-level `model` string in the forwarded JSON
body. Every body occurrence and any forwarded `model` query value must match
the lease exactly. Subrouter routing headers do not satisfy this check because
they are removed before the upstream request. Missing, malformed, duplicate
conflicting, or oversized model-bearing bodies are rejected before proxying.

Lease-authenticated requests stay on the response's advertised account,
provider, and auth mode. Subrouter returns that account's quota or credential
failure directly and does not use another subscription account, Bedrock, or a
dedicated fallback API key. Transport-level replay may retry the same account.
If the assigned account disappears or changes auth mode, the request returns
`503`; the actor can release and acquire a new lease explicitly.

The token has three JWT-shaped segments so Pi's `openai-codex-responses`
adapter can read an account claim. Its public header has `typ: "SRLEASE"`; its
payload has `cloudmux_session_lease: true` and the constant synthetic account
ID `cloudmux-broker`. A random nonce and signature segment make each token
unique. Subrouter authorizes only an exact SHA-256 token-hash match in its
in-memory lease store. It does not trust the public claims or expose the
selected upstream account ID in them.

`POST /internal/v1/session-leases/<leaseId>/renew` extends a running agent loop.
It requires the same admin authentication as lease creation plus the current
broker token in `X-Subrouter-Lease`. It has no request body and returns the same
response shape as creation with a rotated broker token and a fresh 15-minute
`expiresAt`. Organization, workspace, conversation, invocation, agent session,
provider, account, auth mode, and model bindings cannot change during renewal.

The previous token can authorize model requests for 30 seconds so requests that
started during rotation can finish. It can authenticate an idempotent renewal
retry for two minutes. Concurrent retries with the same previous token return
the already-current token instead of rotating again. Cloudmux should run one
renewal loop per lease, renew before expiry, and atomically replace its token
file only after receiving a successful response. A token file must use mode
`0600` and live outside the agent's Git working tree.

Renewal returns `401` when the broker token is missing or invalid and `404`
after expiry or release. `DELETE /internal/v1/session-leases/<leaseId>` removes
every token generation and is idempotent. A release serialized before renewal
cannot be undone by that renewal. Leases are lost on Subrouter restart, so
callers must reacquire them when resuming work.

## Core model

Each incoming request maps to:

```text
provider + conversation session id -> account credential -> upstream request
```

Existing sessions must keep their account assignment. New sessions can be placed on the best account at that moment.

Subrouter also scopes the session by agent type. Today Codex is the default, but Claude and Gemini use separate namespaces so identical provider session ids cannot collide.

## Session IDs

Preferred source:

```text
X-Subrouter-Session
```

Fallback extraction:

- `x-codex-window-id`
- `x-codex-turn-state`
- `x-codex-parent-thread-id`
- `X-Session-ID`
- `X-Conversation-ID`
- `X-Codex-Session-ID`
- `X-Claude-Session-ID`
- `X-Gemini-Session-ID`
- `X-Gemini-Conversation-ID`
- `OpenAI-Conversation-ID`
- `Anthropic-Conversation-ID`
- `Google-Conversation-ID`
- `Idempotency-Key`
- query params named `session_id`, `conversation_id`, or `thread_id`
- small JSON bodies containing `session_id`, `conversation_id`, or `thread_id`

If none exists, Subrouter creates a fallback ID from remote address, user agent, method, and path. That is acceptable for smoke tests but too coarse for real caching.

## Agent type

Clients can set an explicit agent namespace:

```text
X-Subrouter-Agent
```

Accepted values are lowercase token-style names such as `codex`, `claude`, and `gemini`. If the header is missing, Subrouter infers the type from provider-specific session headers. If nothing identifies the agent, Subrouter defaults to `codex` for current compatibility.

## User attribution

Clients can send a self-reported user email for teammate-level observability:

```text
X-Subrouter-User-Email
```

`X-Subrouter-User` and `X-User-Email` are accepted as aliases. Subrouter normalizes the address, stores it on the session assignment, includes it in proxy logs as `user`, and exposes it in `/_subrouter/sessions`.

This is not authentication. Subrouter strips `X-Subrouter-Session`, `X-Subrouter-Agent`, `X-Subrouter-User-Email`, `X-Subrouter-User`, and `X-User-Email` before forwarding upstream.

Clients can force a selected Subrouter account with:

```text
X-Subrouter-Account-ID
```

`X-Subrouter-Account` is accepted as an alias. This is intended for explicit API-key fallback or targeted debugging. API-key account labels may be sent without the `apikey:` prefix. Subrouter stores the resolved account id on the session assignment and strips both account-selection headers before forwarding upstream.

## Transcript persistence

Transcript recording is off by default. `subrouter serve --transcripts <dir>` writes raw proxy transcript JSONL files under:

```text
<dir>/by-agent/<agent-type>/by-session/<agent-session-id>.jsonl
```

The `agent_session_id` is the base provider session id from Subrouter's session id. For Codex, `019...:0` maps to `019...`, matching `session_meta.payload.id` in Codex's own JSONL files under `~/.codex/sessions`. Codex events also include `codex_session_id` as a compatibility alias.

Transcript events include:

- `subrouter_meta`: account, user email, transport, path, upstream, and redacted headers.
- `http_body_chunk`: chunked HTTP or SSE body bytes as base64 with stream id, chunk index, offset, chunk byte count, and chunk SHA-256.
- `http_body`: final HTTP or SSE body summary with stream id, total byte count, total SHA-256, and chunk count.
- `websocket_message`: full WebSocket message payload as base64 with byte count, SHA-256, opcode, and direction.

This stores full payloads by design, but HTTP bodies are recorded as bounded chunks while the proxy streams them. Authorization-style headers are redacted, but bodies may contain sensitive plaintext or encrypted Codex transcript material.

## Scheduling

For each account, normalize all known rate windows into headroom:

```text
headroom = min(window_remaining_percent...)
```

New session selection:

1. Exclude accounts whose hard limit is reached.
2. Prefer usable subscription OAuth over API-key fallback.
3. Protect OAuth accounts below 40% bottleneck or short-window headroom.
4. Among healthy accounts, prefer the most bottleneck headroom expiring per second in the short window.
5. Prefer highest headroom.
6. Prefer fewer assigned active sessions.

Codex has both shorter rolling and daily/weekly style windows, so using the minimum headroom prevents saturating one window while another still looks available. Later, this can become weighted by expected task size.

In serve mode, the periodic `--sr-switch-interval` sweep refreshes the usage
scores used for routing. It never rewrites local Codex, OpenCode, or pi auth;
only an explicit account-manager command can change active auth. Set the
interval to `0` to disable scheduled score refresh.

## Account sources

Codex:

- Read accounts from `~/.subrouter/codex/accounts/*.json`.
- OAuth accounts provide `tokens.access_token` and optional `tokens.account_id`.
- API-key accounts provide `OPENAI_API_KEY`.
- Login, imports, switching, API-key accounts, server install/login, and admin-key usage are native Go commands under `sr` and `subrouter`. The older `cx` and `subrouter cx` forms remain compatibility aliases.

Claude Code:

- Read profile metadata from `~/.subrouter/codex/claude.json`.
- Read per-profile credentials from `~/.subrouter/codex/claude/<profile>` or macOS Keychain using Claude Code's `Claude Code-credentials-<hash>` service naming.
- Profile switching, env output, run, remove, and login are native Go commands under `sr claude`. `sr claude add` stores a one-year `claude setup-token` credential (access token, recorded expiry, `user:inference` scope, no refresh token); `sr claude login` keeps the refreshable browser OAuth credential. Every refresh path no-ops without a refresh token, so a setup token is used until its expiry and then fails closed with a terminal `no usable credential` error naming the re-add command.
- Bare `sr claude` manages local profiles. `sr claude proxy [claude args...]` is
  the explicit profileless pooled launcher: it uses the selected Subrouter
  server, needs no local daemon when that server is remote, and passes Claude
  arguments through unchanged.

Gemini:

- Use a separate `~/.subrouter/codex/gemini.json` namespace.
- Keep routing/session state separate from Codex and Claude even before Gemini credential import is fully implemented.

## Proxy behavior

The proxy should preserve incoming request shape and only inject the credential headers required for the selected account:

- Codex OAuth: `Authorization: Bearer <access_token>` and `ChatGPT-Account-ID` when available.
- API key: `Authorization: Bearer <api_key>`.
- Claude OAuth: provider-specific bearer token and beta headers.

Provider adapters should own provider-specific headers.

Codex has two upstream path conventions. ChatGPT subscription auth normally talks to `https://chatgpt.com/backend-api/codex/responses`, while API keys talk to `https://api.openai.com/v1/responses`. Subrouter accepts either `/v1/...` or bare paths from the client and normalizes them after selecting the account:

- OAuth account: strip a leading `/v1` and forward to the Codex backend.
- API-key account: add `/v1` if the client sent a bare path and forward to the OpenAI API.

This lets Codex use one `openai_base_url` while Subrouter still chooses the right upstream for each account type.
