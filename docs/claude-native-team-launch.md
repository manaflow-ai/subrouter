# One-command Claude Code for a team (subscription logins)

Team members can use their own Claude Code subscriptions while retaining the usual `sr claude` entry point. Once configured, the real Claude Code executable handles prompts, authentication, token refresh, and session history for that **individual user**. Subrouter acts as the launcher and profile selector.

## Set once

```sh
# In the developer's login shell or managed workstation environment:
export SUBROUTER_CLAUDE_DEFAULT_ROUTE=native

# Optional: choose one locally authenticated managed profile.
# Omit this to use the active local profile, or the user's default ~/.claude login.
export SUBROUTER_CLAUDE_NATIVE_PROFILE=work

sr claude
```

An administrator can distribute the environment settings to each developer through their standard workstation provisioning. Provisioning should assign each teammate an individual Claude subscription seat, with a locally completed login.

The option `SUBROUTER_CLAUDE_DEFAULT_ROUTE=native` also works for non-interactive invocations:

```sh
sr claude -p "Summarize recent changes" --output-format stream-json
sr claude --resume <session-id>
```

Applications integrating through subprocesses can invoke the same binary and parse Claude Code's documented output. This allows automation outside Claude Code's interactive terminal while leaving model requests and credentials under the real Claude Code client for the same signed-in user.

## Routing modes

| Implicit command | Default (unset or `pooled`) | `native` |
| --- | --- | --- |
| `sr claude` | Existing subscription pool picker | Direct native Claude Code process |
| `sr claude -p "task"` | Existing launch behavior | Direct native process with CLI arguments |
| `sr claude --resume ID` | Existing launch behavior | Resume using that user's native history |

Explicit `sr claude proxy`, `sr claude run <profile>`, `sr claude login`, and other account-management commands keep their previous meanings.

A native launch clears inherited Subrouter and provider routing environment variables. With a named local managed profile, the launch pins `CLAUDE_CONFIG_DIR` and `CLAUDE_CODE_CONFIG_DIR` to that profile. It uses private launch settings and ignores persistent settings sources that could silently redirect calls through a subscription proxy.

## Team boundaries

- Each person authenticates their own Claude Code seat; a server never receives their subscription token.
- The team's task coordinator can assign work to the user's workstation and collect results with the user's permission. It should not turn one employee's seat into a shared inference backend for everyone.
- Subrouter's existing pooled proxy remains an explicit legacy route. This native mode makes no claim that credential pooling avoids suspensions.
- Shared quotas, queueing, approvals, attribution, and usage reporting can be added on top of separately authenticated team seats. Keep the scheduling control plane distinct from subscription inference.

## Rollback

Unset `SUBROUTER_CLAUDE_DEFAULT_ROUTE`, or set it to `pooled`, to restore the previous bare-command behavior.
