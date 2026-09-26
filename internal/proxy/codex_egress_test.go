package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

const codexEgressHeader = "X-Test-Egress"

// codexEgressTestProxy is a plain HTTP forward proxy: Go's transport sends an
// absolute-URI request to it for http:// targets, which is enough to prove the
// request left through the egress. It stamps the region name on the way out.
func codexEgressTestProxy(t *testing.T, name string, calls *atomic.Int32) *url.URL {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !r.URL.IsAbs() {
			http.Error(w, "proxy expects an absolute URI", http.StatusBadRequest)
			return
		}
		forward, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		forward.Header = r.Header.Clone()
		forward.Header.Set(codexEgressHeader, name)
		response, err := http.DefaultTransport.RoundTrip(forward)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func codexEgressServer(t *testing.T, poolURL *url.URL, proxies []*url.URL, accountCount int) Server {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	pool := make([]accounts.Account, 0, accountCount)
	for index := range accountCount {
		pool = append(pool, accounts.Account{
			ID:       fmt.Sprintf("codex-account-%d", index),
			AuthMode: accounts.AuthModeOAuth,
			Token:    fmt.Sprintf("oauth-token-%d", index),
		})
	}
	server := Server{
		CodexUpstream: poolURL,
		Accounts:      pool,
		Sessions:      store,
		Scheduler:     selectacct.NewScheduler(nil),
		MaxBodyBytes:  1 << 20,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		// The fallbacks are under test, not the default capacity retry in
		// front of them; tests that want it replace this.
		CodexOverloadFailover: withoutCapacityRetry(),
	}
	if len(proxies) > 0 {
		server.CodexEgress = &CodexEgressConfig{Proxies: proxies}
	}
	return server
}

// withoutCapacityRetry leaves the capacity layer installed but with no time
// to retry, so a capacity failure goes straight on to the fallbacks.
func withoutCapacityRetry() *CodexOverloadFailoverConfig {
	return &CodexOverloadFailoverConfig{RetryBudget: time.Nanosecond}
}

func codexEgressWriteOverloaded(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
	_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"capacity\"}}}\n\n")
}

func codexEgressWriteCompleted(w http.ResponseWriter, region string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
	_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"served-from-"+region+"\"}]}]}}\n\n")
}

func codexEgressPost(t *testing.T, proxyURL, sessionID string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/responses",
		strings.NewReader(`{"model":"gpt-6-astra","session_id":"`+sessionID+`","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

// The pool opens the stream with server_is_overloaded from the router's own
// region. The same request replayed through the Frankfurt egress completes,
// and the session sticks to that egress so the next turn skips the pool.
func TestCodexEgressReplaysStreamOverloadAndPinsSession(t *testing.T) {
	var direct, viaEgress atomic.Int32
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if region := r.Header.Get(codexEgressHeader); region != "" {
			viaEgress.Add(1)
			codexEgressWriteCompleted(w, region)
			return
		}
		direct.Add(1)
		codexEgressWriteOverloaded(w)
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	var fraCalls atomic.Int32
	fra := codexEgressTestProxy(t, "fra", &fraCalls)

	proxy := httptest.NewServer(codexEgressServer(t, poolURL, []*url.URL{fra}, 1).Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-1")
	if status != http.StatusOK || !strings.Contains(body, "served-from-fra") {
		t.Fatalf("first turn status=%d body=%s, want the Frankfurt stream", status, body)
	}
	if strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("the failed pool stream leaked to the client: %s", body)
	}
	if direct.Load() != 1 || fraCalls.Load() != 1 || viaEgress.Load() != 1 {
		t.Fatalf("direct=%d fra=%d viaEgress=%d, want 1/1/1", direct.Load(), fraCalls.Load(), viaEgress.Load())
	}

	status, body = codexEgressPost(t, proxy.URL, "session-1")
	if status != http.StatusOK || !strings.Contains(body, "served-from-fra") {
		t.Fatalf("second turn status=%d body=%s", status, body)
	}
	if direct.Load() != 1 {
		t.Fatalf("pinned session touched the pool again: direct=%d", direct.Load())
	}
	if fraCalls.Load() != 2 {
		t.Fatalf("fra calls=%d, want 2", fraCalls.Load())
	}
}

// A 5xx status from the pool is the same regional failure as an in-stream one.
func TestCodexEgressReplaysPoolStatusFailure(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if region := r.Header.Get(codexEgressHeader); region != "" {
			codexEgressWriteCompleted(w, region)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"server_is_overloaded"}}`)
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	var calls atomic.Int32
	tyo := codexEgressTestProxy(t, "tyo", &calls)
	proxy := httptest.NewServer(codexEgressServer(t, poolURL, []*url.URL{tyo}, 1).Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-2")
	if status != http.StatusOK || !strings.Contains(body, "served-from-tyo") {
		t.Fatalf("status=%d body=%s, want the Tokyo stream", status, body)
	}
}

// When the first egress fails the same way, the next one is tried.
func TestCodexEgressTriesNextRegion(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get(codexEgressHeader) {
		case "tyo":
			codexEgressWriteCompleted(w, "tyo")
		default:
			codexEgressWriteOverloaded(w)
		}
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	var fraCalls, tyoCalls atomic.Int32
	fra := codexEgressTestProxy(t, "fra", &fraCalls)
	tyo := codexEgressTestProxy(t, "tyo", &tyoCalls)
	proxy := httptest.NewServer(codexEgressServer(t, poolURL, []*url.URL{fra, tyo}, 1).Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-3")
	if status != http.StatusOK || !strings.Contains(body, "served-from-tyo") {
		t.Fatalf("status=%d body=%s, want the Tokyo stream", status, body)
	}
	if fraCalls.Load() != 1 || tyoCalls.Load() != 1 {
		t.Fatalf("fra=%d tyo=%d, want each tried once", fraCalls.Load(), tyoCalls.Load())
	}
}

// Every region failing returns the pool's own failure, unchanged.
func TestCodexEgressAllRegionsFailReturnsPoolResponse(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		codexEgressWriteOverloaded(w)
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	var calls atomic.Int32
	fra := codexEgressTestProxy(t, "fra", &calls)
	proxy := httptest.NewServer(codexEgressServer(t, poolURL, []*url.URL{fra}, 1).Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-4")
	if status != http.StatusOK || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s, want the pool's own failure stream", status, body)
	}
	if calls.Load() != 1 {
		t.Fatalf("egress calls=%d, want 1", calls.Load())
	}
}

// Quota is account state, not region state: it must reach the layers that
// mark the account exhausted, never an egress.
func TestCodexEgressIgnoresQuotaFailures(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"quota"}}`)
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	var calls atomic.Int32
	fra := codexEgressTestProxy(t, "fra", &calls)
	proxy := httptest.NewServer(codexEgressServer(t, poolURL, []*url.URL{fra}, 1).Handler())
	defer proxy.Close()

	status, _ := codexEgressPost(t, proxy.URL, "session-5")
	if status != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want the pool's 429", status)
	}
	if calls.Load() != 0 {
		t.Fatalf("egress calls=%d, want 0 for a quota failure", calls.Load())
	}
}

// Unconfigured, the request path is unchanged: the overloaded stream reaches
// the client exactly as before.
func TestCodexEgressOffByDefault(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		codexEgressWriteOverloaded(w)
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	proxy := httptest.NewServer(codexEgressServer(t, poolURL, nil, 1).Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-6")
	if status != http.StatusOK || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s", status, body)
	}
}

func TestParseCodexEgressProxies(t *testing.T) {
	proxies, err := ParseCodexEgressProxies(" http://cmux-egress-fra:3128, http://cmux-egress-tyo:3128 ,")
	if err != nil || len(proxies) != 2 || proxies[1].Host != "cmux-egress-tyo:3128" {
		t.Fatalf("proxies=%v err=%v", proxies, err)
	}
	if _, err := ParseCodexEgressProxies("socks5://x:1080"); err == nil {
		t.Fatal("socks scheme must be rejected")
	}
	if got, err := ParseCodexEgressProxies(""); err != nil || len(got) != 0 {
		t.Fatalf("empty list: got=%v err=%v", got, err)
	}
}

// A pinned session whose egress answers a usage limit must not get that 429:
// quota follows the account, so the pin is dropped and the account failover
// beneath it moves the request to another account.
func TestCodexEgressPinDropsOnUsageLimitAndFailsOverAccount(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, token+"@"+r.Header.Get(codexEgressHeader))
		mu.Unlock()
		if token == "oauth-token-0" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"quota"}}`)
			return
		}
		codexEgressWriteCompleted(w, token)
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	var calls atomic.Int32
	fra := codexEgressTestProxy(t, "fra", &calls)
	server := codexEgressServer(t, poolURL, []*url.URL{fra}, 2)
	if _, err := server.Sessions.Put("codex", "session-p", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	server.codexEgressSessions = newAzureCodexSticky()
	server.codexEgressSessions.pin(azureCodexSessionKeyFor("codex", "session-p"), 0)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-p")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("status=%d body=%s, want completion from account 1 after the pinned egress hit a usage limit", status, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || !strings.HasPrefix(seen[0], "oauth-token-0@fra") || !strings.HasPrefix(seen[len(seen)-1], "oauth-token-1@") {
		t.Fatalf("pool saw %v, want the pinned egress attempt on account 0 then account 1", seen)
	}
	if _, pinned := server.codexEgressSessions.lookup(azureCodexSessionKeyFor("codex", "session-p")); pinned {
		t.Fatal("pin should be dropped after an account-level failure")
	}
}
