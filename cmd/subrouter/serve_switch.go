package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// serveSwitchAccount backs /_subrouter/switch-account: the remote analogue
// of `sr switch` (Codex) and `sr claude switch` (Claude), executed on the
// daemon host. Codex first folds the host's current auth.json back into the
// store — matching `sr switch` — so a manually-logged-in account is not
// lost by the overwrite; a sync failure only logs because the switch target
// comes from the store, not the active file.
func serveSwitchAccount(codexStore accounts.CodexStore, claudeStore agentclaude.Store, logger *slog.Logger) func(context.Context, string, string) error {
	return func(ctx context.Context, provider, accountID string) error {
		switch provider {
		case string(accounts.ProviderCodex):
			if err := codexStore.SyncActiveToStore(); err != nil {
				logSRAutoSwitch(logger, slog.LevelWarn, "switch-account sync active to store failed", "error", err)
			}
			return switchActiveCodexAccount(ctx, codexStore, logger, "switch-endpoint", accountID)
		case string(accounts.ProviderClaude):
			profile, ok, err := claudeStore.MatchProfile(accountID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("no Claude profile matching %q", accountID)
			}
			return claudeStore.SetActiveProfile(profile.Name)
		default:
			return fmt.Errorf("unsupported provider %q", provider)
		}
	}
}
