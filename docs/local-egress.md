# Team vault with local egress

Each developer runs Subrouter on `127.0.0.1:31415`. Codex and Claude send requests to that daemon. The daemon asks `cmux.com` for a credential lease, then sends the provider request from the developer's machine.

The central service stores provider refresh tokens and API keys in the team's Subrouter Durable Object. A client receives only the selected provider access token or API key, account metadata, a credential generation, and lease timestamps. It never receives a provider refresh token or ID token.

## Refresh invariants

- Only the team's Durable Object calls provider token endpoints.
- OAuth upload and repair complete one forced central refresh before success. This adopts the current refresh chain and makes the laptop's copied chain stale when the provider rotates it.
- Refreshes for one account are single-flight.
- A successful refresh atomically stores the new access, refresh, and ID tokens before a lease is issued.
- `invalid_grant` and explicit refresh-token failures mark the account as needing repair.
- A transport failure or malformed success response is ambiguous because the provider may have rotated the token. The account is frozen and the old refresh token is never retried.
- `sr account repair <id>` replaces the credential chain in place and clears its sticky sessions.

The logical lease lasts five minutes and the local daemon discards it fifteen seconds early. OAuth access tokens remain usable until the provider's own expiry, even after the logical lease ends. API keys cannot be made short-lived by Subrouter, so they remain the highest-impact credential type.

The local machine stores a revocable Stack session in `~/.config/subrouter/cloud.json` with mode `0600`. This session authenticates the user and team to `cmux.com`; it is separate from provider refresh-token custody. `sr logout` revokes that exact session before removing the file.

## Authorization

The API resolves Stack membership and permissions on every request. `subrouter:use` can list accounts and lease access credentials. `subrouter:manage_accounts` can upload, repair, and delete credentials. `SUBROUTER_ALLOWED_TEAM_IDS` limits the private beta.

The local daemon binds only to loopback and requires the signed-in user's Stack access token on provider proxy requests. This prevents another operating-system user on a shared Linux host from spending team credentials through the loopback port. Health and readiness probes remain unauthenticated. Cloud mode fails closed if login, team selection, or the local daemon is unavailable. It never silently routes through a legacy shared host or a local personal credential store.

## Rollout

1. Deploy the Worker and `cmux.com` API with a one-team allowlist.
2. Run `sr setup`, then `sr login`.
3. Import one low-use account with `sr account import --only <label>`.
4. Make a real Codex or Claude request from that machine. This is the canary validation and must egress locally.
5. Exercise one forced refresh and one `invalid_grant` repair.
6. Only then run `sr account import --all --dry-run`, review the list, and confirm it with `sr account import --all --yes`.

Local source files are not deleted during the canary, but central adoption may rotate their OAuth refresh chain. `sr logout` disables cloud mode; restoring direct provider use can require a fresh local OAuth login.
