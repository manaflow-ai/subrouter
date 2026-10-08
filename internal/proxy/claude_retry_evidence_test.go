package proxy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func claudeRetryTestAccount(id string) accounts.Account {
	return accounts.Account{
		ID:       id,
		Provider: accounts.ProviderClaude,
		AuthMode: accounts.AuthModeOAuth,
		Token:    "token-" + id,
	}
}

func claudeRetryTestScore(id string, headroom float64, fresh bool) selectacct.Score {
	return selectacct.Score{
		Provider:       accounts.ProviderClaude,
		AccountID:      id,
		Headroom:       headroom,
		ShortHeadroom:  headroom,
		WeeklyHeadroom: headroom,
		Fresh:          fresh,
	}
}

func TestClaudeRetryPrefersConfirmedUsableQuotaOverOptimisticDefault(t *testing.T) {
	unknown := claudeRetryTestAccount("unknown")
	verified := claudeRetryTestAccount("verified")
	scheduler := selectacct.NewScheduler([]selectacct.Score{
		claudeRetryTestScore("verified", 0.7, true),
	})
	got := claudeRetryCandidatesFromScores(scheduler, scheduler, []accounts.Account{unknown, verified}, time.Now())
	if len(got) != 1 || got[0].ID != "verified" {
		t.Fatalf("candidate set = %+v; expected the known healthy account before unknown quota", got)
	}
}

func TestClaudeRetrySkipsConfirmedExhaustionUntilReset(t *testing.T) {
	cooked := claudeRetryTestAccount("cooked")
	unknown := claudeRetryTestAccount("unknown")
	quota := claudeRetryTestScore("cooked", 0, true)
	quota.ExhaustedResetAt = time.Now().Add(time.Hour)
	scheduler := selectacct.NewScheduler([]selectacct.Score{quota})
	got := claudeRetryCandidatesFromScores(scheduler, scheduler, []accounts.Account{cooked, unknown}, time.Now())
	if len(got) != 1 || got[0].ID != "unknown" {
		t.Fatalf("retry set = %+v; expected to skip quota until its reported reset", got)
	}
	if got := claudeRetryCandidatesFromScores(scheduler, scheduler, []accounts.Account{cooked}, time.Now()); len(got) != 0 {
		t.Fatalf("all-known-exhausted pool must not send additional upstream attempts: %+v", got)
	}
	// A past reset is not evidence that the account remains exhausted.
	if got := claudeRetryCandidatesFromScores(scheduler, scheduler, []accounts.Account{cooked}, time.Now().Add(2*time.Hour)); len(got) != 1 {
		t.Fatalf("account was not retryable after reported quota reset: %+v", got)
	}
}

func TestClaudeRetryAllowsStaleOrUnknownQuotaAsLastResort(t *testing.T) {
	stale := claudeRetryTestAccount("stale")
	unknown := claudeRetryTestAccount("unknown")
	quota := claudeRetryTestScore("stale", 0, false)
	quota.ExhaustedResetAt = time.Now().Add(time.Hour)
	scheduler := selectacct.NewScheduler([]selectacct.Score{quota})
	got := claudeRetryCandidatesFromScores(scheduler, scheduler, []accounts.Account{stale, unknown}, time.Now())
	if len(got) != 2 {
		t.Fatalf("stale exhaustion is not confirmed; all unverified candidates must remain available: %+v", got)
	}
}

func TestClaudeRetryChecksCurrentModelPoolAndBaseFreshness(t *testing.T) {
	unknown := claudeRetryTestAccount("unknown")
	good := claudeRetryTestAccount("good")
	modelKey := selectacct.ModelKey("claude-opus")
	quota := claudeRetryTestScore("good", 0.9, true)
	quota.ModelScores = map[string]selectacct.Score{
		modelKey: {
			Provider:      accounts.ProviderClaude,
			AccountID:     "good",
			Headroom:      0.7,
			ShortHeadroom: 0.7,
			// A model score from the latest base observation need not
			// duplicate the outer score's Fresh metadata.
		},
	}
	base := selectacct.NewScheduler([]selectacct.Score{quota})
	model := base.ForModel("claude-opus")
	got := claudeRetryCandidatesFromScores(base, model, []accounts.Account{unknown, good}, time.Now())
	if len(got) != 1 || got[0].ID != "good" {
		t.Fatalf("model failover ignored verified model headroom: %+v", got)
	}
}

func TestClaudeOAuthRetryCandidateUsesVerifiedPoolWithoutStatusFetch(t *testing.T) {
	unknown := claudeRetryTestAccount("unknown")
	verified := claudeRetryTestAccount("verified")
	original := claudeRetryTestAccount("original")
	refreshes := make([]string, 0, 1)
	server := Server{
		Accounts: []accounts.Account{original, unknown, verified},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			claudeRetryTestScore("verified", 0.70, true),
		})),
		UsageScoreTTL: 0,
		RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			refreshes = append(refreshes, account.ID)
			return account, nil
		},
	}
	picked, err := server.oauthRetryCandidate(
		context.Background(), accounts.ProviderClaude, "claude", "session-1", "",
		"", map[string]struct{}{"original": {}}, true, false,
	)
	if err != nil || picked.ID != "verified" {
		t.Fatalf("retry chose %+v, err=%v; expected confirmed healthy account", picked, err)
	}
	if len(refreshes) != 1 || refreshes[0] != "verified" {
		t.Fatalf("unnecessary retry/refresh attempts: %v", refreshes)
	}
}

func TestClaudeOAuthRetryCandidateStopsWhenEveryWindowIsConfirmedCooked(t *testing.T) {
	cooked := claudeRetryTestAccount("cooked")
	quota := claudeRetryTestScore("cooked", 0, true)
	quota.ExhaustedResetAt = time.Now().Add(time.Hour)
	refreshes := 0
	server := Server{
		Accounts: []accounts.Account{cooked},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{quota})),
		UsageScoreTTL: 0,
		RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			refreshes++
			return account, nil
		},
	}
	_, err := server.oauthRetryCandidate(
		context.Background(), accounts.ProviderClaude, "claude", "session-2", "",
		"", nil, true, false,
	)
	if err == nil || !strings.Contains(err.Error(), "confirmed quota exhaustion") {
		t.Fatalf("expected confirmed-exhaustion short circuit, got %v", err)
	}
	if refreshes != 0 {
		t.Fatalf("known-exhausted retry made %d unnecessary refresh attempts", refreshes)
	}
}
