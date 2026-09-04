package proxy

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Regression: a usage-score refresh must never demote a healthy account to
// exhausted using stale data. The upstream usage endpoint rate-limits under
// load, so the per-request re-score read stale "last good" cooked windows and
// overwrote the scheduler's healthy scores with zeros, after which the router
// routed traffic to dead accounts. scoreAccounts must preserve the last known
// score when it cannot fetch FRESH usage.
func TestScoreAccountsPreservesHealthyScoreWhenUsageIsStale(t *testing.T) {
	transport := &usageRoundTripper{responses: []*http.Response{usage429Response()}}
	ref := cacheTestAccountRef(t, transport)
	account := accounts.Account{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}

	// Seed a STALE, cooked last-known-good usage entry so the live (rate-limited
	// 429) fetch falls back to it and reports fresh=false.
	key := account.ID + "\x00" + string(account.Provider)
	ref.usageWindowsMu.Lock()
	if ref.usageWindows == nil {
		ref.usageWindows = map[string]usageWindowsEntry{}
	}
	ref.usageWindows[key] = usageWindowsEntry{
		windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60}},
		at:      time.Now().Add(-usageWindowsTTL - time.Minute),
	}
	ref.usageWindowsMu.Unlock()

	server := Server{
		AccountRef: ref,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "claude@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		})),
	}

	scores, scored := server.scoreAccounts(context.Background(), []accounts.Account{account})
	if scored != 0 {
		t.Fatalf("scored = %d, want 0 (stale usage must not count as a fresh score)", scored)
	}
	if len(scores) != 1 {
		t.Fatalf("scores = %d, want 1", len(scores))
	}
	if scores[0].Headroom <= 0 || scores[0].ShortHeadroom <= 0 {
		t.Fatalf("healthy account clobbered to exhausted by stale usage: %+v", scores[0])
	}
}

func TestScoreAccountsTreatsAuthOnlyOAuthRefreshAsAuthEvidenceOnly(t *testing.T) {
	account := accounts.Account{
		ID: "antigravity", Provider: accounts.ProviderAntigravity,
		AuthMode: accounts.AuthModeOAuth, Token: "access",
	}
	source := &stubOAuthSource{
		provider:  accounts.ProviderAntigravity,
		listed:    []accounts.Account{account},
		refreshed: accounts.Account{ID: account.ID, Provider: account.Provider, AuthMode: account.AuthMode, Token: "fresh"},
	}
	networkCalls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		networkCalls++
		return nil, errors.New("unexpected quota request")
	})}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, client)
	ref.oauthSources = []OAuthAccountSource{source}
	server := Server{
		AccountRef: ref,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
			AccountID: account.ID, Provider: account.Provider, Headroom: 0.63, ShortHeadroom: 0.61,
		}})),
	}

	scores, scored := server.scoreAccounts(t.Context(), []accounts.Account{account})
	if source.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want one auth check", source.refreshCalls)
	}
	if networkCalls != 0 {
		t.Fatalf("unsupported quota endpoint was polled %d time(s)", networkCalls)
	}
	if scored != 0 || len(scores) != 1 || scores[0].Headroom != 0.63 || scores[0].ShortHeadroom != 0.61 || scores[0].Fresh {
		t.Fatalf("auth-only routing score = %+v, scored=%d", scores, scored)
	}
}

func TestAntigravityFamilyQuotaRoutesWithoutCollapsingOtherFamily(t *testing.T) {
	accountA := accounts.Account{ID: "agy-a", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth}
	accountB := accounts.Account{ID: "agy-b", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth}
	scores := []selectacct.Score{
		scoreFromUsageWindows(accounts.ProviderAntigravity, accountA.ID, []accounts.UsageWindow{
			{Name: "gemini 5h", Feature: "gemini", UsedPercent: 100, LimitWindowSeconds: 18000},
			{Name: "gemini weekly", Feature: "gemini", UsedPercent: 100, LimitWindowSeconds: 604800},
			{Name: "claude-gpt 5h", Feature: "claude-gpt", UsedPercent: 10, LimitWindowSeconds: 18000},
			{Name: "claude-gpt weekly", Feature: "claude-gpt", UsedPercent: 20, LimitWindowSeconds: 604800},
		}),
		scoreFromUsageWindows(accounts.ProviderAntigravity, accountB.ID, []accounts.UsageWindow{
			{Name: "gemini 5h", Feature: "gemini", UsedPercent: 20, LimitWindowSeconds: 18000},
			{Name: "gemini weekly", Feature: "gemini", UsedPercent: 30, LimitWindowSeconds: 604800},
			{Name: "claude-gpt 5h", Feature: "claude-gpt", UsedPercent: 100, LimitWindowSeconds: 18000},
			{Name: "claude-gpt weekly", Feature: "claude-gpt", UsedPercent: 100, LimitWindowSeconds: 604800},
		}),
	}
	scheduler := selectacct.NewScheduler(scores)
	gemini, err := scheduler.ForModel(antigravityPoolModel(scheduler, "gemini-3.1-pro")).Pick([]accounts.Account{accountA, accountB})
	if err != nil || gemini.ID != accountB.ID {
		t.Fatalf("Gemini picked %+v err=%v, want %s", gemini, err, accountB.ID)
	}
	claude, err := scheduler.ForModel(antigravityPoolModel(scheduler, "claude-sonnet-4.5")).Pick([]accounts.Account{accountA, accountB})
	if err != nil || claude.ID != accountA.ID {
		t.Fatalf("Claude picked %+v err=%v, want %s", claude, err, accountA.ID)
	}
	if scheduler.ForModel("claude-gpt").Exhausted(accounts.ProviderAntigravity, accountA.ID) {
		t.Fatal("Gemini exhaustion collapsed account A's Claude/GPT pool")
	}
	if scheduler.ForModel("gemini").Exhausted(accounts.ProviderAntigravity, accountB.ID) {
		t.Fatal("Claude/GPT exhaustion collapsed account B's Gemini pool")
	}
}

func TestAntigravityPoolModelAndTenantLeaseUseSameFamilies(t *testing.T) {
	for _, test := range []struct{ model, want string }{
		{"gemini-3.1-pro", "gemini"},
		{"claude-sonnet-4.5", "claude-gpt"},
		{"gpt-oss-120b", "claude-gpt"},
		{"future-model", "future-model"},
	} {
		if got := antigravityFamilyPoolModel(test.model); got != test.want {
			t.Fatalf("antigravityFamilyPoolModel(%q) = %q, want %q", test.model, got, test.want)
		}
		wantLease := selectacct.ModelKey(test.want)
		if got := tenantCredentialLeasePoolModel(accounts.ProviderAntigravity, test.model); got != wantLease {
			t.Fatalf("tenant pool(%q) = %q, want %q", test.model, got, wantLease)
		}
	}
}

func TestAntigravityPartialFamilyTelemetryKeepsMissingFamilyEligible(t *testing.T) {
	partial := scoreFromUsageWindows(accounts.ProviderAntigravity, "partial", []accounts.UsageWindow{
		{Name: "gemini 5h", Feature: "gemini", UsedPercent: 20, LimitWindowSeconds: 18000},
	})
	complete := scoreFromUsageWindows(accounts.ProviderAntigravity, "complete", []accounts.UsageWindow{
		{Name: "gemini 5h", Feature: "gemini", UsedPercent: 10, LimitWindowSeconds: 18000},
		{Name: "claude-gpt 5h", Feature: "claude-gpt", UsedPercent: 90, LimitWindowSeconds: 18000},
	})
	scheduler := selectacct.NewScheduler([]selectacct.Score{partial, complete})
	claudePool := scheduler.ForModel(antigravityPoolModel(scheduler, "claude-sonnet-4.5"))
	if claudePool.Exhausted(accounts.ProviderAntigravity, "partial") {
		t.Fatal("missing Claude/GPT telemetry was converted to exhausted")
	}
	picked, err := claudePool.Pick([]accounts.Account{
		{ID: "complete", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth},
		{ID: "partial", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth},
	})
	if err != nil || picked.ID != "partial" {
		t.Fatalf("picked %+v err=%v, want optimistic partial account", picked, err)
	}
}

func TestAntigravityLegacyModelsUseExactPoolsWithinOneFamily(t *testing.T) {
	score := scoreFromUsageWindows(accounts.ProviderAntigravity, "mixed", []accounts.UsageWindow{
		{Name: "claude-sonnet-4.5", Feature: "claude-sonnet-4.5", UsedPercent: 100},
		{Name: "claude-opus-4.1", Feature: "claude-opus-4.1", UsedPercent: 10},
	})
	scheduler := selectacct.NewScheduler([]selectacct.Score{score})
	if got := antigravityPoolModel(scheduler, "claude-sonnet-4.5"); got != "claude-sonnet-4.5" {
		t.Fatalf("Sonnet pool = %q", got)
	}
	if !scheduler.ForModel("claude-sonnet-4.5").Exhausted(accounts.ProviderAntigravity, "mixed") {
		t.Fatal("exhausted exact Sonnet pool appeared usable")
	}
	if scheduler.ForModel("claude-opus-4.1").Exhausted(accounts.ProviderAntigravity, "mixed") {
		t.Fatal("Sonnet exhaustion collapsed healthy exact Opus pool")
	}
	if got := tenantCredentialLeasePoolModel(accounts.ProviderAntigravity, "claude-opus-4.1", scheduler); got != selectacct.ModelKey("claude-opus-4.1") {
		t.Fatalf("tenant exact pool = %q", got)
	}
}

func TestAntigravityLegacyModelMissingOnOneAccountRemainsUnknown(t *testing.T) {
	partialAccount := accounts.Account{ID: "partial", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth}
	observedAccount := accounts.Account{ID: "observed", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth}
	partial := scoreFromUsageWindows(accounts.ProviderAntigravity, partialAccount.ID, []accounts.UsageWindow{
		{Name: "claude-sonnet-4.5", Feature: "claude-sonnet-4.5", UsedPercent: 20},
	})
	observed := scoreFromUsageWindows(accounts.ProviderAntigravity, observedAccount.ID, []accounts.UsageWindow{
		{Name: "claude-opus-4.1", Feature: "claude-opus-4.1", UsedPercent: 95},
	})
	scheduler := selectacct.NewScheduler([]selectacct.Score{partial, observed})
	pool := scheduler.ForModel(antigravityPoolModel(scheduler, "claude-opus-4.1"))
	if pool.Exhausted(accounts.ProviderAntigravity, partialAccount.ID) {
		t.Fatal("missing exact legacy model was treated as exhausted")
	}
	picked, err := pool.Pick([]accounts.Account{observedAccount, partialAccount})
	if err != nil || picked.ID != partialAccount.ID {
		t.Fatalf("picked %+v err=%v, want unknown partial account", picked, err)
	}
}

func TestAntigravityPoolMappingIgnoresClaudeExactPoolCollision(t *testing.T) {
	claude := scoreFromUsageWindows(accounts.ProviderClaude, "claude", []accounts.UsageWindow{
		{Name: "sonnet", Feature: "claude-sonnet-4.5", UsedPercent: 10},
	})
	exhausted := scoreFromUsageWindows(accounts.ProviderAntigravity, "agy-exhausted", []accounts.UsageWindow{
		{Name: "claude-gpt 5h", Feature: "claude-gpt", UsedPercent: 100, LimitWindowSeconds: 18000},
	})
	healthy := scoreFromUsageWindows(accounts.ProviderAntigravity, "agy-healthy", []accounts.UsageWindow{
		{Name: "claude-gpt 5h", Feature: "claude-gpt", UsedPercent: 20, LimitWindowSeconds: 18000},
	})
	scheduler := selectacct.NewScheduler([]selectacct.Score{claude, exhausted, healthy})
	pool := antigravityPoolModel(scheduler, "claude-sonnet-4.5")
	if pool != "claude-gpt" {
		t.Fatalf("AGY pool = %q, want family despite Claude exact pool", pool)
	}
	picked, err := scheduler.ForModel(pool).Pick([]accounts.Account{
		{ID: "agy-exhausted", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth},
		{ID: "agy-healthy", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth},
	})
	if err != nil || picked.ID != "agy-healthy" {
		t.Fatalf("picked %+v err=%v, want healthy family account", picked, err)
	}
	if got := tenantCredentialLeasePoolModel(accounts.ProviderAntigravity, "claude-sonnet-4.5", scheduler); got != selectacct.ModelKey("claude-gpt") {
		t.Fatalf("tenant pool = %q, want AGY family", got)
	}
}

func TestAuthOnlyOAuthRefreshPreservesRequestTimeExhaustion(t *testing.T) {
	account := accounts.Account{
		ID: "antigravity", Provider: accounts.ProviderAntigravity,
		AuthMode: accounts.AuthModeOAuth, Token: "access",
	}
	source := &stubOAuthSource{
		provider:  accounts.ProviderAntigravity,
		listed:    []accounts.Account{account},
		refreshed: accounts.Account{ID: account.ID, Provider: account.Provider, AuthMode: account.AuthMode, Token: "fresh"},
	}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
	ref.oauthSources = []OAuthAccountSource{source}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: account.ID, Provider: account.Provider, Headroom: 1, ShortHeadroom: 1,
	}}))
	_, generation, revision := ref.CredentialSnapshot()
	schedulerRef.AdvanceAccountGenerationWithAccounts(generation, revision, SchedulerAccounts([]accounts.Account{account}))
	schedulerRef.MarkExhaustedUntil(account.Provider, account.ID, "", time.Now().Add(time.Hour))
	server := Server{AccountRef: ref, SchedulerRef: schedulerRef, UsageScoreTTL: time.Nanosecond}

	server.refreshUsageScoresIfStale(t.Context())
	if source.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want one auth-only refresh", source.refreshCalls)
	}
	if !schedulerRef.Get().Exhausted(account.Provider, account.ID) {
		t.Fatal("auth-only token refresh cleared request-time quota exhaustion")
	}
}

// Regression: request-time exhaustion is an expiring overlay, not measured
// usage. If a refresh seeds its carried-forward score from that overlay and the
// mark expires before FinishRefresh, the zero is stranded in the base scheduler
// with no expiry. Simulate that ordering deterministically: score while marked,
// prune the mark, then publish the stale refresh result.
func TestScoreAccountsDoesNotBakeExpiringExhaustionOverlay(t *testing.T) {
	transport := &usageRoundTripper{responses: []*http.Response{usage429Response()}}
	accountRef := cacheTestAccountRef(t, transport)
	acct := accounts.Account{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	key := acct.ID + "\x00" + string(acct.Provider)
	accountRef.usageWindowsMu.Lock()
	accountRef.usageWindows = map[string]usageWindowsEntry{
		key: {
			windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60}},
			at:      time.Now().Add(-usageWindowsTTL - time.Minute),
		},
	}
	accountRef.usageWindowsMu.Unlock()

	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: acct.ID, Provider: acct.Provider, Headroom: 0.75, ShortHeadroom: 0.75,
	}}))
	ref.MarkExhaustedUntil(acct.Provider, acct.ID, "", time.Now().Add(time.Hour))
	server := Server{AccountRef: accountRef, SchedulerRef: ref}

	scores, scored := server.scoreAccounts(context.Background(), []accounts.Account{acct})
	if scored != 0 {
		t.Fatalf("scored = %d, want stale carried-forward score", scored)
	}
	if scores[0].Headroom != 0.75 || scores[0].ShortHeadroom != 0.75 {
		t.Fatalf("refresh seed included temporary exhaustion overlay: %+v", scores[0])
	}

	// Deterministically model the original mark expiring while scoring was in
	// flight, before the resulting scheduler is published.
	ref.MarkExhaustedUntil(acct.Provider, acct.ID, "", time.Now().Add(-time.Second))
	_ = ref.Get() // prune the lapsed mark
	ref.FinishRefresh(selectacct.NewScheduler(scores), true)
	got := ref.Get().ScoreFor(acct.Provider, acct.ID)
	if got.Headroom != 0.75 || got.ShortHeadroom != 0.75 || ref.Get().Exhausted(acct.Provider, acct.ID) {
		t.Fatalf("expired overlay was baked into scheduler base: %+v", got)
	}
}

// Regression: an account whose refresh token is dead (invalid_grant) only
// recovers via human re-auth, so probing it again costs a doomed round trip on
// a path that fronts proxy requests. With a fully expired Claude pool that made
// every scoring sweep pay N dead round trips and `sr` stopped responding.
// scoreAccounts must zero a known-dead account without any network call.
func TestScoreAccountsSkipsKnownDeadCredentialWithoutNetwork(t *testing.T) {
	transport := &usageRoundTripper{responses: []*http.Response{usageOKResponse()}}
	ref := cacheTestAccountRef(t, transport)
	account := accounts.Account{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}

	ref.noteCredResult(account, errors.New(`Claude OAuth refresh failed: 400 Bad Request: {"error": "invalid_grant"}`))

	server := Server{
		AccountRef: ref,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "claude@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		})),
	}

	scores, scored := server.scoreAccounts(context.Background(), []accounts.Account{account})
	if transport.calls != 0 {
		t.Fatalf("transport.calls = %d, want 0 (a dead credential must not be re-probed)", transport.calls)
	}
	if scored != 0 {
		t.Fatalf("scored = %d, want 0", scored)
	}
	if len(scores) != 1 {
		t.Fatalf("scores = %d, want 1", len(scores))
	}
	if scores[0].Headroom != 0 || scores[0].ShortHeadroom != 0 {
		t.Fatalf("dead credential must be zeroed out of routing: %+v", scores[0])
	}
}

// A re-authed account must rejoin routing immediately rather than waiting out
// the fast-fail TTL, so any non-terminal result clears the remembered failure.
func TestNoteCredResultClearsRememberedFailure(t *testing.T) {
	ref := &AccountRef{}
	account := accounts.Account{Provider: accounts.ProviderClaude, ID: "claude@example.com", Token: "credential"}

	ref.noteCredResult(account, errors.New("invalid_grant"))
	if _, dead := ref.terminalCredFailure(account); !dead {
		t.Fatal("terminal credential error was not remembered")
	}

	ref.noteCredResult(account, nil)
	if _, dead := ref.terminalCredFailure(account); dead {
		t.Fatal("a successful refresh must clear the remembered failure")
	}

	// A transient error is not a credential verdict and must not re-arm it.
	ref.noteCredResult(account, context.DeadlineExceeded)
	if _, dead := ref.terminalCredFailure(account); dead {
		t.Fatal("a timeout must not be treated as a dead credential")
	}
}

// Regression: a Codex account and a Claude account routinely share the same ID
// (a Codex email equals a Claude profile name). Mutating the account list
// (refresh/replace) must match provider too, or one provider's update silently
// overwrites the other's entry, dropping it from selection. This is exactly
// what hid the best Codex accounts (e.g. aziz@cmux.com) behind their Claude
// namesakes so Codex never routed to them.
func TestAccountRefReplaceDoesNotClobberAcrossProviders(t *testing.T) {
	ref := &AccountRef{
		accounts: []accounts.Account{
			{ID: "shared@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "codex-tok"},
			{ID: "shared@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "claude-tok"},
		},
	}

	// A Claude-side refresh must update only the Claude entry.
	ref.replace(accounts.Account{ID: "shared@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "claude-tok-2"})

	all := ref.All()
	var codex, claude *accounts.Account
	for i := range all {
		switch all[i].Provider {
		case accounts.ProviderCodex:
			codex = &all[i]
		case accounts.ProviderClaude:
			claude = &all[i]
		}
	}
	if codex == nil {
		t.Fatal("Codex account was clobbered by a same-ID Claude replace")
	}
	if codex.Token != "codex-tok" {
		t.Fatalf("Codex token = %q, want untouched codex-tok", codex.Token)
	}
	if claude == nil || claude.Token != "claude-tok-2" {
		t.Fatalf("Claude entry not updated: %+v", claude)
	}
}
