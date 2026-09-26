package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestSRAutoSwitchPicksBestOAuthAccount(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	var switchedTo string

	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth},
			{ID: "apikey:paid", AuthMode: accounts.AuthModeAPIKey},
		},
		Sessions:     store,
		SchedulerRef: schedulerRef,
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			for _, candidate := range candidates {
				if candidate.AuthMode != accounts.AuthModeOAuth {
					t.Fatalf("auto-switch scored non-OAuth account: %#v", candidate)
				}
			}
			return []selectacct.Score{
				{AccountID: "a@example.com", Headroom: 0.50, ShortHeadroom: 0.50},
				{AccountID: "b@example.com", Headroom: 0.90, ShortHeadroom: 0.90},
				{AccountID: "apikey:paid", Headroom: 1.00, ShortHeadroom: 1.00},
			}, 2
		},
		SwitchActive: func(_ context.Context, accountID string) error {
			switchedTo = accountID
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if picked != "b@example.com" {
		t.Fatalf("picked = %q, want b@example.com", picked)
	}
	if switchedTo != "b@example.com" {
		t.Fatalf("switchedTo = %q, want b@example.com", switchedTo)
	}
	best, err := schedulerRef.Get().PickBest([]accounts.Account{
		{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth},
		{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if best.ID != "b@example.com" {
		t.Fatalf("scheduler ref pick = %q, want b@example.com", best.ID)
	}
}

func TestSRAutoSwitchWithoutAccountManagerHookOnlyPublishesScores(t *testing.T) {
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts:     []accounts.Account{{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth}},
		SchedulerRef: schedulerRef,
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{{AccountID: "healthy@example.com", Headroom: 0.9, ShortHeadroom: 0.9}}, 1
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked != "healthy@example.com" {
		t.Fatalf("picked = %q", picked)
	}
	selected, err := schedulerRef.Get().PickBest([]accounts.Account{{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth}})
	if err != nil || selected.ID != picked {
		t.Fatalf("published scheduler selected %+v, err=%v", selected, err)
	}
}

func TestSRAutoSwitchIgnoresClaudeAccountWithSameID(t *testing.T) {
	var switchedTo string
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "shared@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
			{ID: "healthy@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
			{ID: "shared@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		},
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			if len(candidates) != 2 {
				t.Fatalf("candidates = %#v, want only two Codex accounts", candidates)
			}
			for _, candidate := range candidates {
				if candidate.Provider != accounts.ProviderCodex {
					t.Fatalf("auto-switch scored non-Codex account: %#v", candidate)
				}
			}
			return []selectacct.Score{
				{AccountID: "shared@example.com", Provider: accounts.ProviderCodex, Headroom: 0, ShortHeadroom: 0},
				{AccountID: "healthy@example.com", Provider: accounts.ProviderCodex, Headroom: 0.30, ShortHeadroom: 0.30},
			}, 2
		},
		SwitchActive: func(_ context.Context, accountID string) error {
			switchedTo = accountID
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked != "healthy@example.com" || switchedTo != "healthy@example.com" {
		t.Fatalf("picked=%q switchedTo=%q, want healthy@example.com", picked, switchedTo)
	}
}

func TestSRAutoSwitchPreservesOtherProviderScores(t *testing.T) {
	nonCodexScores := []selectacct.Score{
		{AccountID: "claude", Provider: accounts.ProviderClaude, Headroom: 0.21, ShortHeadroom: 0.22},
		{AccountID: "kimi", Provider: accounts.ProviderKimi, Headroom: 0.31, ShortHeadroom: 0.32},
		{AccountID: "grok", Provider: accounts.ProviderGrok, Headroom: 0.41, ShortHeadroom: 0.42},
		{AccountID: "antigravity", Provider: accounts.ProviderAntigravity, Headroom: 0.51, ShortHeadroom: 0.52},
	}
	initialScores := append([]selectacct.Score{
		{AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.95, ShortHeadroom: 0.95},
		{AccountID: "codex-b", Provider: accounts.ProviderCodex, Headroom: 0.05, ShortHeadroom: 0.05},
	}, nonCodexScores...)
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(initialScores))

	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
			{ID: "codex-b", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
			{ID: "claude", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
			{ID: "kimi", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth},
			{ID: "grok", Provider: accounts.ProviderGrok, AuthMode: accounts.AuthModeOAuth},
			{ID: "antigravity", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth},
		},
		SchedulerRef: schedulerRef,
		SwitchActive: func(context.Context, string) error { return nil },
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			if len(candidates) != 2 {
				t.Fatalf("candidates = %#v, want only two Codex accounts", candidates)
			}
			for _, candidate := range candidates {
				if candidate.Provider != accounts.ProviderCodex {
					t.Fatalf("auto-switch scored non-Codex account: %#v", candidate)
				}
			}
			return []selectacct.Score{
				{AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.10, ShortHeadroom: 0.10},
				{AccountID: "codex-b", Provider: accounts.ProviderCodex, Headroom: 0.90, ShortHeadroom: 0.90},
			}, 2
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked != "codex-b" {
		t.Fatalf("picked = %q, want codex-b", picked)
	}

	published := schedulerRef.Get()
	for _, want := range nonCodexScores {
		got := published.ScoreFor(want.Provider, want.AccountID)
		if got.Headroom != want.Headroom || got.ShortHeadroom != want.ShortHeadroom {
			t.Errorf("published %s score = %+v, want preserved %+v", want.Provider, got, want)
		}
	}
	if got := published.ScoreFor(accounts.ProviderCodex, "codex-a"); got.Headroom != 0.10 {
		t.Errorf("published Codex score = %+v, want refreshed headroom 0.10", got)
	}
}

func TestSRAutoSwitchRetriesPublicationWithoutRefetchingAfterConcurrentFullRefresh(t *testing.T) {
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.5, ShortHeadroom: 0.5,
	}}))
	fetches := 0
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{{
			ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
		}},
		SchedulerRef: schedulerRef,
		SwitchActive: func(context.Context, string) error { return nil },
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			fetches++
			if fetches == 1 {
				revision := schedulerRef.ScoreRevision()
				if !schedulerRef.SetForAccountGenerationAtScoreRevision(selectacct.NewScheduler([]selectacct.Score{{
					AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8,
				}}), 0, revision) {
					t.Fatal("concurrent full refresh was rejected")
				}
			}
			return []selectacct.Score{{
				AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.2, ShortHeadroom: 0.2,
			}}, 1
		},
	})
	if err != nil || picked != "codex-a" {
		t.Fatalf("auto-switch picked %q with error %v", picked, err)
	}
	if fetches != 1 {
		t.Fatalf("score fetches = %d, want one fetch with publication retry", fetches)
	}
	if got := schedulerRef.Get().ScoreFor(accounts.ProviderCodex, "codex-a").Headroom; got != 0.2 {
		t.Fatalf("Codex headroom = %v, want retried fresh value 0.2", got)
	}
}

func TestSRAutoSwitchScoreRevisionConflictDoesNotRepeatUsageSweep(t *testing.T) {
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	fetches := 0
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{{
			ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
		}},
		SchedulerRef: schedulerRef,
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			fetches++
			revision := schedulerRef.ScoreRevision()
			if !schedulerRef.SetForAccountGenerationAtScoreRevision(selectacct.NewScheduler(nil), 0, revision) {
				t.Fatal("test could not advance scheduler revision")
			}
			return []selectacct.Score{{
				AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8,
			}}, 1
		},
	})
	if err != nil || picked != "codex-a" {
		t.Fatalf("auto-switch picked %q with error %v", picked, err)
	}
	if fetches != 1 {
		t.Fatalf("score fetches = %d, want one fetch across CAS retry", fetches)
	}
}

func TestSRAutoSwitchLocalGenerationConflictDoesNotRepeatStableAccountSweep(t *testing.T) {
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	generation := uint64(0)
	fetches := 0
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		AccountsSnapshotFunc: func() ([]accounts.Account, uint64) {
			return []accounts.Account{{
				ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
			}}, generation
		},
		SchedulerRef: schedulerRef,
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			fetches++
			generation++
			schedulerRef.AdvanceAccountGeneration(generation)
			return []selectacct.Score{{
				AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8,
			}}, 1
		},
	})
	if err != nil || picked != "codex-a" {
		t.Fatalf("auto-switch picked %q with error %v", picked, err)
	}
	if fetches != 1 {
		t.Fatalf("score fetches = %d, want one stable account-set fetch", fetches)
	}
}

func TestSRAutoSwitchRefetchesAfterAccountSetChanges(t *testing.T) {
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	generation := uint64(0)
	accountID := "codex-a"
	fetches := 0
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		AccountsSnapshotFunc: func() ([]accounts.Account, uint64) {
			return []accounts.Account{{
				ID: accountID, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
			}}, generation
		},
		SchedulerRef: schedulerRef,
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			fetches++
			measured := candidates[0].ID
			if fetches == 1 {
				accountID = "codex-b"
				generation++
				schedulerRef.AdvanceAccountGeneration(generation)
			}
			return []selectacct.Score{{
				AccountID: measured, Provider: accounts.ProviderCodex, Headroom: .8, ShortHeadroom: .8,
			}}, 1
		},
	})
	if err != nil || picked != "codex-b" {
		t.Fatalf("auto-switch picked %q with error %v", picked, err)
	}
	if fetches != 2 {
		t.Fatalf("score fetches = %d, want one fetch per account-set generation", fetches)
	}
}

func TestSRAutoSwitchUsesLiveAccountsFunc(t *testing.T) {
	var switchedTo string
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "old@example.com", AuthMode: accounts.AuthModeOAuth},
		},
		AccountsFunc: func() []accounts.Account {
			return []accounts.Account{
				{ID: "new@example.com", AuthMode: accounts.AuthModeOAuth},
			}
		},
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			if len(candidates) != 1 || candidates[0].ID != "new@example.com" {
				t.Fatalf("candidates = %#v, want live account", candidates)
			}
			return []selectacct.Score{{AccountID: "new@example.com", Headroom: 0.90, ShortHeadroom: 0.90}}, 1
		},
		SwitchActive: func(_ context.Context, accountID string) error {
			switchedTo = accountID
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked != "new@example.com" || switchedTo != "new@example.com" {
		t.Fatalf("picked=%q switched=%q, want new@example.com", picked, switchedTo)
	}
}

func TestSRAutoSwitchSkipsWhenUsageUnavailable(t *testing.T) {
	ran := false
	_, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth}},
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{{AccountID: "a@example.com", Headroom: 0, ShortHeadroom: 0}}, 0
		},
		SwitchActive: func(context.Context, string) error {
			ran = true
			return nil
		},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if ran {
		t.Fatal("sr switch ran despite missing fresh usage")
	}
}

func TestSRAutoSwitchUsesBestNonExhaustedOAuthBelowProtectedHeadroom(t *testing.T) {
	var switchedTo string
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth},
		},
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{
				{AccountID: "a@example.com", Headroom: 0.12, ShortHeadroom: 0.12},
				{AccountID: "b@example.com", Headroom: 0.39, ShortHeadroom: 0.39},
			}, 2
		},
		SwitchActive: func(_ context.Context, accountID string) error {
			switchedTo = accountID
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked != "b@example.com" || switchedTo != "b@example.com" {
		t.Fatalf("picked=%q switchedTo=%q, want b@example.com", picked, switchedTo)
	}
}

func TestSRAutoSwitchSkipsWhenOAuthAccountsExhausted(t *testing.T) {
	ran := false
	_, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth},
		},
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{
				{AccountID: "a@example.com", Headroom: 0, ShortHeadroom: 0},
				{AccountID: "b@example.com", Headroom: 0, ShortHeadroom: 0},
			}, 2
		},
		SwitchActive: func(context.Context, string) error {
			ran = true
			return nil
		},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if ran {
		t.Fatal("sr switch ran despite exhausted OAuth accounts")
	}
}
