package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
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
	best, err := schedulerRef.Get().Pick([]accounts.Account{
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

func TestSRAutoSwitchDoesNotClobberClaudePool(t *testing.T) {
	// The shared ref already holds a healthy Claude pool (maintained elsewhere
	// by the proxy's provider-aware refresh).
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "claude@x", Provider: accounts.ProviderClaude, Headroom: 0.90, ShortHeadroom: 0.90, Fresh: true},
	}))
	_, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "codex@x", AuthMode: accounts.AuthModeOAuth},
			{ID: "claude@x", AuthMode: accounts.AuthModeOAuth, Provider: accounts.ProviderClaude},
		},
		SchedulerRef: schedulerRef,
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			for _, candidate := range candidates {
				if candidate.Provider == accounts.ProviderClaude {
					t.Fatalf("auto-switch scored a Claude account through the Codex path: %#v", candidate)
				}
			}
			return []selectacct.Score{{AccountID: "codex@x", Headroom: 0.80, ShortHeadroom: 0.80, Fresh: true}}, 1
		},
		SwitchActive: func(context.Context, string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	sched := schedulerRef.Get()
	if sched.Exhausted(accounts.ProviderClaude, "claude@x") {
		t.Fatal("Claude account exhausted after Codex auto-switch (pool clobbered)")
	}
	if got := sched.ScoreFor(accounts.ProviderClaude, "claude@x").Headroom; got != 0.90 {
		t.Fatalf("Claude headroom = %v, want 0.90 preserved", got)
	}
}

func TestCodexOAuthAccountsFilter(t *testing.T) {
	got := codexOAuthAccounts([]accounts.Account{
		{ID: "bare", AuthMode: accounts.AuthModeOAuth},
		{ID: "codex", AuthMode: accounts.AuthModeOAuth, Provider: accounts.ProviderCodex},
		{ID: "claude", AuthMode: accounts.AuthModeOAuth, Provider: accounts.ProviderClaude},
		{ID: "apikey", AuthMode: accounts.AuthModeAPIKey},
	})
	if len(got) != 2 {
		t.Fatalf("got %d accounts, want 2 (bare + codex)", len(got))
	}
	for _, a := range got {
		if a.ID != "bare" && a.ID != "codex" {
			t.Fatalf("unexpected account kept by codexOAuthAccounts: %#v", a)
		}
	}
}
