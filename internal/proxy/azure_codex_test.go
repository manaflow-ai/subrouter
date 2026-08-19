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

// Every Codex turn against the ChatGPT backend carries session_id, which the
// Responses API rejects outright. Without stripping it the fallback answers a
// pool outage with a 400 from a second provider.
func TestAzureCodexBodyDropsChatGPTOnlyFields(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-codex","session_id":"abc","conversation_id":"def","input":[],"store":false}`)
	rewritten, err := azureCodexBody(body, "codex-deployment", "codex\x00session-1")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"session_id", "conversation_id", "thread_id"} {
		if _, present := payload[field]; present {
			t.Fatalf("%s survived: %s", field, rewritten)
		}
	}
	if payload["store"] != false {
		t.Fatalf("store dropped: %s", rewritten)
	}
}

func TestAzureCodexRejectedFieldAndDrop(t *testing.T) {
	unknown := []byte(`{"error":{"message":"Unknown parameter: 'session_id'.","code":"unknown_parameter","param":"session_id"}}`)
	if got := azureCodexRejectedField(unknown); got != "session_id" {
		t.Fatalf("rejected field = %q", got)
	}
	nested := []byte(`{"error":{"message":"'all_turns' is not supported","code":"unsupported_value","param":"reasoning.context"}}`)
	if got := azureCodexRejectedField(nested); got != "reasoning.context" {
		t.Fatalf("nested rejected field = %q", got)
	}
	// Conversation content is never dropped: removing an input element, or the
	// keys that carry the turn itself, would change what the model was asked.
	for _, param := range []string{"input[3]", "input[3].content", "input[3].role", "input[3].arguments"} {
		body := []byte(`{"error":{"code":"unsupported_value","param":"` + param + `"}}`)
		if got := azureCodexRejectedField(body); got != "" {
			t.Fatalf("content path %q was accepted for dropping", got)
		}
	}
	other := []byte(`{"error":{"code":"context_length_exceeded","param":"input"}}`)
	if got := azureCodexRejectedField(other); got != "" {
		t.Fatalf("unrelated 400 named a field to drop: %q", got)
	}

	payload := map[string]json.RawMessage{
		"reasoning": json.RawMessage(`{"effort":"high","context":"all_turns"}`),
		"model":     json.RawMessage(`"gpt-5.6-codex"`),
	}
	if !azureCodexDropField(payload, "reasoning.context") {
		t.Fatal("nested field was not dropped")
	}
	if string(payload["reasoning"]) != `{"effort":"high"}` {
		t.Fatalf("reasoning = %s, want only effort left", payload["reasoning"])
	}
	if azureCodexDropField(payload, "reasoning.missing") || azureCodexDropField(payload, "model.name") {
		t.Fatal("dropping a missing or non-object path reported success")
	}
}

func TestAzureCodexEndpointDefaultDeployment(t *testing.T) {
	endpoint := AzureCodexEndpoint{Deployments: map[string]string{
		"gpt-5.6-codex":                "codex-max",
		AzureCodexDefaultDeploymentKey: "codex-default",
	}}
	if got := endpoint.deployment("gpt-5.6-codex"); got != "codex-max" {
		t.Fatalf("exact mapping = %q", got)
	}
	// Azure trails the ChatGPT model list, so an unlisted model must still land
	// on a real deployment instead of 404ing the request that needed rescue.
	if got := endpoint.deployment("gpt-6-codex-unreleased"); got != "codex-default" {
		t.Fatalf("default mapping = %q", got)
	}
	bare := AzureCodexEndpoint{}
	if got := bare.deployment("gpt-5.6-codex"); got != "gpt-5.6-codex" {
		t.Fatalf("identity mapping = %q", got)
	}
}

// The daemon retries once without the field Azure named, which is how a live
// Codex body survives an Azure model version that is a release behind.
func TestAzureCodexRetriesWithoutRejectedField(t *testing.T) {
	var bodies []string
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		if strings.Contains(string(body), "all_turns") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"unsupported_value","param":"reasoning.context","message":"'all_turns' is not supported"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_azure"}`)
	})
	poolURL, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	server := azureCodexFallbackServer(t, azureURL, poolURL, 0)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	response, err := http.Post(proxy.URL+"/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"s","reasoning":{"effort":"high","context":"all_turns"},"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp_azure") {
		t.Fatalf("status = %d, body = %s, want the retry to succeed", response.StatusCode, body)
	}
	if len(bodies) != 2 {
		t.Fatalf("azure attempts = %d, want 2", len(bodies))
	}
	if strings.Contains(bodies[1], "all_turns") {
		t.Fatalf("retry kept the rejected field: %s", bodies[1])
	}
	if !strings.Contains(bodies[1], `"effort":"high"`) {
		t.Fatalf("retry dropped a sibling field: %s", bodies[1])
	}
}

// Forcing is how the route is proven without waiting for an outage: the pool is
// skipped entirely, and a broken Azure surfaces as an error instead of a
// ChatGPT answer.
func TestAzureCodexForceHeaderSkipsPool(t *testing.T) {
	var poolCalls atomic.Int32
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		poolCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_pool"}`)
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	var azureCalls atomic.Int32
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		azureCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_azure"}`)
	})

	proxy := httptest.NewServer(azureCodexFallbackServer(t, azureURL, poolURL, 2).Handler())
	defer proxy.Close()

	forced := func(path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, proxy.URL+path,
			strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"forced-session"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Subrouter-Azure", "force")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	response := forced("/responses")
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !strings.Contains(string(body), "resp_azure") {
		t.Fatalf("body = %s, want the Azure answer", body)
	}
	if poolCalls.Load() != 0 {
		t.Fatalf("pool calls = %d, want 0 on a forced request", poolCalls.Load())
	}
	// A forced request must not pin: forcing is per request, and the pin exists
	// to preserve a cache created by an involuntary fallback.
	plain, err := http.Post(proxy.URL+"/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"forced-session"}`))
	if err != nil {
		t.Fatal(err)
	}
	plainBody, _ := io.ReadAll(plain.Body)
	_ = plain.Body.Close()
	if !strings.Contains(string(plainBody), "resp_pool") {
		t.Fatalf("unforced turn = %s, want the pool", plainBody)
	}
}

// Codex sends the force header on every request of a forced session, including
// the model catalog, which Azure cannot serve. Those must keep taking the pool
// path instead of failing the session.
func TestAzureCodexForceIgnoresNonResponsesPaths(t *testing.T) {
	var poolPaths []string
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		poolPaths = append(poolPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the model catalog reached Azure")
	})
	proxy := httptest.NewServer(azureCodexFallbackServer(t, azureURL, poolURL, 1).Handler())
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodGet, proxy.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Azure", "force")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the pool to answer the catalog", response.StatusCode)
	}
	if len(poolPaths) == 0 {
		t.Fatal("the catalog request never reached the pool")
	}
}

// A forced request with no Azure configured must fail loudly, naming the fix.
func TestAzureCodexForceWithoutConfigurationExplainsItself(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"resp_pool"}`)
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	server := azureCodexFallbackServer(t, azureURL, poolURL, 1)
	server.AzureCodex = nil
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/responses",
		strings.NewReader(`{"model":"gpt-5.6-codex"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Azure", "force")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
	if !strings.Contains(string(body), "SUBROUTER_AZURE_CODEX_ENDPOINT") {
		t.Fatalf("error body = %q, want the missing configuration named", body)
	}
}

// Rediscovering a rejection every turn means re-uploading the whole
// conversation to learn what the last request already proved. The second
// request must go out clean on the first attempt.
func TestAzureCodexRemembersRejectedFields(t *testing.T) {
	var bodies []string
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		if strings.Contains(string(body), "all_turns") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"unsupported_value","param":"reasoning.context"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_azure"}`)
	})
	poolURL, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(azureCodexFallbackServer(t, azureURL, poolURL, 0).Handler())
	defer proxy.Close()

	send := func(session string) {
		t.Helper()
		response, err := http.Post(proxy.URL+"/responses", "application/json",
			strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"`+session+`","reasoning":{"effort":"high","context":"all_turns"},"input":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp_azure") {
			t.Fatalf("status = %d, body = %s", response.StatusCode, body)
		}
	}

	send("memory-1")
	if len(bodies) != 2 {
		t.Fatalf("first request attempts = %d, want the discovery retry", len(bodies))
	}
	send("memory-2")
	if len(bodies) != 3 {
		t.Fatalf("attempts after learning = %d, want one clean attempt", len(bodies))
	}
	if strings.Contains(bodies[2], "all_turns") {
		t.Fatalf("the remembered field came back: %s", bodies[2])
	}
	if !strings.Contains(bodies[2], `"effort":"high"`) {
		t.Fatalf("a sibling field was dropped with it: %s", bodies[2])
	}
}

func TestAzureCodexFieldMemoryExpires(t *testing.T) {
	now := time.Now()
	memory := newAzureCodexFieldMemory()
	memory.now = func() time.Time { return now }
	key := azureCodexFieldMemoryKey("eastus2", "gpt-5.3-codex")
	memory.remember(key, "reasoning.context")

	if got := memory.known(key); len(got) != 1 || got[0] != "reasoning.context" {
		t.Fatalf("known = %v", got)
	}
	if got := memory.known(azureCodexFieldMemoryKey("eastus2", "other-deployment")); len(got) != 0 {
		t.Fatalf("a rejection leaked to another deployment: %v", got)
	}
	// An Azure model upgrade that accepts the field again must take effect
	// without a restart.
	now = now.Add(azureCodexFieldMemoryTTL + time.Minute)
	if got := memory.known(key); len(got) != 0 {
		t.Fatalf("known after expiry = %v", got)
	}
	var absent *azureCodexFieldMemory
	absent.remember(key, "x")
	if got := absent.known(key); got != nil {
		t.Fatalf("a nil memory returned %v", got)
	}
}

// A conversation that starts on the ChatGPT pool and then falls back carries
// reasoning blobs Azure cannot decrypt. That is the exact case the fallback
// exists for, and it arrives as a 400 with a null param, so the field-dropping
// retry cannot rescue it.
func TestAzureCodexRetriesWithoutForeignEncryptedReasoning(t *testing.T) {
	var bodies []string
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		// Only a sealed item is rejected. Asking for encrypted content back
		// (the include list) is fine, and the request must keep doing so.
		if strings.Contains(string(body), `"encrypted_content":"`) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"The encrypted content for item rs_1 could not be verified.","type":"invalid_request_error","param":null,"code":"invalid_encrypted_content"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_azure"}`)
	})
	poolURL, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(azureCodexFallbackServer(t, azureURL, poolURL, 0).Handler())
	defer proxy.Close()

	const conversation = `{"model":"gpt-5.6-codex","session_id":"sealed-1","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]},` +
		`{"type":"reasoning","id":"rs_1","encrypted_content":"gAAAA-from-chatgpt","summary":[]},` +
		`{"type":"reasoning","id":"rs_2","encrypted_content":"gAAAA-also","summary":[{"type":"summary_text","text":"looked at the file"}]}` +
		`],"include":["reasoning.encrypted_content"]}`

	send := func() {
		t.Helper()
		response, err := http.Post(proxy.URL+"/responses", "application/json", strings.NewReader(conversation))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp_azure") {
			t.Fatalf("status = %d, body = %s, want the retry to succeed", response.StatusCode, body)
		}
	}

	send()
	if len(bodies) != 2 {
		t.Fatalf("attempts = %d, want one rejection then one clean retry", len(bodies))
	}
	retry := bodies[1]
	if strings.Contains(retry, `"encrypted_content":"`) {
		t.Fatalf("the sealed blobs survived the retry: %s", retry)
	}
	// The request should still ask for encrypted reasoning back, so the turns
	// Azure itself produces keep their continuity.
	if !strings.Contains(retry, `"include":["reasoning.encrypted_content"]`) {
		t.Fatalf("the include list was stripped with the blobs: %s", retry)
	}
	// The user's own turn must never be dropped with the reasoning.
	if !strings.Contains(retry, "fix the bug") {
		t.Fatalf("the user message was dropped: %s", retry)
	}
	// A reasoning item with a visible summary keeps that summary; one that held
	// nothing but ciphertext goes away entirely.
	if !strings.Contains(retry, "looked at the file") {
		t.Fatalf("a readable reasoning summary was dropped: %s", retry)
	}
	if strings.Contains(retry, `"rs_1"`) {
		t.Fatalf("an empty reasoning shell was kept: %s", retry)
	}

	// Every later turn of the same conversation carries the same unreadable
	// blobs, so the next request must go out already stripped.
	send()
	if len(bodies) != 3 {
		t.Fatalf("attempts after learning = %d, want one clean attempt", len(bodies))
	}
	if strings.Contains(bodies[2], `"encrypted_content":"`) {
		t.Fatalf("the second turn re-sent sealed reasoning: %s", bodies[2])
	}
}

func TestAzureCodexEncryptedContentDetection(t *testing.T) {
	if !azureCodexEncryptedContentRejected([]byte(`{"error":{"code":"invalid_encrypted_content","param":null}}`)) {
		t.Fatal("the documented code was not recognized")
	}
	if !azureCodexEncryptedContentRejected([]byte(`{"error":{"message":"Encrypted content could not be decrypted or parsed."}}`)) {
		t.Fatal("the message form was not recognized")
	}
	if azureCodexEncryptedContentRejected([]byte(`{"error":{"code":"unknown_parameter","param":"session_id"}}`)) {
		t.Fatal("an unrelated rejection was treated as an encryption failure")
	}
}

func TestAzureCodexStripEncryptedReasoningLeavesOtherBodiesAlone(t *testing.T) {
	payload := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"message","role":"user","content":[]}]`),
	}
	if azureCodexStripEncryptedReasoning(payload) {
		t.Fatal("a body with no sealed reasoning reported a change, which would retry an identical request")
	}
	noInput := map[string]json.RawMessage{"model": json.RawMessage(`"gpt-5.6-codex"`)}
	if azureCodexStripEncryptedReasoning(noInput) {
		t.Fatal("a body with no input reported a change")
	}
}

// Codex sends item-level settings Azure does not know, and it names them by
// index: "Unknown parameter: 'input[39].namespace'". Dropping that key is safe;
// dropping the item itself would change what the model was asked.
func TestAzureCodexDropsAnUnknownKeyInsideAnInputItem(t *testing.T) {
	if got := azureCodexRejectedField([]byte(`{"error":{"code":"unknown_parameter","param":"input[1].namespace"}}`)); got != "input[1].namespace" {
		t.Fatalf("rejected field = %q, want the indexed key", got)
	}
	if got := azureCodexRejectedField([]byte(`{"error":{"code":"unknown_parameter","param":"input[1]"}}`)); got != "" {
		t.Fatalf("an entire input item was offered for dropping: %q", got)
	}

	payload := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"message","role":"user"},{"type":"custom_tool_call","namespace":"mcp","name":"search"}]`),
	}
	if !azureCodexDropField(payload, "input[1].namespace") {
		t.Fatal("the indexed key was not dropped")
	}
	var items []map[string]any
	if err := json.Unmarshal(payload["input"], &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want the conversation intact", len(items))
	}
	if _, present := items[1]["namespace"]; present {
		t.Fatalf("namespace survived: %v", items[1])
	}
	if items[1]["name"] != "search" {
		t.Fatalf("a sibling key was dropped: %v", items[1])
	}
	if items[0]["role"] != "user" {
		t.Fatalf("another item was disturbed: %v", items[0])
	}

	// Out of range, non-object elements, and missing keys must all report
	// failure so the caller stops retrying instead of resending the same body.
	if azureCodexDropField(payload, "input[9].namespace") ||
		azureCodexDropField(payload, "input[0].missing") {
		t.Fatal("a path that changes nothing reported success")
	}
	scalars := map[string]json.RawMessage{"input": json.RawMessage(`["plain"]`)}
	if azureCodexDropField(scalars, "input[0].namespace") {
		t.Fatal("a non-object element reported success")
	}
}

func TestAzureCodexModelAllowed(t *testing.T) {
	unrestricted := &AzureCodexConfig{}
	if !unrestricted.modelAllowed("gpt-5.3-codex") {
		t.Fatal("empty allow list must serve every model")
	}
	var nilConfig *AzureCodexConfig
	if nilConfig.modelAllowed("gpt-5.6-sol") {
		t.Fatal("nil config must not serve anything")
	}
	gated := &AzureCodexConfig{Models: []string{"gpt-5.6*", "gpt-5.3-codex"}}
	for model, want := range map[string]bool{
		"gpt-5.6-sol":   true,
		"gpt-5.6-luna":  true,
		"GPT-5.6-TERRA": true,
		"gpt-5.3-codex": true,
		"gpt-5.2-codex": false,
		"gpt-5":         false,
		"":              false,
	} {
		if got := gated.modelAllowed(model); got != want {
			t.Fatalf("modelAllowed(%q) = %v, want %v", model, got, want)
		}
	}
}

// A model outside the allow list stays on the pool: paying a metered provider
// to answer with a different model than the one requested is worse than
// surfacing the pool's own failure.
func TestAzureCodexFallbackSkipsDisallowedModel(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	var azureCalls atomic.Int32
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		azureCalls.Add(1)
		_, _ = io.WriteString(w, `{"id":"resp_azure"}`)
	})
	server := azureCodexFallbackServer(t, azureURL, poolURL, 1)
	server.AzureCodex.Models = []string{"gpt-5.6*"}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	response, err := http.Post(proxy.URL+"/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.2-codex","session_id":"session-gate","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the pool's own 429 back", response.StatusCode)
	}
	if azureCalls.Load() != 0 {
		t.Fatalf("azure calls = %d, want 0 for a disallowed model", azureCalls.Load())
	}

	allowed, err := http.Post(proxy.URL+"/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-sol","session_id":"session-gate-2","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer allowed.Body.Close()
	if allowed.StatusCode != http.StatusOK {
		t.Fatalf("allowed model status = %d, want 200 from Azure", allowed.StatusCode)
	}
	if azureCalls.Load() != 1 {
		t.Fatalf("azure calls = %d, want 1 for the allowed model", azureCalls.Load())
	}
}

// Codex treats an in-stream server_is_overloaded as terminal ("Selected model
// is at capacity. Please try a different model."), so a 200 SSE stream that
// opens with one is a pool failure the status code never shows.
func TestAzureCodexStreamOverloadedDivertsToAzure(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"capacity\"}}}\n\n")
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	var azureCalls atomic.Int32
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		azureCalls.Add(1)
		_, _ = io.WriteString(w, `{"id":"resp_azure"}`)
	})
	proxy := httptest.NewServer(azureCodexFallbackServer(t, azureURL, poolURL, 1).Handler())
	defer proxy.Close()

	response, err := http.Post(proxy.URL+"/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"session-overload","stream":true,"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from Azure; body: %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "resp_azure") {
		t.Fatalf("body = %s, want the Azure response, not the overloaded pool stream", body)
	}
	if azureCalls.Load() != 1 {
		t.Fatalf("azure calls = %d, want 1", azureCalls.Load())
	}
}

// A healthy stream must reach the client byte-for-byte: the sniff may peek,
// never consume.
func TestAzureCodexStreamPassthroughKeepsBytes(t *testing.T) {
	stream := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	var azureCalls atomic.Int32
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		azureCalls.Add(1)
	})
	proxy := httptest.NewServer(azureCodexFallbackServer(t, azureURL, poolURL, 1).Handler())
	defer proxy.Close()

	response, err := http.Post(proxy.URL+"/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"session-healthy","stream":true,"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != stream {
		t.Fatalf("stream mutated by the sniff:\ngot:  %q\nwant: %q", body, stream)
	}
	if azureCalls.Load() != 0 {
		t.Fatalf("azure calls = %d, want 0 for a healthy stream", azureCalls.Load())
	}
}

func TestAzureCodexStreamFailureDetection(t *testing.T) {
	cases := map[string]struct {
		contentType string
		body        string
		want        codexFailureClass
	}{
		"failed first event": {
			contentType: "text/event-stream",
			body:        "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\"}}}\n\n",
			want:        codexFailureServer,
		},
		"slow_down after created": {
			contentType: "text/event-stream",
			body: "data: {\"type\":\"response.created\"}\n\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"slow_down\"}}}\n\n",
			want: codexFailureServer,
		},
		"unknown future code after in_progress": {
			contentType: "text/event-stream",
			body: "data: {\"type\":\"response.created\"}\n\n" +
				"data: {\"type\":\"response.in_progress\"}\n\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"quantum_flux_disruption\",\"message\":\"try later\"}}}\n\n",
			want: codexFailureServer,
		},
		"quota event": {
			contentType: "text/event-stream",
			body:        "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"usage_limit_reached\"}}}\n\n",
			want:        codexFailureQuota,
		},
		"context window failure passes through": {
			contentType: "text/event-stream",
			body:        "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"context_length_exceeded\"}}}\n\n",
			want:        codexFailureNone,
		},
		"healthy stream": {
			contentType: "text/event-stream",
			body:        "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.output_item.added\"}\n\n",
			want:        codexFailureNone,
		},
		"not sse": {
			contentType: "application/json",
			body:        `{"error":{"code":"server_is_overloaded"}}`,
			want:        codexFailureNone,
		},
		"truncated failed event without trailing blank line": {
			contentType: "text/event-stream",
			body:        "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\"}}}\n",
			want:        codexFailureServer,
		},
	}
	for name, test := range cases {
		response := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{test.contentType}},
			Body:       io.NopCloser(strings.NewReader(test.body)),
		}
		class, replaced := azureCodexStreamFailure(response)
		if class != test.want {
			t.Fatalf("%s: class = %v, want %v", name, class, test.want)
		}
		rest, err := io.ReadAll(replaced.Body)
		if err != nil {
			t.Fatalf("%s: read restitched body: %v", name, err)
		}
		if string(rest) != test.body {
			t.Fatalf("%s: restitched body = %q, want the original %q", name, rest, test.body)
		}
	}
}

// Codex compresses large bodies, and a session long enough to exhaust a quota
// is far past a megabyte. Bounding the decode by the session-id peek limit
// refused the fallback for exactly those requests.
func TestAzureCodexFallbackServesABodyLargerThanThePeekLimit(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	var received atomic.Int64
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received.Store(int64(len(body)))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_azure"}`)
	})

	server := azureCodexFallbackServer(t, azureURL, poolURL, 1)
	// The default peek limit, which this request is far larger than.
	server.MaxBodyBytes = 1 << 20
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	filler := strings.Repeat("a conversation turn that has to survive the fallback. ", 60000)
	payload := `{"model":"gpt-5.6-codex","session_id":"large","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + filler + `"}]}]}`
	if len(payload) <= (1 << 20) {
		t.Fatalf("payload is %d bytes, want more than the peek limit", len(payload))
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll([]byte(payload), nil)
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
		t.Fatalf("status = %d, body = %s, want the large request served by Azure", response.StatusCode, body)
	}
	if received.Load() < int64(len(payload)/2) {
		t.Fatalf("azure received %d bytes, want the whole conversation", received.Load())
	}
}

func TestCodexTurnFailureClass(t *testing.T) {
	cases := map[string]codexFailureClass{
		`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`:                          codexFailureServer,
		`{"type":"response.failed","response":{"error":{"code":"slow_down"}}}`:                                     codexFailureServer,
		`{"type":"response.failed","response":{"error":{"code":"a_code_from_the_future"}}}`:                        codexFailureServer,
		`{"type":"response.failed","response":{"error":{"message":"something odd happened"}}}`:                     codexFailureServer,
		`{"type":"error","code":"usage_limit_reached"}`:                                                            codexFailureQuota,
		`{"type":"response.failed","response":{"error":{"code":"insufficient_quota"}}}`:                            codexFailureQuota,
		`{"type":"response.failed","response":{"error":{"code":"usage_not_included"}}}`:                            codexFailureQuota,
		`{"error":{"code":"rate_limit_exceeded"}}`:                                                                 codexFailureQuota,
		`{"type":"response.failed","response":{"error":{"message":"You have hit your usage limit."}}}`:             codexFailureQuota,
		`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded"}}}`:                       codexFailureClient,
		`{"type":"response.failed","response":{"error":{"message":"input exceeds the maximum context length"}}}`:   codexFailureClient,
		`{"type":"response.failed","response":{"error":{"code":"invalid_prompt"}}}`:                                codexFailureClient,
		`{"type":"response.failed","response":{"error":{"code":"cyber_policy"}}}`:                                  codexFailureClient,
		`{"type":"response.failed","response":{"error":{"code":"unsupported_value","param":"reasoning.context"}}}`: codexFailureClient,
		`{"type":"response.output_text.delta","delta":"usage limit reached"}`:                                      codexFailureNone,
		`{"type":"response.completed","response":{"id":"resp_1"}}`:                                                 codexFailureNone,
		`not json`: codexFailureNone,
	}
	for body, want := range cases {
		if got := codexTurnFailureClass([]byte(body)); got != want {
			t.Fatalf("codexTurnFailureClass(%s) = %v, want %v", body, got, want)
		}
	}
}

// A quota failure inside a 200 stream is the stream form of a 429: the account
// gets marked so the next pick avoids it, and Azure finishes the turn.
func TestAzureCodexStreamQuotaMarksAccountAndDiverts(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"usage_limit_reached\"}}}\n\n")
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	var azureCalls atomic.Int32
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		azureCalls.Add(1)
		_, _ = io.WriteString(w, `{"id":"resp_azure"}`)
	})
	server := azureCodexFallbackServer(t, azureURL, poolURL, 1)
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "codex-account-0", Headroom: 0.80, ShortHeadroom: 0.80},
	}))
	server.SchedulerRef = schedulerRef
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	response, err := http.Post(proxy.URL+"/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"session-stream-quota","stream":true,"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp_azure") {
		t.Fatalf("response = %d %s, want the Azure answer", response.StatusCode, body)
	}
	if azureCalls.Load() != 1 {
		t.Fatalf("azure calls = %d, want 1", azureCalls.Load())
	}
	if _, marked := schedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, "codex-account-0", ""); !marked {
		t.Fatal("a stream quota failure must mark the account like a 429 would")
	}
}

// A code the proxy has never seen ends the turn if forwarded, so it diverts.
func TestAzureCodexStreamUnknownFailureDiverts(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"quantum_flux_disruption\"}}}\n\n")
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	var azureCalls atomic.Int32
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		azureCalls.Add(1)
		_, _ = io.WriteString(w, `{"id":"resp_azure"}`)
	})
	proxy := httptest.NewServer(azureCodexFallbackServer(t, azureURL, poolURL, 1).Handler())
	defer proxy.Close()

	response, err := http.Post(proxy.URL+"/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"session-unknown","stream":true,"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp_azure") {
		t.Fatalf("response = %d %s, want the Azure answer for an unknown failure code", response.StatusCode, body)
	}
}

// A context-window failure is the request's own fault: Azure would refuse it
// the same way for money, so the stream passes through untouched.
func TestAzureCodexStreamClientFailurePassesThrough(t *testing.T) {
	stream := "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"context_length_exceeded\"}}}\n\n"
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	var azureCalls atomic.Int32
	_, azureURL := azureCodexTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		azureCalls.Add(1)
	})
	proxy := httptest.NewServer(azureCodexFallbackServer(t, azureURL, poolURL, 1).Handler())
	defer proxy.Close()

	response, err := http.Post(proxy.URL+"/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-codex","session_id":"session-ctx","stream":true,"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != stream {
		t.Fatalf("body = %q, want the pool's own failure untouched", body)
	}
	if azureCalls.Load() != 0 {
		t.Fatalf("azure calls = %d, want 0 for a client-caused failure", azureCalls.Load())
	}
}

// Codex replays compaction items as well as reasoning items, and both carry a
// blob sealed by the provider that made them. Keying the strip on the item type
// left compaction items behind, so the retry resent an identical body and the
// turn failed.
func TestAzureCodexStripsSealedContentFromAnyItemType(t *testing.T) {
	payload := map[string]json.RawMessage{
		"input": json.RawMessage(`[` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"go on"}]},` +
			`{"type":"compaction","id":"cmp_1","encrypted_content":"sealed"},` +
			`{"type":"reasoning","id":"rs_1","encrypted_content":"sealed","summary":[{"type":"summary_text","text":"kept"}]}` +
			`]`),
	}
	if !azureCodexStripEncryptedReasoning(payload) {
		t.Fatal("nothing was stripped")
	}
	var items []map[string]any
	if err := json.Unmarshal(payload["input"], &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want the empty compaction item dropped and the rest kept: %v", len(items), items)
	}
	for _, item := range items {
		if _, sealed := item["encrypted_content"]; sealed {
			t.Fatalf("a sealed blob survived: %v", item)
		}
	}
	if items[0]["role"] != "user" {
		t.Fatalf("the user turn was disturbed: %v", items[0])
	}
	if items[1]["type"] != "reasoning" {
		t.Fatalf("the readable reasoning summary was dropped: %v", items[1])
	}
}
