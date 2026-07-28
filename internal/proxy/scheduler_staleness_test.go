package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// Regression: the scheduler is a load-balancing hint, not a hard gate. Usage
// scores can go stale (the per-request re-score reads usage through a cache
// that falls back to stale "last good" data when the upstream usage endpoint
// rate-limits), which made healthy accounts look exhausted and produced bogus
// "no usable OAuth accounts" 503s while real quota existed. The router must
// route to the best account regardless of stale exhaustion scores and let the
// real upstream response decide.
func TestRouterDoesNotRefuseWhenScoresAreStaleExhausted(t *testing.T) {
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_1"}`))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Every OAuth Codex account is scored fully exhausted, simulating stale
	// cached scores. UsageScoreTTL=0 disables the pre-selection refresh so the
	// stale scores stick, exactly reproducing the stuck-scheduler state.
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "a@example.com", Provider: accounts.ProviderCodex, Headroom: 0, ShortHeadroom: 0},
		{AccountID: "b@example.com", Provider: accounts.ProviderCodex, Headroom: 0, ShortHeadroom: 0},
	}))

	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "a@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "a-token"},
			{ID: "b@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "b-token"},
		},
		Sessions:      store,
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: 0,
		MaxBodyBytes:  1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Session", "codex-session:stale")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)

	if response.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("router refused with 503 on stale-exhausted scores: %s", string(body))
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (routed despite stale exhaustion); body=%s", response.StatusCode, string(body))
	}
	if upstreamHits == 0 {
		t.Fatal("request was not forwarded upstream; router refused instead of routing")
	}
}
