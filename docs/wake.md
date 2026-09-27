# Quota wake alarms

`sr wake` is the local control surface for quota-triggered agent resumes. The
alarm store, policy commands, proxy handoff, automatic scheduling, and the
launchd worker are available on this branch. Fleet enablement remains an
explicit rollout step after the upgraded proxy is deployed.
It is disabled by default and must be enabled independently for Codex and
Claude. The existing cmux/session watcher remains the only component that
reads a terminal surface or sends a resume action; Subrouter owns quota
classification, reset selection, and durable alarm state.

## Behavior

When an enabled agent receives an authoritative account or model-pool quota
failure, Subrouter fails over across untried accounts first. If no eligible
account remains, it records the earliest reset for the requested pool and
creates one persistent wake alarm. The alarm is limited to a recently active
session that produced the quota signal. Its fire time is reset time plus a
configurable grace delay and bounded per-session jitter. Dispatch is rate
limited so many sessions do not reconnect at once.

On the first watcher scan, recently active means last activity within eight
hours. The worker waits 30 seconds after a failure before saving the cmux
activity baseline, so the error itself can finish updating the terminal.
Later passes accept only a quota or provider signal from the preceding eight
hours. They do not rediscover and resume an old tab from a broad screen scrape. An alarm
already scheduled for a future reset can fire after eight hours if the exact
agent, session, and surface binding still exists at dispatch.
If the proxy sees a later successful request or a newer quota failure, the old
alarm becomes stale. Later cmux activity also makes it stale after a short
window that lets the original quota error finish updating the screen.

The watcher validates the original machine, cmux surface, agent type, session
ID, and active-writer state before sending the configured action. A mismatched
or missing surface becomes `stale`; no replacement tab is selected.

`sr wake worker` is the singleton shared watcher. It reads the proxy's
classified `/_subrouter/recovery-status` handoff, resolves the saved cmux
session with `cmux sessions --json`, validates the exact surface with
`cmux read-screen`, and is the only process that sends the action. Install it
across reboot with `sr wake install`; remove it with `sr wake uninstall`.
The worker uses a 30-second deterministic per-alarm jitter and a five-second
default spacing between sends; use `--interval` and `--spacing` to tune the
monitor without changing alarm times.

The queue keeps three recovery kinds separate: `codex-provider` is a temporary
model-provider capacity event and requires a cooldown plus a lightweight health
check; `codex-quota` is an account reset and uses `/goal resume` after reset and
grace; `claude-quota` is a Claude reset and uses `continue`. A signal for one
kind cannot create or dispatch another kind of alarm.

An existing automatic quota alarm also watches for fresh quota returning
before its predicted reset time. Once the proxy confirms a valid subscription
account has enough quota for that agent and model pool, the shared worker moves
that alarm to one minute from now plus its normal jitter. This covers a manual
reset credit or a service-side early reset. It does not accelerate other agents,
other model pools, provider-capacity alarms, manual alarms, or cancelled alarms.
It requires a subscription usage measurement newer than the failure. Cached
pre-failure windows, stale last-good usage, and paid extra-usage-only windows
cannot prove recovery. Automatic recovery
must still be enabled for the agent. Early wake is on by default and can be
disabled per agent with `sr wake early codex disable` or `sr wake early claude
disable`; `enable` restores the default. The scheduled reset time remains the
fallback if no fresh recovery is observed.

Codex capacity recovery must not immediately replay a large `/goal resume`
request after a provider failure. The first failure records the provider error
and enters a short cooldown. The watcher or a lightweight provider probe must
show that the route is usable before one expensive resume is attempted. Further
failures use bounded backoff and a finite attempt budget; they do not loop
large-context resumes. Token usage and whether generation began are recorded
for each attempt so this policy can be tuned from evidence. The proxy records
dispatch, pending state, and the next matching POST outcome
(`generation_began`, `generation_not_started`, or
`provider_or_quota_failure`) without adding a second terminal watcher. Token
fields are retained when the caller supplies usage counts; the watcher itself
does not parse or persist request bodies.

The default policy permits two goal attempts and does not send a fallback
`continue`. Fallback is an explicit policy choice and is sent at most once
after the configured failure threshold; otherwise the state becomes `stop`
until a fresh provider event. This keeps repeated capacity failures from
burning replay tokens.

Enable the opt-in fallback explicitly with `sr wake policy codex
--allow-continue --continue-after 2`. The policy is persisted with restrictive
permissions and remains disabled until changed.

## Configuration and controls

Automatic recovery is disabled by default for each agent. Early wake is enabled
by default for both agents, but only has an effect when automatic recovery is
enabled. Settings persist in the local `wake-config.json` state file.

Controls are:

```text
sr wake list
sr wake show <id>
sr wake enable <claude|codex>
sr wake disable <claude|codex>
sr wake early <claude|codex> <enable|disable>
sr wake update <id> --delay 5m --expires-in 2d4h15m
sr wake now <claude|codex|all>
sr wake cancel <id>
sr wake cancel --agent <claude|codex>
sr wake cancel --all
sr wake worker --once
sr wake install
sr wake uninstall
sr wake policy codex --no-continue --max-goal-attempts 2 --cooldown 1m
```

Durations accept days, hours, and minutes (`2d4h15m`). They are converted to
absolute UTC timestamps when stored so alarms survive reboot. Each record has
`wake_at`, `expires_at`, the exact cmux surface and session identity, the
provider/model pool, action, launchd label, and status (`scheduled`, `fired`,
`completed`, `stale`, `cancelled`, `expired`, or `failed`).

`wake now` revalidates and dispatches existing alarms immediately. It uses the
same writer checks and throttling as automatic dispatch, which makes it safe
after a manual quota reset.

## Provider signals

Codex quota signals include `usage_limit_reached`, `insufficient_quota`,
`usage_not_included`, `quota_exceeded`, `rate_limit_exceeded`, workspace credit
or spend-cap failures, and reset fields such as `resets_in_seconds` or
`resets_at`, including their `response.failed` SSE and websocket forms.

Claude quota signals include HTTP 429/401/403, rejected
`anthropic-ratelimit-unified-status`, rejected 5-hour or weekly windows,
model-scoped `7d_oi` rejection, `Retry-After`, and unified reset timestamps.
Headerless transient 429s and `allowed_warning` responses remain request-level
failover only and do not create long-lived alarms.

## Acceptance

- The watcher creates no automatic alarm while the agent setting is disabled.
  Explicit `wake schedule` alarms remain available for manual control, including
  `wake now` and worker dispatch.
- A session fails over immediately before any alarm is created.
- The alarm selects the earliest eligible account reset for the requested pool.
- Reboot and sleep/wake preserve the alarm and its absolute timestamps.
- `list`, `show`, `update`, `now`, and `cancel` are deterministic and durable.
- Duplicate workers cannot read or write the same surface concurrently.
- Codex and Claude actions are not interchangeable.
- The full `go test ./...` suite passes before handoff.
