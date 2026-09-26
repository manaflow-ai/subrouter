# Generic upstream 429 exhausted a routed session without account failover

**Status:** FIXED (candidate; live deployment pending)
**Filed:** 2026-09-03 by root@Daniels-Mac-Studio
**Owner:** Subrouter maintainers
**Class:** SYSTEMIC
**Recurrence:** seen at least twice in routed Codex sessions

## What happened

Two cmux agent sessions routed through the live Subrouter supervisor ended with
`exceeded retry limit, last status: 429 Too Many Requests`. The sessions were
sticky to one Codex account and the generic upstream 429 was returned to the
client instead of trying another eligible account.

## Evidence

- cmux workspace `E5AD624A-D3BB-477C-A9EC-38A0D2C7CCAE`, surfaces 3 and 9.
- `sr codex resume` launched Codex with `model_provider="subrouter"` and a
  local forwarder connected to live `127.0.0.1:31415`.
- Live logs showed repeated requests on the same account and no alternate-account
  usage-limit retry for the reported request IDs.
- The old classifier recognized only structured `usage_limit_reached` Codex
  responses; generic/headerless 429s passed through.

## Root cause

Codex request replay and quota classification were narrower than the upstream
response contract. AGY Cloud Code POSTs had the same omission, and AGY was not
enabled in local usage failover.

## Why it will recur

Any provider can emit a transient or allocation 429 without the structured
quota body. Sticky sessions then repeatedly retry one account until the client
exhausts its own retry budget.

## The fix now

- Codex generic 429s now trigger bounded request-scoped alternate-account
  failover without marking the account exhausted.
- AGY `/v1internal:*` POSTs are replayable; AGY 429s trigger account failover;
  AGY 401/expired credentials trigger credential failover.
- Explicit structured quota responses retain account-exhaustion accounting.

## The fix forever

Keep provider-specific classifiers and replayability tests together. Every new
provider must have tests for structured quota, generic 429, credential failure,
non-replayable requests, and sticky-session alternate selection. The canary must
exercise a real request through a second account before disarming rollback.

| Failure | Mechanism that catches it | Why alternatives do not |
|---|---|---|
| Generic Codex/AGY 429 | Provider retry classifier + replayable request test | Health checks cannot observe upstream quota responses |
| Credential 401/expiry | Credential-refresh/failover path + identity test | Client retries can repeat the invalid credential |
| Wrong account identity | OAuth userinfo verification and profile fingerprint guard | Labels alone are aliases, not identity |
| Sticky session never moves | Two-account session assignment/failover canary | A single successful request proves only one account |

## Routing

- [x] Fix now, by implementation agent
- [ ] Hand off

## Resolution

**Outcome:** Candidate commits `47d9742`, `8ac8ebc`, and `8f7561b` add AGY
request/credential failover and safe Codex generic-429 failover. Native AGY
Keychain/profile hardening is in `cd9573e` and `a7935f8`.
**Landed in:** isolated candidate worktree; live deployment is still pending.
**Did it work?:** Focused proxy, AGY, CLI, and Darwin cross-compilation tests
pass; shadow AGY sessions selected both accounts and reached the real Cloud Code
endpoints, but both generation attempts returned upstream HTTP 429. Identity
selection and bounded failover are proven; a successful routed generation is not.
**Still open:** Live cutover and authenticated production canary remain
required. Native AGY remains process-scoped/serialized rather than mid-session
rotatable.
