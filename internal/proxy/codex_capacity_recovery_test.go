package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	if status != http.StatusOK || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s, want the exhausted capacity response", status, body)
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
	if status != http.StatusOK || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s, want the exhausted capacity response", status, body)
	}
	if elapsed := time.Since(started); elapsed < 3800*time.Millisecond {
		t.Fatalf("request returned after %v, want it to continue past the 3s shedding budget", elapsed)
	}
	if egressCalls.Load() < 1 || poolCalls.Load() < 10 {
		t.Fatalf("fallback calls egress=%d pool=%d, want shedding to hand off then keep retrying", egressCalls.Load(), poolCalls.Load())
	}
}
