package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestClaudeTransientAdmissionCooldown(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, status, retryAfter string
		want                     time.Duration
	}{
		{name: "headerless", want: time.Second},
		{name: "allowed", status: "allowed", want: time.Second},
		{name: "warning with hint", status: "allowed_warning", retryAfter: "3", want: 3 * time.Second},
		{name: "http date", retryAfter: now.Add(4 * time.Second).Format(http.TimeFormat), want: now.Add(4 * time.Second).Truncate(time.Second).Sub(now)},
		{name: "malformed hint", retryAfter: "oops", want: time.Second},
		{name: "long provider hint", retryAfter: "90", want: 90 * time.Second},
		{name: "bounded pathological hint", retryAfter: "9999999", want: 8 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := testAdmissionControl(1, 2)
			key := admissionKeyFor(accounts.ProviderClaude, "a", "claude-opus")
			header := http.Header{}
			header.Set("Anthropic-Ratelimit-Unified-Status", tc.status)
			header.Set("Retry-After", tc.retryAfter)
			// A healthy quota window's reset is not a throttle deadline.
			header.Set("Anthropic-Ratelimit-Unified-Reset", fmt.Sprint(now.Add(7*24*time.Hour).Unix()))
			control.observe(key, http.StatusTooManyRequests, header, now)
			until := control.buckets[key].pausedTil
			if !until.Equal(now.Add(tc.want)) {
				t.Fatalf("cooldown = %v, want %v", until.Sub(now), tc.want)
			}
			if !control.paused(key, now) || control.paused(key, until) {
				t.Fatal("cooldown did not expire at its deadline")
			}
			if control.paused(admissionKeyFor(accounts.ProviderClaude, "b", "claude-opus"), now) ||
				control.paused(admissionKeyFor(accounts.ProviderClaude, "a", "claude-sonnet"), now) {
				t.Fatal("throttle escaped its account/model bucket")
			}
		})
	}
}

func transientThrottleServer(count int) Server {
	s := Server{UsageScoreTTL: time.Hour}
	var scores []selectacct.Score
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("account-%d", i)
		s.Accounts = append(s.Accounts, accounts.Account{ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: id})
		scores = append(scores, selectacct.Score{AccountID: id, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1, Fresh: true})
	}
	s.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(scores))
	s.RefreshAccountFn = func(_ context.Context, a accounts.Account) (accounts.Account, error) { return a, nil }
	return s
}

func transientThrottleResponse(status string, retryAfter string) *http.Response {
	header := http.Header{}
	header.Set("Anthropic-Ratelimit-Unified-Status", status)
	header.Set("Retry-After", retryAfter)
	return &http.Response{StatusCode: http.StatusTooManyRequests, Header: header, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error"}}`))}
}

func TestClaudeTransient429HonorsShortRetryAfterOnSameAccount(t *testing.T) {
	for _, status := range []string{"", "allowed", "allowed_warning"} {
		t.Run("status="+status, func(t *testing.T) {
			s := transientThrottleServer(3)
			calls := 0
			var waits []time.Duration
			transport := usageLimitRetryTransport{
				server: &s, provider: accounts.ProviderClaude, account: "account-0", maxAttempts: 3,
				method: http.MethodPost, path: "/v1/messages", poolModel: "claude-opus", sleep: recordSleep(&waits),
				base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.Header.Get("Authorization") != "Bearer account-0" {
						t.Fatal("short throttle discarded the sticky account")
					}
					if calls == 1 {
						return transientThrottleResponse(status, "3"), nil
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
				}),
			}
			resp, err := transport.RoundTrip(claudeOverloadRequest("account-0"))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK || calls != 2 || len(waits) != 1 || waits[0] != 3*time.Second {
				t.Fatalf("status=%d calls=%d waits=%v, want 200 after one 3s same-account retry", resp.StatusCode, calls, waits)
			}
		})
	}
}

// Every caller reaches the original account before its throttle is observed,
// reproducing a concurrent burst. The real physical-attempt wrapper records
// holds, releases bodies and enforces admission. Even with ample retry budget
// and eight accounts, each caller may send to at most one alternate.
func TestClaudeTransient429ConcurrentBurstIsBounded(t *testing.T) {
	s := transientThrottleServer(8)
	const callers = 16
	s.admission = testAdmissionControl(callers, callers*2)
	var entered atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := claudeOverloadRequest("account-0")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req = req.WithContext(ctx)
			seen := map[string]int{}
			base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				token := r.Header.Get("Authorization")
				seen[token]++
				if token == "Bearer account-0" {
					if entered.Add(1) == callers {
						close(start)
					}
					select {
					case <-start:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return transientThrottleResponse("allowed_warning", "10"), nil
			})
			a := &upstreamAttempt{server: &s, provider: accounts.ProviderClaude, account: s.Accounts[0],
				path: "/v1/messages", poolModel: "claude-opus", getBody: req.GetBody, budget: newAttemptBudget(20)}
			transport := upstreamLayers{usageLimit: &usageLimitRetryTransport{method: http.MethodPost, maxAttempts: 8}}.build(base, a)
			resp, err := transport.RoundTrip(req)
			if err != nil && !admissionRejected(err) {
				t.Errorf("round trip: %v", err)
			}
			if resp != nil {
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusTooManyRequests {
					t.Errorf("status=%d, want original transient 429", resp.StatusCode)
				}
			}
			if len(seen) > 2 || seen["Bearer account-0"] != 1 {
				t.Errorf("burst fanned out or retried before Retry-After: %v", seen)
			}
			for token, count := range seen {
				if count != 1 {
					t.Errorf("%s received %d attempts, want one", token, count)
				}
			}
		}()
	}
	wg.Wait()
	for _, a := range s.Accounts {
		if s.SchedulerRef.Get().ForModel("claude-opus").Exhausted(a.Provider, a.ID) {
			t.Fatalf("transient burst poisoned quota for %s", a.ID)
		}
	}
	for _, bucket := range s.admission.snapshot().Accounts {
		if bucket.Active != 0 || bucket.Queued != 0 {
			t.Fatalf("burst leaked admission leases: %+v", bucket)
		}
	}
}

func TestClaudeRetryCandidateSkipsTransientPauseWithoutQuotaExhaustion(t *testing.T) {
	s := transientThrottleServer(2)
	s.admission = testAdmissionControl(1, 2)
	key := admissionKeyFor(accounts.ProviderClaude, "account-1", "claude-opus")
	s.admission.observe(key, http.StatusTooManyRequests, http.Header{}, time.Now())
	if _, _, err := s.oauthRetryCandidateWithReservation(context.Background(), accounts.ProviderClaude, "claude", "s", "", "claude-opus", map[string]struct{}{"account-0": {}}, true, false); err == nil {
		t.Fatal("retry selected a throttled account")
	}
	if s.SchedulerRef.Get().ForModel("claude-opus").Exhausted(accounts.ProviderClaude, "account-1") {
		t.Fatal("transient pause was treated as quota exhaustion")
	}
	// A different model remains eligible on the same account.
	got, reservation, err := s.oauthRetryCandidateWithReservation(context.Background(), accounts.ProviderClaude, "claude", "s", "", "claude-sonnet", map[string]struct{}{"account-0": {}}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	reservation.Release()
	if got.ID != "account-1" {
		t.Fatalf("picked %s, want account-1", got.ID)
	}
}

func TestClaudeRejected429StillWalksQuotaPool(t *testing.T) {
	s := transientThrottleServer(4)
	seen := map[string]bool{}
	transport := usageLimitRetryTransport{
		server: &s, provider: accounts.ProviderClaude, account: "account-0", maxAttempts: 4,
		method: http.MethodPost, path: "/v1/messages", poolModel: "claude-opus", budget: newAttemptBudget(10),
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			token := req.Header.Get("Authorization")
			if seen[token] {
				t.Fatalf("quota retry revisited %s", token)
			}
			seen[token] = true
			if len(seen) < 4 {
				return transientThrottleResponse("rejected", "3600"), nil
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
		}),
	}
	resp, err := transport.RoundTrip(claudeOverloadRequest("account-0"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(seen) != 4 {
		t.Fatalf("status=%d accounts=%v, want successful walk of all four quota candidates", resp.StatusCode, seen)
	}
}

func TestClaudeTransient429AlternateBudgetSurvivesOuterReplay(t *testing.T) {
	s := transientThrottleServer(6)
	req := claudeOverloadRequest("account-0")
	a := &upstreamAttempt{server: &s, provider: accounts.ProviderClaude, account: s.Accounts[0],
		path: "/v1/messages", poolModel: "claude-opus", getBody: req.GetBody, budget: newAttemptBudget(20)}
	calls := 0
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 2 {
			return &http.Response{StatusCode: http.StatusRequestTimeout, Header: http.Header{}, Body: http.NoBody}, nil
		}
		return transientThrottleResponse("allowed", "10"), nil
	})
	transport := upstreamLayers{
		usageLimit:     &usageLimitRetryTransport{method: http.MethodPost, maxAttempts: 6},
		replayablePost: &replayablePostRetryTransport{method: http.MethodPost, maxAttempts: 6},
	}.build(base, a)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || calls != 3 {
		t.Fatalf("status=%d calls=%d, want 429 after initial, alternate 408, one outer replay", resp.StatusCode, calls)
	}
}

func TestClaudeTransientPauseAlsoBlocksPaidExtraUsageCandidate(t *testing.T) {
	s := transientThrottleServer(2)
	s.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "account-0", Provider: accounts.ProviderClaude, WeeklyHeadroomKnown: true},
		{AccountID: "account-1", Provider: accounts.ProviderClaude, WeeklyHeadroomKnown: true,
			ClaudeExtraUsageEnabled: true, ClaudeExtraUsageKnown: true, ClaudeExtraUsageRemaining: 9},
	}))
	s.admission = testAdmissionControl(1, 2)
	s.admission.observe(admissionKeyFor(accounts.ProviderClaude, "account-1", "claude-opus"), http.StatusTooManyRequests, http.Header{}, time.Now())
	if _, _, err := s.oauthRetryCandidateWithReservation(context.Background(), accounts.ProviderClaude, "claude", "s", "", "claude-opus", map[string]struct{}{"account-0": {}}, true, true); err == nil {
		t.Fatal("paid extra usage bypassed the transient throttle pause")
	}
}

func TestClaudeTransient429StillUsesConfiguredFallback(t *testing.T) {
	s := transientThrottleServer(6)
	calls, fallbacks := 0, 0
	transport := usageLimitRetryTransport{
		server: &s, provider: accounts.ProviderClaude, account: "account-0", maxAttempts: 6,
		method: http.MethodPost, path: "/v1/messages", poolModel: "claude-fable", budget: newAttemptBudget(20),
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return transientThrottleResponse("allowed", "10"), nil
		}),
		fableFallback: func() (*http.Response, bool) {
			fallbacks++
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, true
		},
	}
	resp, err := transport.RoundTrip(claudeOverloadRequest("account-0"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls != 2 || fallbacks != 1 {
		t.Fatalf("status=%d calls=%d fallbacks=%d, want fallback after one alternate", resp.StatusCode, calls, fallbacks)
	}
}
