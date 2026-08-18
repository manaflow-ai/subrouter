package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func azureCodexTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *url.URL) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL + "/openai/v1")
	if err != nil {
		t.Fatal(err)
	}
	return server, parsed
}

func TestAzureCodexBodyRewritesModelAndCacheKey(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-codex","stream":true,"input":[]}`)
	rewritten, err := azureCodexBody(body, "codex-deployment", "codex\x00session-1")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "codex-deployment" {
		t.Fatalf("model = %v, want the Azure deployment name", payload["model"])
	}
	key, _ := payload["prompt_cache_key"].(string)
	if !strings.HasPrefix(key, "sr-") {
		t.Fatalf("prompt_cache_key = %q, want a derived key so turns share a cached prefix", key)
	}
	if payload["stream"] != true {
		t.Fatalf("stream flag lost: %s", rewritten)
	}
	// The same session must derive the same key, or every turn is a cache miss.
	again, err := azureCodexBody(body, "codex-deployment", "codex\x00session-1")
	if err != nil {
		t.Fatal(err)
	}
	var second map[string]any
	if err := json.Unmarshal(again, &second); err != nil {
		t.Fatal(err)
	}
	if second["prompt_cache_key"] != payload["prompt_cache_key"] {
		t.Fatalf("cache key changed between turns: %v then %v", payload["prompt_cache_key"], second["prompt_cache_key"])
	}
}

func TestAzureCodexBodyKeepsClientCacheKey(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-codex","prompt_cache_key":"codex-conversation-42"}`)
	rewritten, err := azureCodexBody(body, "gpt-5.6-codex", "codex\x00session-1")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["prompt_cache_key"] != "codex-conversation-42" {
		t.Fatalf("client cache key overwritten: %v", payload["prompt_cache_key"])
	}
}

func TestAzureCodexPathAllowlist(t *testing.T) {
	for _, path := range []string{"/responses", "/v1/responses", "/backend-api/codex/responses"} {
		if _, ok := azureCodexPath(path); !ok {
			t.Errorf("%s should be serveable by Azure", path)
		}
	}
	// Compaction and the catalog have no Azure equivalent; sending them would
	// turn a pool failure into a 404 from a second provider.
	for _, path := range []string{"/responses/compact", "/v1/responses/compact", "/alpha/search", "/codex/models"} {
		if _, ok := azureCodexPath(path); ok {
			t.Errorf("%s must not be routed to Azure", path)
		}
	}
	if azureCodexRequest(http.MethodGet, "/responses") {
		t.Error("GET /responses took the Azure path")
	}
}

func TestAzureCodexStickyPinExpires(t *testing.T) {
	now := time.Now()
	sticky := newAzureCodexSticky()
	sticky.now = func() time.Time { return now }
	sticky.pin("codex\x00session-1", 1)

	endpoint, ok := sticky.lookup("codex\x00session-1")
	if !ok || endpoint != 1 {
		t.Fatalf("lookup = (%d, %v), want the pinned endpoint", endpoint, ok)
	}
	// Every request extends the pin, so an active conversation stays on one cache.
	now = now.Add(azureCodexStickyTTL - time.Minute)
	if _, ok := sticky.lookup("codex\x00session-1"); !ok {
		t.Fatal("pin expired while the session was still active")
	}
	now = now.Add(azureCodexStickyTTL + time.Minute)
	if _, ok := sticky.lookup("codex\x00session-1"); ok {
		t.Fatal("pin outlived its TTL, keeping an idle session off the pool")
	}
	sticky.pin("codex\x00session-2", 0)
	sticky.unpin("codex\x00session-2")
	if _, ok := sticky.lookup("codex\x00session-2"); ok {
		t.Fatal("unpin left the session pinned")
	}
	// An empty session key is never pinned: it would collide across sessions.
	sticky.pin("", 0)
	if _, ok := sticky.lookup(""); ok {
		t.Fatal("empty session key was pinned")
	}
}

func TestAzureCodexEndpointIndexIsStablePerSession(t *testing.T) {
	first := azureCodexEndpointIndex("codex\x00session-1", 3)
	if first != azureCodexEndpointIndex("codex\x00session-1", 3) {
		t.Fatal("endpoint choice is not stable for one session")
	}
	if first < 0 || first >= 3 {
		t.Fatalf("endpoint index %d out of range", first)
	}
	if azureCodexEndpointIndex("codex\x00session-1", 1) != 0 {
		t.Fatal("single endpoint must always be index 0")
	}
}

func TestAttemptBudgetIsSharedAndBounded(t *testing.T) {
	budget := newAttemptBudget(2)
	if !budget.consume() || !budget.consume() {
		t.Fatal("budget refused a retry it should allow")
	}
	if budget.consume() {
		t.Fatal("budget allowed a retry past its limit")
	}
	var nilBudget *attemptBudget
	if !nilBudget.consume() {
		t.Fatal("an absent budget must not bound retries")
	}
}

// The pool's own failure statuses decide whether Azure is worth paying for.
func TestAzureCodexPoolFailedStatuses(t *testing.T) {
	for _, status := range []int{401, 403, 408, 429, 500, 502, 503} {
		if !azureCodexPoolFailed(status) {
			t.Errorf("status %d should trigger the Azure fallback", status)
		}
	}
	for _, status := range []int{200, 201, 400, 404, 422} {
		if azureCodexPoolFailed(status) {
			t.Errorf("status %d must not trigger the Azure fallback", status)
		}
	}
}

func azureCodexFallbackServer(t *testing.T, azureURL *url.URL, poolURL *url.URL, accountCount int) Server {
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
	return Server{
		CodexUpstream: poolURL,
		Accounts:      pool,
		Sessions:      store,
		Scheduler:     selectacct.NewScheduler(nil),
		MaxBodyBytes:  1 << 20,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		AzureCodex: &AzureCodexConfig{
			Endpoints: []AzureCodexEndpoint{{
				Name:        "test-azure",
				BaseURL:     azureURL,
				APIKey:      "azure-key",
				Deployments: map[string]string{"gpt-5.6-codex": "codex-deployment"},
			}},
		},
	}
}

// End to end: the Codex pool answers 429 on every account, the retry budget
// runs out, and Azure serves the same request. The follow-up turn must go
// straight to Azure so the conversation keeps hitting one prompt cache.
func TestAzureCodexFallbackServesAndPinsSession(t *testing.T) {
	var poolCalls atomic.Int32
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		poolCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"quota"}}`)
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}

	var azureCalls atomic.Int32
	var azureBody atomic.Value
	var azureAPIKey atomic.Value
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		azureCalls.Add(1)
		if r.URL.Path != "/openai/v1/responses" {
			t.Errorf("azure path = %q, want /openai/v1/responses", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		azureBody.Store(string(body))
		azureAPIKey.Store(r.Header.Get("Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_azure"}`)
	})

	proxy := httptest.NewServer(azureCodexFallbackServer(t, azureURL, poolURL, 2).Handler())
	defer proxy.Close()

	request := func() *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, proxy.URL+"/responses",
			strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"session-1","input":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	first := request()
	body, _ := io.ReadAll(first.Body)
	_ = first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the Azure fallback; body: %s", first.StatusCode, body)
	}
	if !strings.Contains(string(body), "resp_azure") {
		t.Fatalf("body = %s, want the Azure response", body)
	}
	if azureCalls.Load() != 1 {
		t.Fatalf("azure calls = %d, want 1", azureCalls.Load())
	}
	if key, _ := azureAPIKey.Load().(string); key != "azure-key" {
		t.Fatalf("azure api key header = %q", key)
	}
	sent, _ := azureBody.Load().(string)
	if !strings.Contains(sent, `"model":"codex-deployment"`) {
		t.Fatalf("azure body = %s, want the deployment name", sent)
	}
	if poolCalls.Load() != 2 {
		t.Fatalf("pool attempts = %d, want both accounts tried before Azure", poolCalls.Load())
	}

	// Sticky: the next turn must not touch the pool at all.
	poolCalls.Store(0)
	second := request()
	_, _ = io.Copy(io.Discard, second.Body)
	_ = second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200", second.StatusCode)
	}
	if poolCalls.Load() != 0 {
		t.Fatalf("pool calls after pinning = %d, want 0; the session left its Azure cache", poolCalls.Load())
	}
	if azureCalls.Load() != 2 {
		t.Fatalf("azure calls = %d, want 2", azureCalls.Load())
	}
}

// A 400 is the client's own fault: paying Azure to repeat it would waste money
// and hide the error.
func TestAzureCodexFallbackIgnoresClientErrors(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad input"}}`)
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	var azureCalls atomic.Int32
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		azureCalls.Add(1)
		_, _ = io.WriteString(w, `{"id":"resp_azure"}`)
	})

	proxy := httptest.NewServer(azureCodexFallbackServer(t, azureURL, poolURL, 2).Handler())
	defer proxy.Close()

	response, err := http.Post(proxy.URL+"/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"session-2"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want the pool's 400 passed through", response.StatusCode)
	}
	if azureCalls.Load() != 0 {
		t.Fatalf("azure calls = %d, want 0", azureCalls.Load())
	}
}

// Azure failing must not swallow the pool's answer: the client sees the real
// pool error, and the session is not pinned to a broken endpoint.
func TestAzureCodexFallbackKeepsPoolResponseWhenAzureFails(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	})

	server := azureCodexFallbackServer(t, azureURL, poolURL, 2)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	response, err := http.Post(proxy.URL+"/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"session-3"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the pool's 429", response.StatusCode)
	}
}

// No usable account is a pool failure before the first upstream call, and it is
// the case that matters most: an expired credential store would otherwise 503
// every Codex request.
func TestAzureCodexFallbackWhenPoolHasNoAccount(t *testing.T) {
	var azureCalls atomic.Int32
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		azureCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_azure"}`)
	})
	poolURL, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	server := azureCodexFallbackServer(t, azureURL, poolURL, 2)
	server.Accounts = nil
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	response, err := http.Post(proxy.URL+"/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"session-4"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want Azure to answer; body: %s", response.StatusCode, body)
	}
	if azureCalls.Load() != 1 {
		t.Fatalf("azure calls = %d, want 1", azureCalls.Load())
	}
}

// The retry budget is what makes "five retries, then Azure" true: with a large
// pool the failover loop would otherwise walk every account before the fallback
// ran, and both retry layers would each get a full budget.
func TestAzureCodexFallbackCapsPoolRetries(t *testing.T) {
	var poolCalls atomic.Int32
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		poolCalls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_azure"}`)
	})

	proxy := httptest.NewServer(azureCodexFallbackServer(t, azureURL, poolURL, 12).Handler())
	defer proxy.Close()

	response, err := http.Post(proxy.URL+"/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"session-budget"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp_azure") {
		t.Fatalf("status = %d, body = %s, want the Azure fallback", response.StatusCode, body)
	}
	if got := poolCalls.Load(); got != azureCodexPoolRetryBudget+1 {
		t.Fatalf("pool attempts = %d, want %d (one attempt plus %d retries)",
			got, azureCodexPoolRetryBudget+1, azureCodexPoolRetryBudget)
	}
}

// Codex compresses large request bodies with zstd. Azure takes plain JSON, so a
// compressed body must be decoded before it is rewritten; otherwise the whole
// fallback silently stops working on exactly the long conversations that hit
// quota first.
func TestAzureCodexFallbackDecodesZstdBody(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	var azureBody atomic.Value
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		azureBody.Store(string(body))
		if encoding := r.Header.Get("Content-Encoding"); encoding != "" {
			t.Errorf("azure request carried Content-Encoding %q", encoding)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_azure"}`)
	})

	proxy := httptest.NewServer(azureCodexFallbackServer(t, azureURL, poolURL, 1).Handler())
	defer proxy.Close()

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll([]byte(`{"model":"gpt-5.6-codex","session_id":"session-zstd","input":[]}`), nil)
	_ = encoder.Close()

	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/responses", bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp_azure") {
		t.Fatalf("status = %d, body = %s, want the Azure fallback", response.StatusCode, body)
	}
	sent, _ := azureBody.Load().(string)
	if !strings.Contains(sent, `"model":"codex-deployment"`) {
		t.Fatalf("azure body = %q, want decoded JSON with the deployment name", sent)
	}
}
