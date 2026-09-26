# Provider adapter and launcher matrix

Subrouter's server route, native CLI launcher, and live-account validation are
separate capabilities. A row marked “adapter” means the server can route and
rewrite that wire protocol; it does not imply that invoking the vendor CLI by
its ordinary name uses Subrouter.

| Adapter / account mode | Native launcher or client path | Credential owned by selected router | Upstream auth rewrite | Direct bypass | Stickiness and failover | `sr status` evidence | Hermetic coverage | Live-account evidence |
|---|---|---|---|---|---|---|---|---|
| Codex OAuth and API key | `sr codex`; Responses at `/v1` | Isolated OAuth accounts and API keys | ChatGPT OAuth or OpenAI bearer selected per account; client placeholder removed | Plain `codex` | Per thread; quota, auth, and transport failover across eligible accounts | 5h/7d, reset credits, dollars, and named windows only when exposed | Full routing, credential isolation, retry, and launcher suites | User-tested |
| Claude OAuth and API key | `sr claude proxy`; Anthropic Messages at router root | Isolated Claude profiles and API keys | Client auth removed; selected bearer and required beta headers added | Plain `claude` or `sr claude-direct` | Per conversation; pooled profile failover, with optional pinned launch | Plan, session, weekly, model-specific, and extra windows when exposed | Full routing, profile isolation, retry, and launcher suites | User-tested |
| Kimi OAuth subscription | `sr kimi` (`proxy` alias); `/kimi/v1/messages` | Isolated managed Kimi profiles | Client auth removed; selected OAuth bearer and Kimi headers added | Plain `kimi` | Pooled per-session failover by default; `--account` is a hard per-launch pin with no account failover | Plan plus 5h/weekly windows; active/ready/recommended state | H1, H2, H3 | User-tested |
| Kimi API key | `sr kimi` (`proxy` alias); `/kimi/v1/messages` | Labeled API-key accounts | Client auth removed; selected bearer and `x-api-key` added | Plain `kimi` or direct vendor URL | Pooled 429/auth failover by default; `--account` is a hard per-launch pin | Key health; subscription windows only when the default Kimi authority exposes them | H1, H2, H3 | Server route user-tested; distinct-key ownership is label-based |
| Qwen Coding Plan API key | OpenAI-compatible client at `/qwen/v1` | Labeled Coding Plan keys | Client auth removed; selected bearer added | Plain `qwen` with its normal provider | Per session; key failover | Key health/model count; quota not inferred | H1, H2 | No live generation canary recorded |
| Qwen Token Plan, OpenAI protocol | `sr qwen` (`proxy` alias); `/qwen-token/v1` | Labeled Token Plan keys; optional per-key console credential is telemetry only | Client auth removed; selected plan bearer added | Plain `qwen` | Pooled key failover by default; `--account` is a hard per-launch pin; pool shared with Anthropic route | Vendor Lite/Standard/Pro, reported 5h/7d windows, login-needed state, active/recommended | H1, H2, H3 | Account and console status user-tested; native generation canary required after deployment |
| Qwen Token Plan, Anthropic protocol | Anthropic client at `/qwen-anthropic` | Same Token Plan pool as OpenAI route | Client auth removed; selected plan bearer and Anthropic version added | Direct vendor Anthropic endpoint | Same sticky account and exhaustion state as `/qwen-token` | Same shared account and console telemetry | H1, H2 | No live generation canary recorded |
| Antigravity / AGY OAuth | `sr agy` routed native launcher; server route `/antigravity` | Isolated server OAuth accounts imported with `sr agy add <label>` | `CLOUD_CODE_URL` points AGY at a short-lived local relay; relay replaces client auth with the selected router OAuth bearer | Plain `agy` | Server-side pooled selection/failover is per session and model-family aware; `--account` is a hard pin | Verified email and plan when exposed; independent Gemini and Claude/GPT quota families; ready/active/error | H1, H2, H3, H4 | Shadow proved two identities, pinned and pooled real 2xx Gemini generations via the daily consumer Cloud Code endpoint |
| Grok API key | OpenAI-compatible client at `/grok/v1` | Labeled xAI keys | Client auth removed; selected bearer added | Direct xAI URL | Per session; key failover | Key health/model count; quota not inferred | H1, H2 | No live-account canary recorded |
| Grok OAuth subscription | OpenAI-compatible client at `/grok/v1` | Router-managed Grok OAuth credential | Client auth removed; OAuth bearer plus CLI subscription headers added | Direct Grok CLI | Sticky session; current OAuth source is one login, while API keys can remain alternates | Stored/auth error and active session; no quota claim | H1, H4 | No live-account canary recorded |
| OpenRouter API key | OpenAI-compatible client at `/openrouter/v1` | Labeled OpenRouter keys | Client auth removed; selected bearer added | Direct OpenRouter URL | Per session; key failover | Key health and vendor credit data when `/key` exposes it | H1, H2 | No auditable in-repository live canary recorded |
| DeepSeek API key | OpenAI-compatible client at `/deepseek/v1` | Labeled DeepSeek keys | Client auth removed; selected bearer added | Direct DeepSeek URL | Per session; key failover | Key health/model count; quota not inferred | H1, H2 | No live-account canary recorded |
| Together API key | OpenAI-compatible client at `/together/v1` | Labeled Together keys | Client auth removed; selected bearer added | Direct Together URL | Per session; key failover | Key health/model count; quota not inferred | H1, H2 | No live-account canary recorded |
| Fireworks API key | OpenAI-compatible client at `/fireworks/v1` | Labeled Fireworks keys | Client auth removed; selected bearer added | Direct Fireworks URL | Per session; key failover | Key health/model count; quota not inferred | H1, H2 | No live-account canary recorded |
| OpenCode Zen API key | OpenAI-compatible client at `/opencode-zen/v1` | Labeled OpenCode Zen keys | Client auth removed; selected bearer added | Direct OpenCode Zen URL | Per session; key failover | Key health/model count; quota not inferred | H1, H2 | No live-account canary recorded |
| Z.AI API key | OpenAI-compatible client at `/zai/v1` | Labeled Z.AI keys | Client auth removed; selected bearer added | Direct Z.AI URL | Per session; key failover | Key health/model count; quota not inferred | H1 | No live-account canary recorded |
| Declared OpenAI-compatible API key | OpenAI-compatible client at `/<declared-name>/v1` | Labeled keys for a startup-declared provider | Client auth removed; selected bearer added | Direct declared upstream | Per session; key failover | Account/routing state; vendor quota not inferred | H1, H2 | No live-account canary recorded |
| Legacy Gemini CLI scaffold (not Antigravity) | None | None | None | Plain Gemini tooling | None | Profile management scaffold only | CLI help and namespace-exclusion tests; profile store untested | Not routed; not a canary target |

Hermetic coverage key:

- **H1 — route and auth:** `internal/proxy/opencode_provider_test.go` and
  `internal/proxy/keyed_provider_config_test.go` prove path normalization,
  credential replacement, protocol-specific headers, and declared-provider
  validation without vendor credentials.
- **H2 — stickiness and failure:**
  `internal/proxy/qwen_failover_test.go` and
  `internal/proxy/keyed_credential_failover_test.go` prove account ownership,
  sticky placement, 429/auth failover, and the shared Token Plan pool.
- **H3 — native launch:** `cmd/subrouter/sr_native_proxy_test.go` proves local
  relay composition, credential scrubbing, process-only Kimi/Qwen routing,
  system-policy refusal, direct-home preservation, and strict per-launch pins.
- **H4 — OAuth lifecycle and quota:** `internal/proxy/oauth_source_test.go`,
  `internal/proxy/scoreaccounts_test.go`, and provider-store/usage tests prove
  refresh behavior, verified telemetry, model-family isolation, and safe
  handling of missing quota buckets.

## Native launcher boundary

The dedicated launchers are explicit `sr` commands while the plain vendor
commands remain direct:

```text
sr codex
sr claude proxy
sr kimi
sr qwen
```

The older `sr kimi proxy` and `sr qwen proxy` spellings remain aliases. Kimi
and Qwen default to the router's pooled scheduler. A leading
`--account <selector>` pins only that launched child to the exact account and
disables account failover; bare `--account` opens a pinned picker. The option is
wrapper-owned only before the first vendor argument or `--`, so vendor arguments
remain unambiguous. None of these choices changes the global recommendation.

Kimi and Qwen launchers preserve their normal session stores. Kimi uses its
documented in-memory model override and forces that model for resumed sessions.
Qwen uses a temporary highest-precedence provider overlay because saved
`modelProviders` outrank a simple base-URL environment variable. It refuses to
run when an existing system policy is configured rather than masking that
policy. The overlay disables Qwen's `/auth` and `/model` provider switches and
shadows direct Alibaba credentials with non-secret process sentinels.

AGY exposes `CLOUD_CODE_URL` as a supported endpoint override. `sr agy` uses
that override to send Cloud Code requests through a short-lived local relay;
the relay injects the server-selected isolated OAuth credential, so account
pooling, quota-aware stickiness, refresh, and bounded failover work like the
other routed clients. Imports, identity verification, and read-only status
remain available through the same managed inventory. The local AGY login is
only needed for CLI startup; plain `agy` remains direct and unaffected.

Pooled native-launcher account affinity for new sessions is
provider-and-working-directory scoped. Kimi's workspace-relative `--continue`
operations keep that same affinity; Qwen deliberately rejects continue and
resume operations. An explicit Kimi session ID is instead provider-and-session
scoped, so it retains one router assignment across working directories without
inspecting or persisting vendor session files. A hard-pinned launch carries the
forced account only through the short-lived local relay and uses an
account-scoped session identity. The router never falls back
to a different account, parallel pins do not fight over one assignment, and a
pin does not replace the corresponding pooled session assignment.

Before starting the child, every native launcher performs an authenticated
`HEAD` preflight against the exact data-plane root. Lease-required/hosted
routers fail closed; the current launchers support local and ordinary
self-hosted routers, not the Cloudmux session-lease contract.

## Remote OAuth ownership

Kimi has an explicit managed-profile login flow. Antigravity instead uses the
vendor CLI's fixed Keychain slot as a deliberate one-at-a-time import source:
sign plain `agy` into an account, run `sr agy add <label>`, then repeat. Import
validates the refresh chain and binds it to the issuing public OAuth client so
the selected self-hosted router can refresh it without depending on its own CLI
binary. Plain `agy` and its Keychain item remain outside Subrouter ownership.

Implemented and hermetically tested, but without an auditable in-repository
live-account canary, are OpenRouter, Grok, DeepSeek, Together, Fireworks,
OpenCode Zen, Z.AI, Qwen Coding Plan, the Qwen Token Plan Anthropic protocol,
and declared custom OpenAI-compatible routes. Operators with those accounts
are invited to run an exact routed canary and record a credential-free result
or artifact reference. The legacy Gemini CLI namespace is excluded from that
list because it is only a store scaffold, not the Antigravity adapter described
above; its profile store does not yet have direct tests.

Live validation should be recorded only after a real request crosses the exact
launcher and selected router. A successful status probe or local vendor login
does not count as a routed generation canary.
