# Recovering interrupted agent work

`sr recover` discovers local Claude sessions and delegated task artifacts without sending transcript contents to Subrouter.

```sh
sr recover list --query "Mac mini fleet" --limit 10
sr recover show --session SESSION_ID --json
sr recover prompt --session SESSION_ID --task TASK_ID
```

`SUBROUTER_CLAUDE_SESSION_ROOT` and `SUBROUTER_RECOVERY_TASK_ROOTS` can point at exported or test fixtures. The command is read-only; cmux owns workspace creation and launches the resulting prompt through `sr codex`.
