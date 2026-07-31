package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

const defaultSRSwitchInterval = 10 * time.Minute

type srAutoSwitchConfig struct {
	Interval     time.Duration
	Accounts     []accounts.Account
	AccountsFunc func() []accounts.Account
	// AccountsSnapshotFunc couples dynamic accounts to the generation used for
	// scheduler publication. It supersedes AccountsFunc when configured.
	AccountsSnapshotFunc func() ([]accounts.Account, uint64)
	Sessions             *session.Store
	SchedulerRef         *selectacct.SchedulerRef
	Logger               *slog.Logger
	FetchScores          func(context.Context, []accounts.Account) ([]selectacct.Score, int)
	SwitchActive         func(context.Context, string) error
	// Lease keeps the sweep singleton across concurrently live workers.
	Lease srAutoSwitchLease
}

func runSRAutoSwitch(ctx context.Context, cfg srAutoSwitchConfig) {
	if cfg.Interval <= 0 {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			claimed, leaseErr := cfg.Lease.acquire(cfg.Interval)
			if leaseErr != nil {
				logSRAutoSwitch(cfg.Logger, slog.LevelWarn, "sr auto-switch lease unavailable, sweeping anyway", "error", leaseErr)
			}
			if !claimed {
				// Another live worker already swept within this interval.
				timer.Reset(cfg.Interval)
				continue
			}
			picked, err := srAutoSwitchOnce(ctx, cfg)
			if err != nil {
				logSRAutoSwitch(cfg.Logger, slog.LevelWarn, "sr auto-switch failed", "error", err)
			} else {
				logSRAutoSwitch(cfg.Logger, slog.LevelInfo, "sr auto-switch selected account", "account", picked)
			}
			timer.Reset(cfg.Interval)
		}
	}
}

func srAutoSwitchOnce(ctx context.Context, cfg srAutoSwitchConfig) (string, error) {
	fetchScores := cfg.FetchScores
	if fetchScores == nil {
		fetchScores = fetchCodexScoresWithSuccess
	}
	switchActive := cfg.SwitchActive
	if switchActive == nil {
		switchActive = func(ctx context.Context, accountID string) error {
			store := accounts.DefaultCodexStore()
			stored, ok, err := store.FindStored(accountID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("account %q not found", accountID)
			}
			refreshCtx := accounts.WithCodexRefreshReason(ctx, "sr-auto-switch")
			refreshed, _, err := store.RefreshStoredIfExpired(refreshCtx, nil, stored)
			if err == nil {
				stored = refreshed
			} else {
				logSRAutoSwitch(cfg.Logger, slog.LevelWarn, "sr auto-switch token refresh failed, using cached tokens", "account", accountID, "error", err)
			}
			if err := accounts.WriteActiveCodexAuth(stored.Auth); err != nil {
				return err
			}
			for _, result := range syncCodexCompatibleAuth(stored) {
				if result.Err != nil {
					logSRAutoSwitch(cfg.Logger, slog.LevelWarn, "sr auto-switch compatible auth sync failed", "tool", result.Tool, "error", result.Err)
				}
			}
			return nil
		}
	}

	allAccounts := cfg.Accounts
	accountGeneration := uint64(0)
	if cfg.AccountsSnapshotFunc != nil {
		allAccounts, accountGeneration = cfg.AccountsSnapshotFunc()
	} else if cfg.AccountsFunc != nil {
		allAccounts = cfg.AccountsFunc()
	}
	candidates := oauthAccounts(allAccounts)
	if len(candidates) == 0 {
		return "", fmt.Errorf("no OAuth Codex accounts available for sr auto-switch")
	}

	scores, successful := fetchScores(ctx, candidates)
	if successful == 0 {
		return "", fmt.Errorf("no fresh OAuth usage scores available")
	}

	scheduler := selectacct.NewScheduler(scores)
	if cfg.SchedulerRef != nil {
		if !cfg.SchedulerRef.SetForAccountGeneration(scheduler, accountGeneration) {
			return "", fmt.Errorf("account pool changed during sr auto-switch")
		}
	}
	if cfg.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(cfg.Sessions.CountByAccount())
	}

	picked, err := scheduler.Pick(candidates)
	if err != nil {
		return "", err
	}
	if !scheduler.UsableForNewSession(picked.Provider, picked.ID) {
		if scheduler.Exhausted(picked.Provider, picked.ID) {
			return "", fmt.Errorf("no usable OAuth Codex accounts available")
		}
		logSRAutoSwitch(cfg.Logger, slog.LevelWarn, "sr auto-switch selected account below new-session headroom threshold", "account", picked.ID, "threshold", selectacct.MinNewSessionHeadroom)
	}
	if err := switchActive(ctx, picked.ID); err != nil {
		return "", err
	}
	return picked.ID, nil
}

func oauthAccounts(all []accounts.Account) []accounts.Account {
	out := make([]accounts.Account, 0, len(all))
	for _, account := range all {
		if account.AuthMode == accounts.AuthModeOAuth {
			out = append(out, account)
		}
	}
	return out
}

func logSRAutoSwitch(logger *slog.Logger, level slog.Level, message string, args ...any) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Log(context.Background(), level, message, args...)
}
