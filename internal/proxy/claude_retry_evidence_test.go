package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
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
	got := claudeRetryCandidatesFromScores(scheduler, scheduler, []accounts.Account{unknown, verified}, "", time.Now())
	if len(got) != 1 || got[0].ID != "verified" {
		t.Fatalf("candidate set = %+v; expected the known healthy account before unknown quota", got)
	}
}

func TestClaudeRetryPrefersAnyFreshHeadroomOverUnknownQuota(t *testing.T) {
	fresh := claudeRetryTestAccount("fresh")
	unknown := claudeRetryTestAccount("unknown")
	scheduler := selectacct.NewScheduler([]selectacct.Score{
		claudeRetryTestScore("fresh", 0.08, true),
	})
	got := claudeRetryCandidatesFromScores(scheduler, scheduler, []accounts.Account{unknown, fresh}, "", time.Now())
	if len(got) != 1 || got[0].ID != fresh.ID {
		t.Fatalf("candidate set = %+v; expected fresh headroom before unknown quota", got)
	}
}

func TestClaudeRetrySkipsConfirmedExhaustionUntilReset(t *testing.T) {
	cooked := claudeRetryTestAccount("cooked")
	unknown := claudeRetryTestAccount("unknown")
	quota := claudeRetryTestScore("cooked", 0, true)
	quota.ExhaustedResetAt = time.Now().Add(time.Hour)
	scheduler := selectacct.NewScheduler([]selectacct.Score{quota})
	got := claudeRetryCandidatesFromScores(scheduler, scheduler, []accounts.Account{cooked, unknown}, "", time.Now())
	if len(got) != 1 || got[0].ID != "unknown" {
		t.Fatalf("retry set = %+v; expected to skip quota until its reported reset", got)
	}
	if got := claudeRetryCandidatesFromScores(scheduler, scheduler, []accounts.Account{cooked}, "", time.Now()); len(got) != 0 {
		t.Fatalf("all-known-exhausted pool must not send additional upstream attempts: %+v", got)
	}
	// A past reset is not evidence that the account remains exhausted.
	if got := claudeRetryCandidatesFromScores(scheduler, scheduler, []accounts.Account{cooked}, "", time.Now().Add(2*time.Hour)); len(got) != 1 {
		t.Fatalf("account was not retryable after reported quota reset: %+v", got)
	}
}

func TestClaudeRetrySkipsKnownFutureResetBeforeUnknownQuota(t *testing.T) {
	stale := claudeRetryTestAccount("stale")
	unknown := claudeRetryTestAccount("unknown")
	quota := claudeRetryTestScore("stale", 0, false)
	quota.ExhaustedResetAt = time.Now().Add(time.Hour)
	scheduler := selectacct.NewScheduler([]selectacct.Score{quota})
	got := claudeRetryCandidatesFromScores(scheduler, scheduler, []accounts.Account{stale, unknown}, "", time.Now())
	if len(got) != 1 || got[0].ID != unknown.ID {
		t.Fatalf("known future reset must stay out of the retry set: %+v", got)
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
	got := claudeRetryCandidatesFromScores(base, model, []accounts.Account{unknown, good}, "claude-opus", time.Now())
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
		Accounts:      []accounts.Account{cooked},
		SchedulerRef:  selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{quota})),
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

func TestClaudeRetrySkipsKnownUnsupportedModelButAllowsUnknownEvidence(t *testing.T) {
	model := "claude-opus"
	supported := claudeRetryTestScore("supported", 0.7, true)
	supported.ModelScores = map[string]selectacct.Score{
		selectacct.ModelKey(model): claudeRetryTestScore("supported", 0.6, true),
	}
	unsupported := claudeRetryTestScore("unsupported", 0.8, true)
	// Claude treats a missing model bucket in a fresh quota reading as
	// unsupported when a dedicated model pool is known to exist.
	uncertain := claudeRetryTestScore("uncertain", 0.6, false)
	base := selectacct.NewScheduler([]selectacct.Score{supported, unsupported, uncertain})
	pool := base.ForModel(model)

	got := claudeRetryCandidatesFromScores(base, pool,
		[]accounts.Account{claudeRetryTestAccount("unsupported"), claudeRetryTestAccount("uncertain")},
		model, time.Now())
	if len(got) != 1 || got[0].ID != "uncertain" {
		t.Fatalf("retry should skip verified unsupported model and retain unknown: %+v", got)
	}
	got = claudeRetryCandidatesFromScores(base, pool,
		[]accounts.Account{claudeRetryTestAccount("unsupported")}, model, time.Now())
	if len(got) != 0 {
		t.Fatalf("known unsupported model must not receive a futile retry: %+v", got)
	}
}


func TestClaudeNewSessionPrefersVerifiedQuota(t *testing.T) {
	unknown := claudeRetryTestAccount("unknown")
	verified := claudeRetryTestAccount("verified")
	base := selectacct.NewScheduler([]selectacct.Score{
		claudeRetryTestScore(verified.ID, 0.7, true),
	})
	got := claudeNewSessionCandidatesFromScores(
		base, base, []accounts.Account{unknown, verified}, "",
	)
	if len(got) != 1 || got[0].ID != verified.ID {
		t.Fatalf("new-session candidates = %+v, want the measured healthy account", got)
	}
}

func TestClaudeNewSessionKeepsUnknownFallbackWhenNoVerifiedAdmission(t *testing.T) {
	unknown := claudeRetryTestAccount("unknown")
	low := claudeRetryTestAccount("low")
	stale := claudeRetryTestAccount("stale")
	base := selectacct.NewScheduler([]selectacct.Score{
		claudeRetryTestScore(low.ID, 0.2, true),
		claudeRetryTestScore(stale.ID, 0.9, false),
	})
	candidates := []accounts.Account{unknown, low, stale}
	got := claudeNewSessionCandidatesFromScores(base, base, candidates, "")
	if len(got) != len(candidates) {
		t.Fatalf("low/stale scores improperly disabled optimistic fallback: %+v", got)
	}
	for i := range candidates {
		if got[i].ID != candidates[i].ID {
			t.Fatalf("candidate order changed without verified headroom: %+v", got)
		}
	}
}

func TestClaudeNewSessionNeedsMeasuredModelPoolEvidence(t *testing.T) {
	modelName := "claude-opus"
	unknownModel := claudeRetryTestAccount("wide-only")
	verifiedModel := claudeRetryTestAccount("opus-confirmed")
	wide := claudeRetryTestScore(unknownModel.ID, 0.95, true)
	opus := claudeRetryTestScore(verifiedModel.ID, 0.7, true)
	opus.ModelScores = map[string]selectacct.Score{
		selectacct.ModelKey(modelName): claudeRetryTestScore(verifiedModel.ID, 0.6, true),
	}
	base := selectacct.NewScheduler([]selectacct.Score{wide, opus})
	got := claudeNewSessionCandidatesFromScores(
		base, base.ForModel(modelName),
		[]accounts.Account{unknownModel, verifiedModel}, modelName,
	)
	if len(got) != 1 || got[0].ID != verifiedModel.ID {
		t.Fatalf("new-session placement treated missing model quota as verified: %+v", got)
	}
}

func TestClaudeFirstPlacementPrefersVerifiedQuotaAndKeepsAffinity(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	unknown := claudeRetryTestAccount("unknown")
	verified := claudeRetryTestAccount("verified")
	base := selectacct.NewScheduler([]selectacct.Score{
		claudeRetryTestScore(verified.ID, 0.7, true),
	})
	server := Server{
		Accounts:      []accounts.Account{unknown, verified},
		Sessions:      store,
		SchedulerRef:  selectacct.NewSchedulerRef(base),
		UsageScoreTTL: time.Hour,
		MaxBodyBytes:  1 << 20,
	}
	newRequest := func() *http.Request {
		return httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[]}`))
	}
	first, _, _, err := server.accountForSessionProvider(
		accounts.ProviderClaude, "claude", "session-first-placement", newRequest(),
	)
	if err != nil || first.ID != verified.ID {
		t.Fatalf("initial placement=%+v err=%v, want confirmed quota", first, err)
	}
	// A healthy sticky session keeps its serving account even when the
	// other account receives better quota evidence after initial placement.
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		claudeRetryTestScore(unknown.ID, 0.95, true),
		claudeRetryTestScore(verified.ID, 0.65, true),
	}))
	sticky, _, _, err := server.accountForSessionProvider(
		accounts.ProviderClaude, "claude", "session-first-placement", newRequest(),
	)
	if err != nil || sticky.ID != verified.ID {
		t.Fatalf("sticky placement=%+v err=%v, should retain existing account", sticky, err)
	}
	// An explicit user preference remains authoritative for a fresh session.
	preferred, _, _, err := server.accountForSessionProviderWithOptions(
		accounts.ProviderClaude, "claude", "session-explicit-preference", newRequest(),
		accountSelectionOptions{preferredAccountID: verified.ID},
	)
	if err != nil || preferred.ID != verified.ID {
		t.Fatalf("preferred placement=%+v err=%v, requested preference lost", preferred, err)
	}
}
