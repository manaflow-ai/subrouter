package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func failingCodexPool(t *testing.T, calls *atomic.Int32) *url.URL {
	t.Helper()
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		codexEgressWriteOverloaded(w)
	}))
	t.Cleanup(pool.Close)
	parsed, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func failingAzureCodexTransport(calls *atomic.Int32) http.RoundTripper {
	return roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("azure unavailable"))}, nil
	})
}

func TestCodexPostFallbackRetryBudgetPreservesOverrides(t *testing.T) {
	config := &CodexOverloadFailoverConfig{StayMaxWait: 5 * time.Minute}
	base := codexCapacityRetryPolicy{persist: true, persistBudget: 2 * time.Minute}
	if budget, unbounded := (&CodexOverloadFailoverConfig{}).postFallbackRetryBudget(base, overloadRetryPolicy{}); budget != codexCapacityDefaultStayMaxWait || unbounded {
		t.Fatalf("default wait = %v, %t; want 4m, bounded", budget, unbounded)
	}
	if budget, unbounded := config.postFallbackRetryBudget(base, overloadRetryPolicy{}); budget != 5*time.Minute || unbounded {
		t.Fatalf("operator wait = %v, %t; want 5m, bounded", budget, unbounded)
	}
	requested := base
	requested.retry.maxWait, requested.retry.maxWaitSet = 3*time.Minute, true
	if budget, unbounded := config.postFallbackRetryBudget(requested, overloadRetryPolicy{maxWait: 3 * time.Minute}); budget != 3*time.Minute || unbounded {
		t.Fatalf("requested wait = %v, %t; want 3m, bounded", budget, unbounded)
	}
	unboundedConfig := &CodexOverloadFailoverConfig{StayUnbounded: true}
	if budget, unbounded := unboundedConfig.postFallbackRetryBudget(base, overloadRetryPolicy{}); budget != 0 || !unbounded {
		t.Fatalf("unbounded wait = %v, %t; want 0, unbounded", budget, unbounded)
	}
}

func TestCodexRetryableCapacityResponse(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		convert     bool
	}{
		{
			name:        "sse capacity",
			status:      http.StatusOK,
			contentType: "text/event-stream",
			body:        "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\"}}}\n\n",
			convert:     true,
		},
		{
			name:        "json capacity",
			status:      http.StatusBadGateway,
			contentType: "application/json",
			body:        `{"error":{"code":"server_is_overloaded"}}`,
			convert:     true,
		},
		{
			name:        "generic server error",
			status:      http.StatusBadGateway,
			contentType: "application/json",
			body:        `{"error":{"code":"server_error"}}`,
		},
		{
			name:        "capacity after output",
			status:      http.StatusOK,
			contentType: "text/event-stream",
			body:        "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\"}}}\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{
				StatusCode: test.status,
				Header:     http.Header{"Content-Type": []string{test.contentType}},
				Body:       io.NopCloser(strings.NewReader(test.body)),
			}
			response, converted := codexRetryableCapacityResponse(response)
			if converted != test.convert {
				t.Fatalf("converted = %t, want %t", converted, test.convert)
			}
			if !test.convert {
				if response.StatusCode != test.status {
					t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
				}
				return
			}
			if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "1" {
				t.Fatalf("response = %#v, want retryable 503", response)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != codexRetryableCapacityBody || strings.Contains(string(body), "server_is_overloaded") {
				t.Fatalf("body = %q, want the retryable capacity body", body)
			}
		})
	}
}

func TestCodexPersistRetryHonorsUnboundedPostFallbackWait(t *testing.T) {
	config := &CodexOverloadFailoverConfig{persistGap: func() time.Duration { return 0 }}
	transport := codexOverloadFailoverTransport{server: &Server{CodexOverloadFailover: config}}
	if _, planned := transport.planPersistRetry(context.Background(), "account-a", nil, time.Now().Add(-time.Second), true); !planned {
		t.Fatal("unbounded persistent retry should ignore the elapsed deadline")
	}
	if _, planned := transport.planPersistRetry(context.Background(), "account-a", nil, time.Now().Add(-time.Second), false); planned {
		t.Fatal("bounded persistent retry should honor the elapsed deadline")
	}
}

func TestCodexFallbackRetryUsesCurrentAccount(t *testing.T) {
	accountA := accounts.Account{ID: "account-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "token-a"}
	accountB := accounts.Account{ID: "account-b", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "token-b"}
	server := &Server{}
	req, err := http.NewRequest(http.MethodPost, "https://pool.invalid/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", accountA.AuthorizationHeader())
	attempt := &upstreamAttempt{
		server:  server,
		account: accountA,
		getBody: req.GetBody,
		capacityPolicy: &codexCapacityRetryPolicy{
			persist:       true,
			persistBudget: time.Minute,
		},
		addressed: accountB,
	}
	response := &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"server_is_overloaded"}}`)),
	}

	next, ok := server.codexFallbackRetryRequest(req, attempt, response)
	if !ok {
		t.Fatal("fallback retry not scheduled")
	}
	if got := next.Header.Get("Authorization"); got != accountB.AuthorizationHeader() {
		t.Fatalf("Authorization = %q, want current account %q", got, accountB.AuthorizationHeader())
	}
}

func TestCodexCapacityFinalResponseRequiresRetryableOptIn(t *testing.T) {
	pools := atomic.Int32{}
	poolURL := failingCodexPool(t, &pools)
	server := codexOverloadServer(t, poolURL, 1, true)
	server.CodexOverloadFailover.RetryBudget = 25 * time.Millisecond
	fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	request, err := http.NewRequest(http.MethodPost, proxy.URL+"/responses", strings.NewReader(`{"model":"gpt-6-astra","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "server_is_overloaded") || strings.Contains(string(body), "subrouter_capacity_retry") {
		t.Fatalf("status=%d body=%s, want the legacy capacity response without opt-in", response.StatusCode, body)
	}
}

func TestCodexCapacityKeepsRetryingAfterEgressAndAzureFail(t *testing.T) {
	var poolCalls, azureCalls, egressCalls atomic.Int32
	poolURL := failingCodexPool(t, &poolCalls)
	egress := codexEgressTestProxy(t, "fra", &egressCalls)
	server := codexEgressServer(t, poolURL, []*url.URL{egress}, 1)
	server.CodexOverloadFailover = &CodexOverloadFailoverConfig{
		CapacityRetryPersist: true,
		CapacityRetryBudget:  300 * time.Millisecond,
		StayMaxWait:          300 * time.Millisecond,
		RetryBudget:          40 * time.Millisecond,
	}
	fastCapacityGaps(server.CodexOverloadFailover, 10*time.Millisecond)
	server.CodexOverloadFailover.stayRetryLimit = 0
	server.AzureCodex = &AzureCodexConfig{
		Endpoints: []AzureCodexEndpoint{{BaseURL: mustParseURL(t, "https://azure.example/openai/v1"), APIKey: "test-key"}},
		Transport: failingAzureCodexTransport(&azureCalls),
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	started := time.Now()
	status, body := codexEgressPost(t, proxy.URL, "session-fallback-recovery")
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "subrouter_capacity_retry") || strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s, want a retryable exhausted capacity response", status, body)
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond {
		t.Fatalf("request returned after %v, want the persistent budget after both fallbacks failed", elapsed)
	}
	if egressCalls.Load() < 1 || azureCalls.Load() < 1 || poolCalls.Load() < 10 {
		t.Fatalf("fallback calls egress=%d azure=%d pool=%d, want fallback attempts followed by persistent pool retries", egressCalls.Load(), azureCalls.Load(), poolCalls.Load())
	}
}

func TestCodexCapacitySheddingStillHoldsAfterFallbackFailure(t *testing.T) {
	var poolCalls, egressCalls atomic.Int32
	poolURL := failingCodexPool(t, &poolCalls)
	egress := codexEgressTestProxy(t, "fra", &egressCalls)
	server := codexOverloadServer(t, poolURL, 2, true)
	server.CodexEgress = &CodexEgressConfig{Proxies: []*url.URL{egress}}
	server.CodexOverloadFailover.CapacityRetryPersist = true
	server.CodexOverloadFailover.CapacityRetryBudget = 4 * time.Second
	fastCapacityGaps(server.CodexOverloadFailover, 10*time.Millisecond)
	server.codexShedding = newCodexSheddingTracker()
	for range codexSheddingMinSamples {
		server.codexShedding.record("gpt-6-astra", "", true, time.Now())
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	started := time.Now()
	status, body := codexEgressPost(t, proxy.URL, "session-shedding-recovery")
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "subrouter_capacity_retry") || strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s, want a retryable exhausted capacity response", status, body)
	}
	if elapsed := time.Since(started); elapsed < 3800*time.Millisecond {
		t.Fatalf("request returned after %v, want it to continue past the 3s shedding budget", elapsed)
	}
	if egressCalls.Load() < 1 || poolCalls.Load() < 10 {
		t.Fatalf("fallback calls egress=%d pool=%d, want shedding to hand off then keep retrying", egressCalls.Load(), poolCalls.Load())
	}
}
