# Claude proxy home migration

`sr claude proxy` keeps credentials and account state in its per-account home, then reconciles the rest of that home with `~/.claude` at launch. Projects, skills, plugins, agents, commands, hooks, `CLAUDE.md`, keybindings, and output styles are shared through symlinks. If an older account home contains a real copy, Subrouter copies unique files into `~/.claude`, preserves conflicts there with a legacy suffix, and leaves a timestamped backup beside the old entry before replacing it with the symlink. Repeating a launch is safe.

The proxy launch settings start from `~/.claude/settings.json`, merge settings Claude saved in the account home, and then add Subrouter's routing and status-line settings. Routing environment values remain authoritative. The resolved `~/.claude/projects` directory is added to `permissions.additionalDirectories`, which allows Claude Code's memory files to follow the shared projects link without a symlink escape prompt.

Claude Code also has `autoMemoryDirectory`, but it selects one directory for all projects. That would flatten project memory and change Claude's project namespace, so Subrouter keeps the per-project `projects/<project>/memory` layout and grants the resolved projects root instead.

On systems where `/home/leo` is bind-mounted as `/Users/leoli`, path comparisons use filesystem identity, so either spelling remains stable and does not trigger a repeated migration.

Backups named `*.subrouter-backup-*` are retained for manual recovery. Subrouter never removes those backups or credentials.
