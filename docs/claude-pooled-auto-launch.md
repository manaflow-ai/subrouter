# Automatic pooled Claude Code launches

Keep the existing **subscription-based pooled proxy** while making the ordinary
`sr claude` command hands-free for everyone on a team.

## One-time team setup

1. Configure every workstation to use the intended Subrouter team remote (for
   example, `sr remote use team`) and give it the appropriate tenant access.
2. Enroll the team's approved subscription accounts with the existing
   server-owned Claude login flow. The server owns rotating OAuth refresh
   credentials; avoid simultaneously refreshing a copied token on other hosts.
3. Each teammate runs `sr claude mode auto` **once**. Alternatively, a team
   administrator can distribute `SUBROUTER_CLAUDE_LAUNCH_MODE=auto` through
   workstation provisioning.

From then on:

```sh
sr claude
sr claude -p "Explain this change" --output-format stream-json
sr claude --resume <session-id>
```

All three invoke **the same Subrouter pooled proxy** without an account-selection
prompt, including if an older local profile is marked active. The selected
Subrouter server picks eligible accounts according to its existing session,
quota, and retry policies. The client does not pre-pin or express an initial
account preference. It also maintains the current status line, session ledger,
session-affinity, and failover capabilities.

To see or change the saved local preference:

```sh
sr claude mode
sr claude mode auto
sr claude mode choose
```

- The old interactive soft-preference picker remains at `sr claude choose`.
  A choice is an initial preference; the server can still fail over.
- For a **hard pin** (which disables account failover), keep using
  `sr claude proxy --account`.
- Existing explicit `sr claude proxy`, `sr claude run`, `sr claude login`,
  and other account management commands retain their meaning.
- The default mode for existing installations remains `choose` until a user
  or administrator opts in.
- `SUBROUTER_CLAUDE_LAUNCH_MODE` overrides the saved per-user preference
  when set, making central rollout and rollback easy.

## Using other clients

The same server proxy continues to expose its Anthropic-compatible Messages
endpoint to authenticated external clients. Set up each client using the
Subrouter server/tenant endpoint and its access policy; clients should supply
a distinct session ID for sticky routing. The `sr claude` launch-mode
preference affects only the CLI convenience command.

## Operational and policy notes

This change improves the ergonomics of existing subscription pooling. It
does **not** modify credential behavior, spoof client identity, or claim to
prevent Anthropic suspensions. Anthropic's consumer subscription policies
restrict credential redistribution. Teams are responsible for evaluating
their authorization and terms before deploying pooled subscription access.
