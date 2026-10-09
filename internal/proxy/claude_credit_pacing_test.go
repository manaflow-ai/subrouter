package proxy

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

var creditTestNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func creditState(start time.Time, spent float64) claudeCreditKeyState {
	return claudeCreditKeyState{AmountUSD: 200, AmountSet: true, ExpirySet: true,
		PeriodStart: start, ExpiresAt: start.AddDate(0, 1, 0), SpentUSD: spent}
}

func TestClaudeCreditPaceScenarios(t *testing.T) {
	now := creditTestNow
	for _, tc := range []struct {
		name         string
		state        claudeCreditKeyState
		wantPace     string
		wantPromoted bool
	}{
		{name: "early month keeps plan first", state: creditState(now.Add(-24*time.Hour), 0), wantPace: claudeCreditPaceAhead},
		{name: "behind linear schedule is promoted", state: creditState(now.AddDate(0, 0, -15), 10), wantPace: claudeCreditPaceBehind, wantPromoted: true},
		{name: "behind but current burn drains it is on pace", state: func() claudeCreditKeyState {
			s := creditState(now.AddDate(0, 0, -15), 10)
			s.BurnUSDPerHour, s.BurnUpdatedAt = 1, now
			return s
		}(), wantPace: claudeCreditPaceOnPace},
		{name: "final 48 hours is promoted even on pace", state: func() claudeCreditKeyState {
			s := creditState(now.Add(24*time.Hour).AddDate(0, -1, 0), 150)
			s.BurnUSDPerHour, s.BurnUpdatedAt = 100, now
			return s
		}(), wantPace: claudeCreditPaceFinal, wantPromoted: true},
		{name: "metered out is not promoted", state: creditState(now.AddDate(0, 0, -15), 200), wantPace: claudeCreditPaceMetered0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := evaluateClaudeCredit("claude:k", tc.state, now)
			if view.Pace != tc.wantPace || view.Promoted != tc.wantPromoted {
				t.Fatalf("pace = %q promoted %v, want %q %v (%+v)", view.Pace, view.Promoted, tc.wantPace, tc.wantPromoted, view)
			}
		})
	}
}

func TestClaudeCreditPeriodRollResetsSpendAndHold(t *testing.T) {
	store := newClaudeCostStore(filepath.Join(t.TempDir(), "state.json"))
	clock := creditTestNow
	store.now = func() time.Time { return clock }
	expires := clock.Add(10 * 24 * time.Hour)
	spent := 150.0
	if err := store.set("claude:k", 200, expires, &spent); err != nil {
		t.Fatal(err)
	}
	probe := store.markExhausted("claude:k")
	if !probe.Equal(clock.Add(claudeCreditReprobeInterval)) {
		t.Fatalf("probe = %v, want one day later", probe)
	}
	clock = clock.Add(time.Hour)
	view := store.views(clock)["claude:k"]
	if view.Pace != claudeCreditPaceSpent || view.HeldUntil.IsZero() {
		t.Fatalf("view after spent 400 = %+v, want held", view)
	}
	clock = expires.Add(time.Minute)
	store.loadedAt = time.Time{}
	view = store.views(clock)["claude:k"]
	if view.SpentUSD != 0 || !view.HeldUntil.IsZero() || !view.ExpiresAt.Equal(expires.AddDate(0, 1, 0)) {
		t.Fatalf("after period end = %+v, want spend reset, hold cleared, expiry rolled a month", view)
	}

	// The state survives a restart: a new store reads the same file.
	reopened := newClaudeCostStore(store.path)
	reopened.now = func() time.Time { return clock }
	if got := reopened.views(clock)["claude:k"]; got.AmountUSD != 200 || !got.ExpiresAt.Equal(view.ExpiresAt) {
		t.Fatalf("reopened view = %+v, want persisted grant and rolled expiry", got)
	}
}

func TestClaudeCreditDefaultsAndMetering(t *testing.T) {
	store := newClaudeCostStore(filepath.Join(t.TempDir(), "state.json"))
	added := creditTestNow.Add(-time.Hour)
	store.now = func() time.Time { return creditTestNow }
	store.ensureKeys(map[string]time.Time{"claude:new": added})
	view := store.views(creditTestNow)["claude:new"]
	if view.AmountUSD != claudeCreditDefaultGrantUSD || !view.ExpiresAt.Equal(added.Add(claudeCreditDefaultPeriod)) || !view.Defaulted {
		t.Fatalf("default view = %+v, want $200 expiring 30 days after the add date", view)
	}
	cost := claudeAPIListCostUSD("claude-sonnet-4-6", tokenUsage{InputTokens: 1_000_000, OutputTokens: 100_000})
	if cost < 4.49 || cost > 4.51 {
		t.Fatalf("sonnet cost = %v, want $4.50 at list price", cost)
	}
	store.recordSpend("claude:new", cost)
	store.loadedAt = time.Time{}
	store.lastFlush = time.Time{}
	_ = store.mutate(nil)
	view = store.views(creditTestNow)["claude:new"]
	if view.SpentUSD < 4.49 || view.BurnPerHour <= 0 {
		t.Fatalf("metered view = %+v, want spend and burn recorded", view)
	}
}

const claudeTestModelBody = `{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[]}`

func claudeCreditServer(t *testing.T, plan []selectacct.Score, keys map[string]claudeCreditKeyState) (Server, *session.Store) {
	t.Helper()
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	costs := newClaudeCostStore(filepath.Join(t.TempDir(), "costs.json"))
	var all []accounts.Account
	scores := append([]selectacct.Score(nil), plan...)
	for i := range scores {
		scores[i].Provider = accounts.ProviderClaude
		scores[i].Fresh = true
		all = append(all, accounts.Account{ID: scores[i].AccountID, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-" + scores[i].AccountID})
	}
	_ = costs.mutate(func(file *claudeCostStateFile, _ time.Time) {
		for id, state := range keys {
			copied := state
			file.Keys[id] = &copied
		}
	})
	for id := range keys {
		all = append(all, accounts.Account{ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "key-" + id})
		scores = append(scores, selectacct.Score{AccountID: id, Provider: accounts.ProviderClaude, Headroom: 0.01, ShortHeadroom: 0.01,
			MissingModelSupport: selectacct.ModelSupportUnknown})
	}
	return Server{
		Accounts:     all,
		Sessions:     sessions,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(scores)),
		MaxBodyBytes: 1 << 16,
		claudeCosts:  costs,
	}, sessions
}

func pickClaudeForSession(t *testing.T, server Server, sessionID string) (accounts.Account, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(claudeTestModelBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	account, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", sessionID, req)
	return account, err
}

func TestClaudeNewSessionPlanFirstEarlyInPeriod(t *testing.T) {
	now := time.Now().UTC()
	server, _ := claudeCreditServer(t,
		[]selectacct.Score{{AccountID: "plan", Headroom: 0.9, ShortHeadroom: 0.9, WeeklyHeadroom: 0.9}},
		map[string]claudeCreditKeyState{"claude:early": creditState(now.Add(-24*time.Hour), 0)})
	got, err := pickClaudeForSession(t, server, "s-early")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "plan" {
		t.Fatalf("picked %q, want plan quota first early in the grant period", got.ID)
	}
}

func TestClaudeNewSessionPromotesBehindPaceKey(t *testing.T) {
	now := time.Now().UTC()
	server, _ := claudeCreditServer(t,
		[]selectacct.Score{{AccountID: "plan", Headroom: 0.9, ShortHeadroom: 0.9, WeeklyHeadroom: 0.9}},
		map[string]claudeCreditKeyState{"claude:behind": creditState(now.AddDate(0, 0, -20), 5)})
	got, err := pickClaudeForSession(t, server, "s-behind")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "claude:behind" {
		t.Fatalf("picked %q, want the behind-pace credit key ahead of plan quota", got.ID)
	}
	statuses := server.withClaudeCostTiers([]AccountUsageStatus{
		{AccountStatus: AccountStatus{ID: "claude:behind", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey}},
	})
	credit := statuses[0].Credit
	if credit == nil || !credit.Promoted || credit.RemainingUSD != 195 || credit.Pace != claudeCreditPaceBehind {
		t.Fatalf("status credit = %+v, want $195 left, behind, promoted", credit)
	}
}

func TestClaudeFinal48hKeyPromotedAndSoonestExpiryFirst(t *testing.T) {
	now := time.Now().UTC()
	final := creditState(now.Add(30*time.Hour).AddDate(0, -1, 0), 120)
	final.BurnUSDPerHour, final.BurnUpdatedAt = 50, now
	later := creditState(now.AddDate(0, 0, -25), 0)
	server, _ := claudeCreditServer(t,
		[]selectacct.Score{{AccountID: "plan", Headroom: 0.9, ShortHeadroom: 0.9, WeeklyHeadroom: 0.9}},
		map[string]claudeCreditKeyState{"claude:later": later, "claude:final": final})
	got, err := pickClaudeForSession(t, server, "s-final")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "claude:final" {
		t.Fatalf("picked %q, want the key expiring within 48h first", got.ID)
	}

	// Two ordinary (unpromoted) keys: the one expiring first is spent first.
	soon := creditState(now.Add(-time.Hour), 0)
	soon.ExpiresAt = now.Add(10 * 24 * time.Hour)
	far := creditState(now.Add(-time.Hour), 0)
	server, _ = claudeCreditServer(t,
		[]selectacct.Score{{AccountID: "plan", Headroom: 0, ShortHeadroom: 0, WeeklyHeadroom: 0, WeeklyHeadroomKnown: true}},
		map[string]claudeCreditKeyState{"claude:far": far, "claude:soon": soon})
	got, err = pickClaudeForSession(t, server, "s-soon")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "claude:soon" {
		t.Fatalf("picked %q, want the soonest-expiring grant", got.ID)
	}
}

// Moving a running session re-bills its whole prompt prefix, so a promoted
// key only takes new sessions.
func TestClaudePromotedKeyDoesNotMoveRunningSession(t *testing.T) {
	now := time.Now().UTC()
	server, sessions := claudeCreditServer(t,
		[]selectacct.Score{{AccountID: "plan", Headroom: 0.6, ShortHeadroom: 0.6, WeeklyHeadroom: 0.6}},
		map[string]claudeCreditKeyState{"claude:behind": creditState(now.AddDate(0, 0, -20), 0)})
	if _, err := sessions.Put("claude", "s-running", "plan", ""); err != nil {
		t.Fatal(err)
	}
	got, err := pickClaudeForSession(t, server, "s-running")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "plan" {
		t.Fatalf("running session moved to %q, want it kept on its plan account", got.ID)
	}
}

func TestClaudeSpentKeyHeldUntilPeriodEndThenExtraUsageThen503(t *testing.T) {
	now := time.Now().UTC()
	cooked := selectacct.Score{AccountID: "plan", Headroom: 0, ShortHeadroom: 0, WeeklyHeadroom: 0, WeeklyHeadroomKnown: true,
		ClaudeExtraUsageEnabled: true, ClaudeExtraUsageKnown: true, ClaudeExtraUsageRemaining: 500}
	server, _ := claudeCreditServer(t, []selectacct.Score{cooked},
		map[string]claudeCreditKeyState{"claude:only": creditState(now.AddDate(0, 0, -5), 0)})
	got, err := pickClaudeForSession(t, server, "s-1")
	if err != nil || got.ID != "claude:only" {
		t.Fatalf("picked %q, %v; want the free-credit key while plans are cooked", got.ID, err)
	}

	server.markClaudeCreditExhausted("claude:only")
	server.claudeCosts.loadedAt = time.Time{}
	view := server.claudeCosts.views(now)["claude:only"]
	if view.HeldUntil.Before(now.Add(23 * time.Hour)) {
		t.Fatalf("hold until %v, want the day-long re-probe hold, not one hour", view.HeldUntil)
	}
	got, err = pickClaudeForSession(t, server, "s-2")
	if err != nil || got.ID != "plan" {
		t.Fatalf("picked %q, %v; want extra usage once free credit is spent", got.ID, err)
	}

	cooked.ClaudeExtraUsageEnabled = false
	cooked.Provider = accounts.ProviderClaude
	server.SchedulerRef.Set(selectacct.NewScheduler([]selectacct.Score{cooked,
		{AccountID: "claude:only", Provider: accounts.ProviderClaude, Headroom: 0.01, ShortHeadroom: 0.01, MissingModelSupport: selectacct.ModelSupportUnknown}}))
	if _, err := pickClaudeForSession(t, server, "s-3"); err == nil || !strings.Contains(err.Error(), "no non-exhausted claude accounts") {
		t.Fatalf("error = %v, want the pool-exhausted error the handler turns into a 503", err)
	}
}

const anthropicRevokedBody = `{"type":"error","error":{"type":"authentication_error","message":"OAuth access token has been revoked."}}`

func TestClaudeRevokedAccountExcludedUntilCredentialChanges(t *testing.T) {
	server, _ := claudeCreditServer(t, []selectacct.Score{
		{AccountID: "revoked", Headroom: 1, ShortHeadroom: 1, WeeklyHeadroom: 1},
		{AccountID: "healthy", Headroom: 0.5, ShortHeadroom: 0.5, WeeklyHeadroom: 0.5},
	}, nil)
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if strings.Contains(req.Header.Get("Authorization"), "tok-revoked") {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(anthropicRevokedBody))}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"ok"}`))}
	}}
	transport := usageLimitRetryTransport{base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "s-rev", account: "revoked", method: http.MethodPost, path: "/v1/messages",
		maxAttempts: 3, budget: newAttemptBudget(2)}
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-revoked")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{}`)), nil }
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want failover to the healthy account", response.StatusCode)
	}

	server.claudeCosts.loadedAt = time.Time{}
	if !server.scheduler().Exhausted(accounts.ProviderClaude, "revoked") {
		t.Fatal("revoked account still routable")
	}
	got, err := pickClaudeForSession(t, server, "s-new")
	if err != nil || got.ID != "healthy" {
		t.Fatalf("picked %q, %v; want the healthy account over a revoked one with more headroom", got.ID, err)
	}
	statuses := server.withClaudeCostTiers([]AccountUsageStatus{
		{AccountStatus: AccountStatus{ID: "revoked", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}},
	})
	if statuses[0].RevokedSince.IsZero() {
		t.Fatalf("status = %+v, want revoked_since", statuses[0])
	}

	// Re-login replaces the credential; the account rejoins routing. (A
	// credential reload also drops the controller's short credential mark.)
	server.Accounts[0].Token = "tok-relogged"
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "revoked", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1, WeeklyHeadroom: 1},
	}))
	if server.scheduler().Exhausted(accounts.ProviderClaude, "revoked") {
		t.Fatal("re-logged account still excluded")
	}
}

// A corrupt state file must never be overwritten with defaults: that would
// silently drop operator-set grants and expiries.
func TestClaudeCostStoreRefusesToOverwriteCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newClaudeCostStore(path)
	if err := store.set("claude:k", 200, creditTestNow.AddDate(0, 0, 10), nil); err == nil {
		t.Fatal("set succeeded over a corrupt state file")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{not json" {
		t.Fatalf("state file = %q, %v; want it left untouched", data, err)
	}
}
