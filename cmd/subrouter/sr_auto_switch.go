package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

const defaultSRSwitchInterval = 10 * time.Minute

const srAutoSwitchPublishAttempts = 3

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
	// SwitchActive is an explicit account-manager hook used by callers and
	// tests that intentionally synchronize interactive auth. Nil keeps the
	// sweep read-only while still publishing fresh routing scores.
	SwitchActive func(context.Context, string) error
	// Lease keeps the sweep singleton across concurrently live workers.
	Lease srAutoSwitchLease
	// DelayFirstSweep is used by serving workers whose startup score coordinator
	// already owns the immediate shared sweep. Periodic refreshes begin after one
	// interval instead of duplicating startup work.
	DelayFirstSweep bool
	// ScoreSnapshots publishes successful periodic refreshes for successor
	// workers. Nil leaves one-shot and test callers unchanged.
	ScoreSnapshots *srUsageScoreStore
}

func runSRAutoSwitch(ctx context.Context, cfg srAutoSwitchConfig) {
	if cfg.Interval <= 0 {
		return
	}
	firstDelay := time.Duration(0)
	if cfg.DelayFirstSweep {
		firstDelay = cfg.Interval
	}
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	// A broken lease fails the same way every tick; log each distinct error
	// once, and again after it clears and recurs.
	lastLeaseErr := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			claimed, leaseErr := cfg.Lease.acquire(cfg.Interval)
			if leaseErr != nil {
				if msg := leaseErr.Error(); msg != lastLeaseErr {
					lastLeaseErr = msg
					logSRAutoSwitch(cfg.Logger, slog.LevelWarn, "sr auto-switch lease unavailable, sweeping anyway", "error", leaseErr)
				}
			} else if lastLeaseErr != "" {
				lastLeaseErr = ""
				logSRAutoSwitch(cfg.Logger, slog.LevelInfo, "sr auto-switch lease recovered")
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
	var scheduler selectacct.Scheduler
	var candidates []accounts.Account
	var scores []selectacct.Score
	var successful int
	var fetchedGenerationKey string
	lastConflict := "scheduler score snapshot changed"
	for attempt := 1; attempt <= srAutoSwitchPublishAttempts; attempt++ {
		allAccounts := cfg.Accounts
		accountGeneration := uint64(0)
		if cfg.AccountsSnapshotFunc != nil {
			allAccounts, accountGeneration = cfg.AccountsSnapshotFunc()
		} else if cfg.AccountsFunc != nil {
			allAccounts = cfg.AccountsFunc()
		}
		candidates = codexOAuthAccounts(allAccounts)
		if len(candidates) == 0 {
			return "", fmt.Errorf("no OAuth Codex accounts available for sr auto-switch")
		}
		generationKey := codexScoreGenerationKey(accountGeneration, candidates)
		scoreRevision := uint64(0)
		if cfg.SchedulerRef != nil {
			scoreRevision = cfg.SchedulerRef.ScoreRevision()
		}

		// Score-revision conflicts do not invalidate provider measurements. Keep
		// the fetched scores and retry only the atomic publication. A changed
		// account or credential generation is the sole reason to re-fetch.
		if fetchedGenerationKey != generationKey {
			scores, successful = fetchScores(ctx, candidates)
			if successful == 0 {
				return "", fmt.Errorf("no fresh OAuth usage scores available")
			}
			// OAuth refresh may rotate the credential used by the successful
			// measurement. The scores describe that post-refresh credential, so
			// adopt its key without issuing the same usage requests again. A real
			// account-set change invalidates the batch and is re-fetched.
			if cfg.AccountsSnapshotFunc != nil {
				currentAccounts, currentGeneration := cfg.AccountsSnapshotFunc()
				currentCandidates := codexOAuthAccounts(currentAccounts)
				if !sameCodexScoreAccounts(candidates, currentCandidates) {
					fetchedGenerationKey = ""
					if attempt == srAutoSwitchPublishAttempts {
						return "", fmt.Errorf("sr auto-switch account pool changed during usage fetch")
					}
					continue
				}
				candidates = currentCandidates
				accountGeneration = currentGeneration
				generationKey = codexScoreGenerationKey(accountGeneration, candidates)
				if cfg.SchedulerRef != nil {
					scoreRevision = cfg.SchedulerRef.ScoreRevision()
				}
			}
			fetchedGenerationKey = generationKey
		}

		scheduler = selectacct.NewScheduler(scores)
		if cfg.SchedulerRef == nil {
			break
		}
		// The auto-switch refresh is Codex-only, but SchedulerRef is shared by
		// every provider. Atomically replace only the freshly fetched Codex scores
		// so a concurrent full-provider refresh cannot be erased.
		var published bool
		scheduler, published = cfg.SchedulerRef.MergeScoresForAccountGenerationAtScoreRevision(
			scores, accountGeneration, scoreRevision,
		)
		if published {
			break
		}
		if cfg.AccountsSnapshotFunc != nil {
			currentAccounts, currentGeneration := cfg.AccountsSnapshotFunc()
			currentKey := codexScoreGenerationKey(currentGeneration, codexOAuthAccounts(currentAccounts))
			if currentKey != generationKey {
				lastConflict = "account pool changed"
			} else {
				lastConflict = "scheduler score snapshot changed"
			}
			if currentKey != generationKey {
				fetchedGenerationKey = ""
			}
		}
		if attempt == srAutoSwitchPublishAttempts {
			return "", fmt.Errorf(
				"sr auto-switch could not publish fresh usage after %d attempts because the %s concurrently",
				srAutoSwitchPublishAttempts, lastConflict,
			)
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	if cfg.ScoreSnapshots != nil && fetchedGenerationKey != "" {
		if err := cfg.ScoreSnapshots.write(srUsageScoreSnapshot{
			SchemaVersion: scoreSnapshotSchemaVersion,
			GenerationKey: fetchedGenerationKey,
			FetchedAt:     time.Now().UTC(),
			Scores:        scores,
		}); err != nil {
			return "", fmt.Errorf("publish shared usage score snapshot: %w", err)
		}
	}
	if cfg.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(proxy.SchedulerSessionCounts(cfg.Sessions))
	}

	// PickBest, not Pick: auto-switch maintains one active CLI account over
	// time, and the placement spread would rotate it across equally-usable
	// accounts on every interval.
	picked, err := scheduler.PickBest(candidates)
	if err != nil {
		return "", err
	}
	if !scheduler.UsableForNewSession(picked.Provider, picked.ID) {
		if scheduler.Exhausted(picked.Provider, picked.ID) {
			return "", fmt.Errorf("no usable OAuth Codex accounts available")
		}
		logSRAutoSwitch(cfg.Logger, slog.LevelWarn, "sr auto-switch selected account below new-session headroom threshold", "account", picked.ID, "threshold", selectacct.MinNewSessionHeadroom)
	}
	if cfg.SwitchActive != nil {
		if err := cfg.SwitchActive(ctx, picked.ID); err != nil {
			return "", err
		}
	}
	return picked.ID, nil
}

func codexOAuthAccounts(all []accounts.Account) []accounts.Account {
	out := make([]accounts.Account, 0, len(all))
	for _, account := range all {
		if account.AuthMode == accounts.AuthModeOAuth &&
			(account.Provider == "" || account.Provider == accounts.ProviderCodex) {
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
