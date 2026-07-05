# @subrouter/cloudflare-worker

Cloudflare **Durable Objects** subrouter: a multi-tenant routing service with
hashed tenant keys, per-tenant Durable Object state, sticky session affinity,
OAuth refresh alarms, Go-compatible `/_subrouter/*` admin endpoints, and
upstream proxying for Codex/OpenAI and Claude/Anthropic accounts.

## Environments

| Env        | Worker name                       | URL                                  |
| ---------- | --------------------------------- | ------------------------------------ |
| dev        | `regatta-subrouter-do`            | `wrangler dev` -> http://127.0.0.1:8787 |
| staging    | `regatta-subrouter-do-staging`    | https://subrouter-staging.cmux.dev   |
| production | `regatta-subrouter-do-production` | https://subrouter.cmux.dev           |

Each env is a distinct Worker with its own Durable Object namespaces and
SQLite state. Registry tenants store tenant-scoped accounts, sticky sessions,
usage, transcripts, quotas, lifecycle, and alarms under
`SUBROUTER_DO.getByName("tenant:" + <tenant id>)`; historical bare names such
as `legacy` and `demo-org` remain reserved for compatibility state.
`TENANT_REGISTRY_DO.getByName("registry")` stores only tenant metadata and
SHA-256 hashes of tenant keys.

## Auth Model

`ADMIN_TOKEN` gates `/admin/*`, `/_subrouter/*`, and `/websocket-status`.

Data-plane requests require a tenant key in either:

```sh
Authorization: Bearer srt_<32 lowercase hex chars>
X-Subrouter-Tenant-Key: srt_<32 lowercase hex chars>
```

Tenant keys are returned only when created or rotated. The registry stores
`sha256(key)` hex, never the plaintext key. Revoked keys return `403`; missing,
malformed, unknown, and rotated-away keys return `401`.

Legacy shared-token access is disabled by default. To temporarily allow the old
`PROXY_TOKEN` path, set `ALLOW_LEGACY_PROXY_TOKEN=1` and `PROXY_TOKEN`; those
requests route only to tenant id `legacy`.

## Tenant Lifecycle

Create a tenant:

```sh
curl -sS https://subrouter.cmux.dev/admin/tenants \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"Acme"}'
```

Response:

```json
{"id":"acme","name":"Acme","key":"srt_0123456789abcdef0123456789abcdef","created_at":1783123200000,"revoked_at":null}
```

List tenants, without keys or hashes:

```sh
curl -sS https://subrouter.cmux.dev/admin/tenants \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

Rotate a tenant key:

```sh
curl -sS -X POST https://subrouter.cmux.dev/admin/tenants/acme/rotate \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

Revoke a tenant:

```sh
curl -sS -X POST https://subrouter.cmux.dev/admin/tenants/acme/revoke \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

## Account Upload API

Upload Claude OAuth credentials:

```sh
curl -sS https://subrouter.cmux.dev/tenant/accounts \
  -H "Authorization: Bearer $TENANT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"provider":"claude","label":"alice@example.com","claudeAiOauth":{"accessToken":"...","refreshToken":"...","expiresAt":1735689600000,"subscriptionType":"max","rateLimitTier":"..."}}'
```

Upload an Anthropic API key:

```sh
curl -sS https://subrouter.cmux.dev/tenant/accounts \
  -H "Authorization: Bearer $TENANT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"provider":"anthropic-apikey","label":"anthropic prod","apiKey":"sk-ant-..."}'
```

Upload Codex OAuth tokens:

```sh
curl -sS https://subrouter.cmux.dev/tenant/accounts \
  -H "Authorization: Bearer $TENANT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"provider":"codex","label":"codex@example.com","tokens":{"accessToken":"...","refreshToken":"...","idToken":"...","accountID":"..."}}'
```

Upload an OpenAI API key:

```sh
curl -sS https://subrouter.cmux.dev/tenant/accounts \
  -H "Authorization: Bearer $TENANT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"provider":"openai-apikey","label":"openai prod","apiKey":"sk-..."}'
```

Responses are sanitized:

```json
{"id":"claude-...","provider":"claude","label":"alice@example.com","auth_mode":"oauth","created_at":"2026-07-03T00:00:00.000Z","email":"alice@example.com"}
```

List and delete accounts:

```sh
curl -sS https://subrouter.cmux.dev/tenant/accounts \
  -H "Authorization: Bearer $TENANT_KEY"

curl -sS -X DELETE https://subrouter.cmux.dev/tenant/accounts/$ACCOUNT_ID \
  -H "Authorization: Bearer $TENANT_KEY"
```

OAuth uploads schedule the existing Durable Object alarm refresh path. Rotated
refresh tokens are persisted in the tenant DO.

## Admin Tenant Data

Tenant-scoped admin reads require an explicit `?tenant=<id>`:

```sh
curl -sS "https://subrouter.cmux.dev/_subrouter/accounts?tenant=acme" \
  -H "Authorization: Bearer $ADMIN_TOKEN"

curl -sS "https://subrouter.cmux.dev/_subrouter/transcripts?tenant=acme" \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

`GET /_subrouter/health` and bare `GET /_subrouter/ready` are
unauthenticated and do not return tenant data. `GET /_subrouter/ready?tenant=<id>`
is also unauthenticated and returns only `{ ok, draining }` for load-balancer
and rollout probes; malformed tenant ids return `400`, while well-formed unknown
ids return ready because no tenant state exists yet.

## cmux CLI Call Sequence

The follow-up cmux integration should call:

1. Admin side: `POST /admin/tenants` to create the tenant and show the key once.
2. Tenant side: `POST /tenant/accounts` to upload Claude/Codex OAuth or API-key credentials.
3. Tenant side: `GET /tenant/accounts` to show sanitized configured accounts.
4. Tenant side: `DELETE /tenant/accounts/:id` to remove an account.
5. Data plane: proxy requests with `Authorization: Bearer $TENANT_KEY`.

## Deploy

```sh
bun run deploy:staging
bun run deploy:production
```

Required Actions secrets:

- `CLOUDFLARE_ACCOUNT_ID` - Cloudflare account ID for Wrangler.
- `CLOUDFLARE_API_TOKEN` - long-lived Cloudflare API token with Workers deploy
  permissions for both `regatta-subrouter-do-staging` and
  `regatta-subrouter-do-production`.

## Observability

Request spans export to Axiom only when both secrets are configured:

- `AXIOM_TOKEN`
- `AXIOM_DATASET`

Optional:

- `AXIOM_DOMAIN` - defaults to `api.axiom.co`.

When either required Axiom setting is missing, telemetry is inert and performs
no export fetch. OAuth refresh alarm events are not emitted as spans in this
round; only the top-level Worker request span is exported.

Example Axiom APL:

```apl
['cmux-prod-otel-traces']
| where ['service.name'] == 'subrouter-do'
| summarize requests=count() by ['url.path'], ['subrouter.auth'], bin(_time, 5m)
```

## Local Dev

Create `.dev.vars` (gitignored) with at least:

```sh
ADMIN_TOKEN=<anything>
```

Optional legacy compatibility:

```sh
PROXY_TOKEN=<anything>
ALLOW_LEGACY_PROXY_TOKEN=1
```

Then run:

```sh
bun run dev
```

## Endpoints

- `GET /healthz`
- `GET /_subrouter/health`
- `GET /_subrouter/ready` - service-level readiness, no tenant data.
- `GET /_subrouter/ready?tenant=` - tenant drain readiness, returns 503 when draining; malformed tenant ids return 400.
- `POST /admin/tenants`, `GET /admin/tenants`
- `POST /admin/tenants/:id/rotate`, `POST /admin/tenants/:id/revoke`
- `POST /tenant/accounts`, `GET /tenant/accounts`, `DELETE /tenant/accounts/:id`
- `POST /_subrouter/drain?tenant=`
- `GET /_subrouter/drain-status?tenant=`
- `GET /_subrouter/accounts?tenant=`
- `GET|POST /_subrouter/account-status?tenant=`
- `GET /_subrouter/usage-status?tenant=`
- `POST /_subrouter/reload-accounts?tenant=`
- `GET /_subrouter/sessions?tenant=`
- `GET /_subrouter/dashboard?tenant=`
- `GET /_subrouter/transcripts?tenant=`
- `GET /_subrouter/transcripts/:agent/:session?tenant=`
- `GET /_subrouter/transcripts/:agent/:session/raw?tenant=`
- `POST /route` - tenant-key authed route selection.
- `POST /usage` - tenant-key authed usage recording.
- `GET /status` - tenant-key authed DO route counters.
- `GET /ws` - tenant-key authed WebSocket; messages `{ type: "ping"|"route"|"usage"|"status", ... }`.
- `GET /websocket-status?tenant=`, `GET /admin/accounts?tenant=`, `POST /admin/accounts`, `POST /admin/model-probe` - require `Authorization: Bearer $ADMIN_TOKEN`.
