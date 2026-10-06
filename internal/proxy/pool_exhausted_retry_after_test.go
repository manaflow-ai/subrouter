package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// retryAfterToken is the cross-repo contract: cmux's agent auto-resume parses
// exactly this from the agent's error text to wait for capacity instead of
// blindly re-sending on a fixed backoff ladder.
var retryAfterToken = regexp.MustCompile(`retry after (\d+)s`)

type exhaustedPoolAccount struct {
	id      string
	windows []accounts.UsageWindow
	// mark, when non-zero, is a request-time exhaustion mark (an upstream
	// 429's reset) recorded on the account.
	mark time.Time
	// score, when set, replaces the window-derived score (no reset known).
	score *selectacct.Score
}

func usageWindow(name string, used float64, windowSeconds int64, resetAt time.Time) accounts.UsageWindow {
	return accounts.UsageWindow{
		Name:               name,
		UsedPercent:        used,
		LimitWindowSeconds: windowSeconds,
		ResetAt:            resetAt,
		ResetAfterSeconds:  int64(time.Until(resetAt).Seconds()),
	}
}

func fiveHour(used float64, resetAt time.Time) accounts.UsageWindow {
	return usageWindow("5h", used, 5*60*60, resetAt)
}

func sevenDay(used float64, resetAt time.Time) accounts.UsageWindow {
	return usageWindow("7d", used, 7*24*60*60, resetAt)
}

// serveExhaustedClaudePool sends one request through the real handler with
// every Claude account in pool exhausted and returns the 503.
func serveExhaustedClaudePool(t *testing.T, pool []exhaustedPoolAccount) *httptest.ResponseRecorder {
	t.Helper()
	var accountList []accounts.Account
	var scores []selectacct.Score
	for _, entry := range pool {
		accountList = append(accountList, accounts.Account{
			ID: entry.id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-" + entry.id,
		})
		if entry.score != nil {
			scores = append(scores, *entry.score)
			continue
		}
		scores = append(scores, scoreFromUsageWindows(accounts.ProviderClaude, entry.id, entry.windows))
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(scores))
	for _, entry := range pool {
		if !entry.mark.IsZero() {
			schedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, entry.id, "", entry.mark)
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("exhausted pool reached upstream with %q", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL := mustParseURL(t, upstream.URL)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Sessions:       store,
		ClaudeUpstream: upstreamURL,
		Accounts:       accountList,
		SchedulerRef:   schedulerRef,
		MaxBodyBytes:   1 << 20,
	}.Handler()
	req := httptest.NewRequest(http.MethodPost, "http://subrouter.local/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "session-exhausted-pool")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "no non-exhausted claude accounts available") {
		t.Fatalf("body lost the existing prefix: %q", response.Body.String())
	}
	return response
}

// requireRetryAfter checks the body token and the Retry-After header agree
// and land within a couple of seconds of want.
func requireRetryAfter(t *testing.T, response *httptest.ResponseRecorder, want time.Duration) int64 {
	t.Helper()
	body := response.Body.String()
	match := retryAfterToken.FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("body has no %q token: %q", retryAfterToken, body)
	}
	bodySeconds, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	header := response.Header().Get("Retry-After")
	if header != match[1] {
		t.Fatalf("Retry-After header = %q, body says %q; they must agree. body=%q", header, match[1], body)
	}
	if diff := bodySeconds - int64(want.Seconds()); diff < -2 || diff > 2 {
		t.Fatalf("retry after %ds, want about %ds; body=%q", bodySeconds, int64(want.Seconds()), body)
	}
	return bodySeconds
}

func TestExhaustedClaudePoolReportsEarliestSessionReset(t *testing.T) {
	now := time.Now()
	response := serveExhaustedClaudePool(t, []exhaustedPoolAccount{
		{id: "a@example.com", windows: []accounts.UsageWindow{fiveHour(100, now.Add(2*time.Hour)), sevenDay(40, now.Add(72*time.Hour))}},
		{id: "b@example.com", windows: []accounts.UsageWindow{fiveHour(100, now.Add(47*time.Minute)), sevenDay(50, now.Add(72*time.Hour))}},
		{id: "c@example.com", windows: []accounts.UsageWindow{fiveHour(100, now.Add(3*time.Hour)), sevenDay(10, now.Add(96*time.Hour))}},
	})
	requireRetryAfter(t, response, 47*time.Minute)
	if !strings.Contains(response.Body.String(), "; next account frees up in 47m (retry after ") {
		t.Fatalf("body = %q, want the human 47m hint", response.Body.String())
	}
}

func TestExhaustedClaudePoolWeeklyResetOutlastsSessionReset(t *testing.T) {
	now := time.Now()
	response := serveExhaustedClaudePool(t, []exhaustedPoolAccount{
		{id: "weekly@example.com", windows: []accounts.UsageWindow{fiveHour(100, now.Add(time.Hour)), sevenDay(100, now.Add(30*time.Hour))}},
	})
	requireRetryAfter(t, response, 30*time.Hour)
	if !strings.Contains(response.Body.String(), "next account frees up in 1d6h (retry after ") {
		t.Fatalf("body = %q, want the weekly 1d6h hint", response.Body.String())
	}
}

func TestExhaustedClaudePoolRequestTimeMarkOutlastsWindowReset(t *testing.T) {
	now := time.Now()
	response := serveExhaustedClaudePool(t, []exhaustedPoolAccount{
		{
			id:      "marked@example.com",
			windows: []accounts.UsageWindow{fiveHour(100, now.Add(10*time.Minute)), sevenDay(30, now.Add(72*time.Hour))},
			mark:    now.Add(90 * time.Minute),
		},
	})
	requireRetryAfter(t, response, 90*time.Minute)
	if !strings.Contains(response.Body.String(), "next account frees up in 1h30m (retry after ") {
		t.Fatalf("body = %q, want the 1h30m hint", response.Body.String())
	}
}

func TestExhaustedClaudePoolWithUnknownResetOmitsHint(t *testing.T) {
	response := serveExhaustedClaudePool(t, []exhaustedPoolAccount{
		{id: "unknown@example.com", score: &selectacct.Score{
			AccountID: "unknown@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0, WeeklyHeadroom: 1,
		}},
	})
	body := strings.TrimSpace(response.Body.String())
	if body != "no non-exhausted claude accounts available" {
		t.Fatalf("body = %q, want the bare message with no guessed hint", body)
	}
	if got := response.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want none when no reset is known", got)
	}
}

func TestPoolExhaustedErrorMessageContract(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		next time.Time
		want string
	}{
		{time.Time{}, "no non-exhausted claude accounts available"},
		{now.Add(-time.Minute), "no non-exhausted claude accounts available"},
		{now.Add(47 * time.Minute), "no non-exhausted claude accounts available; next account frees up in 47m (retry after 2820s)"},
		{now.Add(2*time.Hour + 5*time.Minute), "no non-exhausted claude accounts available; next account frees up in 2h5m (retry after 7500s)"},
		{now.Add(2*time.Hour + 4*time.Minute + 1*time.Second), "no non-exhausted claude accounts available; next account frees up in 2h5m (retry after 7441s)"},
		{now.Add(300 * time.Millisecond), "no non-exhausted claude accounts available; next account frees up in 1s (retry after 1s)"},
		{now.Add(45 * time.Second), "no non-exhausted claude accounts available; next account frees up in 45s (retry after 45s)"},
		{now.Add(9*24*time.Hour + 3*time.Hour), "no non-exhausted claude accounts available; next account frees up in 9d3h (retry after 788400s)"},
	}
	for _, tc := range cases {
		err := newPoolExhaustedError(accounts.ProviderClaude, tc.next, now)
		if got := err.Error(); got != tc.want {
			t.Errorf("next=%v: Error() = %q, want %q", tc.next.Sub(now), got, tc.want)
		}
		var typed *poolExhaustedError
		if !errors.As(err, &typed) {
			t.Fatalf("newPoolExhaustedError returned %T, want *poolExhaustedError", err)
		}
	}
}

// The client-side proxy in team mode never copies the hosted server's body
// into its error (it may relay provider text), so the broker's Retry-After is
// the only hint that crosses. It must reach both the header and the body token.
func TestBrokerRetryAfterReachesBodyToken(t *testing.T) {
	response := httptest.NewRecorder()
	writeAccountSelectionUnavailable(response, &broker.HTTPStatusError{
		StatusCode: http.StatusServiceUnavailable, RetryAfter: "1800", Message: "Service Unavailable",
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	requireRetryAfter(t, response, 30*time.Minute)
}
