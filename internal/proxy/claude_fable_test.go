package proxy

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestStripClaudeUnsupportedFields(t *testing.T) {
	in := []byte(`{"model":"claude-fable-5","context_management":{"edits":[]},"max_tokens":8,"messages":[]}`)
	out := stripClaudeUnsupportedFields(in)
	if strings.Contains(string(out), "context_management") {
		t.Fatalf("context_management not stripped: %s", out)
	}
	if !strings.Contains(string(out), "claude-fable-5") || !strings.Contains(string(out), "max_tokens") {
		t.Fatalf("other fields lost: %s", out)
	}
	// No context_management => unchanged.
	clean := []byte(`{"model":"claude-fable-5"}`)
	if string(stripClaudeUnsupportedFields(clean)) != string(clean) {
		t.Fatalf("clean body altered")
	}
}

func TestClaudeFableRequestDetection(t *testing.T) {
	s := Server{MaxBodyBytes: 1 << 20, ClaudeFableAPIKey: "sk-ant-fable-key"}
	fable := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-fable-5","messages":[]}`))
	fable.Header.Set("Content-Type", "application/json")
	if !s.claudeFableRequest(fable) {
		t.Fatal("fable messages POST should be detected")
	}
	// The body must survive the probe for the normal proxy path.
	b, _ := io.ReadAll(fable.Body)
	if !strings.Contains(string(b), "claude-fable-5") {
		t.Fatalf("request body not restored after probe: %q", b)
	}
	opus := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
	opus.Header.Set("Content-Type", "application/json")
	if s.claudeFableRequest(opus) {
		t.Fatal("opus must NOT be detected as fable")
	}
	b, _ = io.ReadAll(opus.Body)
	if !strings.Contains(string(b), "claude-opus-4-8") {
		t.Fatalf("request body not restored for non-fable: %q", b)
	}
	get := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	if s.claudeFableRequest(get) {
		t.Fatal("GET must not be detected as fable")
	}
}

func TestServeClaudeFableFallbackViaAPIKey(t *testing.T) {
	var captured *http.Request
	var capturedBody string
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		captured = req
		b, _ := io.ReadAll(req.Body)
		capturedBody = string(b)
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"type":"message","usage":{"output_tokens":3}}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	s := Server{ClaudeFableAPIKey: "sk-ant-fable-key", Transport: rt}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-fable-5","context_management":{"edits":[]},"max_tokens":8,"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer oauth-should-be-dropped")
	rec := httptest.NewRecorder()

	if !s.serveClaudeFableFallback(rec, req) {
		t.Fatal("expected fable request to be handled")
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if captured == nil {
		t.Fatal("no upstream request captured")
	}
	if captured.URL.String() != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("upstream URL = %q", captured.URL.String())
	}
	if captured.Header.Get("X-Api-Key") != "sk-ant-fable-key" {
		t.Fatalf("x-api-key = %q, want the fable key", captured.Header.Get("X-Api-Key"))
	}
	if captured.Header.Get("Authorization") != "" {
		t.Fatalf("Authorization should be dropped, got %q", captured.Header.Get("Authorization"))
	}
	if strings.Contains(capturedBody, "context_management") {
		t.Fatalf("context_management not stripped from forwarded body: %s", capturedBody)
	}
}

func TestClaudeFableFallbackPrefersBedrockOverAPIKey(t *testing.T) {
	bedrockCalls := 0
	bedrockRT := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		bedrockCalls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"type":"message","usage":{"output_tokens":3}}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	apiRT := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Error("api key must not be used while Bedrock serves the request")
		return nil, errors.New("unexpected api-key call")
	})
	s := Server{
		ClaudeFableAPIKey: "sk-ant-fable-key",
		Transport:         apiRT,
		Bedrock: &BedrockConfig{
			Regions:   []string{"us-east-1"},
			Sources:   []BedrockCredentialSource{{Name: "aw0", Credentials: staticBedrockCreds()}},
			Transport: bedrockRT,
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, ok := s.claudeFableFallbackResponse(req, []byte(`{"model":"claude-fable-5","max_tokens":8,"messages":[]}`))
	if !ok {
		t.Fatal("fallback chain should produce a response")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if bedrockCalls != 1 {
		t.Fatalf("bedrock calls = %d, want 1", bedrockCalls)
	}
}

func TestClaudeFableFallbackBedrockUnusableFallsToAPIKey(t *testing.T) {
	for _, bedrockStatus := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusInternalServerError, http.StatusForbidden} {
		bedrockRT := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: bedrockStatus,
				Body:       io.NopCloser(strings.NewReader(`{"message":"Bedrock is unable to process your request."}`)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		})
		apiCalls := 0
		apiRT := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			apiCalls++
			if got := req.Header.Get("X-Api-Key"); got != "sk-ant-fable-key" {
				t.Errorf("x-api-key = %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"type":"message"}`)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		})
		s := Server{
			ClaudeFableAPIKey: "sk-ant-fable-key",
			Transport:         apiRT,
			Bedrock: &BedrockConfig{
				Regions:   []string{"us-east-1"},
				Sources:   []BedrockCredentialSource{{Name: "aw0", Credentials: staticBedrockCreds()}},
				Transport: bedrockRT,
			},
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
		resp, ok := s.claudeFableFallbackResponse(req, []byte(`{"model":"claude-fable-5","max_tokens":8,"messages":[]}`))
		if !ok {
			t.Fatalf("bedrock %d: fallback chain should reach the api key", bedrockStatus)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("bedrock %d: status = %d, want 200 from api key", bedrockStatus, resp.StatusCode)
		}
		if apiCalls != 1 {
			t.Fatalf("bedrock %d: api key calls = %d, want 1", bedrockStatus, apiCalls)
		}
	}
}

func TestClaudeFableFallbackBedrockTransportErrorFallsToAPIKey(t *testing.T) {
	bedrockRT := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset by peer")
	})
	apiRT := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"type":"message"}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	s := Server{
		ClaudeFableAPIKey: "sk-ant-fable-key",
		Transport:         apiRT,
		Bedrock: &BedrockConfig{
			Regions:   []string{"us-east-1"},
			Sources:   []BedrockCredentialSource{{Name: "aw0", Credentials: staticBedrockCreds()}},
			Transport: bedrockRT,
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, ok := s.claudeFableFallbackResponse(req, []byte(`{"model":"claude-fable-5","max_tokens":8,"messages":[]}`))
	if !ok {
		t.Fatal("bedrock transport error should fall through to the api key")
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from api key", resp.StatusCode)
	}
}

func TestClaudeFableFallbackBedrock429WithoutAPIKeyReturnsBedrockResponse(t *testing.T) {
	bedrockRT := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"message":"Too many tokens, please wait before trying again."}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	s := Server{
		Bedrock: &BedrockConfig{
			Regions:   []string{"us-east-1"},
			Sources:   []BedrockCredentialSource{{Name: "aw0", Credentials: staticBedrockCreds()}},
			Transport: bedrockRT,
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, ok := s.claudeFableFallbackResponse(req, []byte(`{"model":"claude-fable-5","max_tokens":8,"messages":[]}`))
	if !ok {
		t.Fatal("with no api key the bedrock 429 is still the chain's answer")
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the bedrock 429 surfaced", resp.StatusCode)
	}
}

func TestServeClaudeFableFallbackUsesBedrockSources(t *testing.T) {
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"type":"message","usage":{"output_tokens":3}}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	s := Server{Bedrock: &BedrockConfig{
		Regions:   []string{"us-east-1"},
		Sources:   []BedrockCredentialSource{{Name: "aw0", Credentials: staticBedrockCreds()}},
		Transport: rt,
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-fable-5","max_tokens":8,"messages":[]}`))
	rec := httptest.NewRecorder()

	if !s.serveClaudeFableFallback(rec, req) {
		t.Fatal("expected fable request to be handled by Bedrock source config")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestClaudeFableBedrockResponseStreamsSSE(t *testing.T) {
	frames := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":11}}}`),
		buildEventStreamFrame(t, `{"type":"message_delta","usage":{"output_tokens":7}}`)...,
	)
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/invoke-with-response-stream") {
			t.Errorf("stream request must use the streaming endpoint, got %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(frames)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := Server{Bedrock: &BedrockConfig{
		Regions:   []string{"us-east-1"},
		Sources:   []BedrockCredentialSource{{Name: "aw0", Credentials: staticBedrockCreds()}},
		Transport: rt,
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := s.claudeFableBedrockResponse(req.Context(), []byte(`{"model":"claude-fable-5","stream":true,"max_tokens":8,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", got)
	}
	sse, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sse), "event: message_start") || !strings.Contains(string(sse), "event: message_delta") {
		t.Fatalf("transcoded SSE missing events: %q", sse)
	}
}

func TestUsageLimitRetryTransportClaudeFableFallsBackAfterPoolExhausted(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-fable", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	// Every OAuth account is genuinely out of quota.
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`)),
			Header:     h,
		}
	}}
	bedrockRT := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"type":"message","usage":{"output_tokens":3}}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	server.Bedrock = &BedrockConfig{
		Regions:   []string{"us-east-1"},
		Sources:   []BedrockCredentialSource{{Name: "aw0", Credentials: staticBedrockCreds()}},
		Transport: bedrockRT,
	}
	fableBody := []byte(`{"model":"claude-fable-5","max_tokens":8,"messages":[]}`)
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(fableBody))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(fableBody)), nil }
	req.Header.Set("Authorization", "Bearer tok-cooked")
	transport := usageLimitRetryTransport{
		base:        stub,
		server:      &server,
		provider:    accounts.ProviderClaude,
		agent:       "claude",
		session:     "session-fable",
		account:     "cooked@example.com",
		method:      http.MethodPost,
		path:        "/v1/messages",
		maxAttempts: 4,
		fableFallback: func() (*http.Response, bool) {
			return server.claudeFableFallbackResponse(req, fableBody)
		},
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from bedrock after pool exhausted", response.StatusCode)
	}
	if stub.calls != 2 {
		t.Fatalf("pool attempts = %d, want 2 (both OAuth accounts tried before fallback)", stub.calls)
	}
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), `"type":"message"`) {
		t.Fatalf("fallback body = %q, want bedrock message", body)
	}
}

func TestUsageLimitRetryTransportClaudeFableFallbackSuppressesExhaustedLog(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-fable", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`)),
			Header:     h,
		}
	}}
	bedrockRT := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"type":"message","usage":{"output_tokens":3}}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	server.Bedrock = &BedrockConfig{
		Regions:   []string{"us-east-1"},
		Sources:   []BedrockCredentialSource{{Name: "aw0", Credentials: staticBedrockCreds()}},
		Transport: bedrockRT,
	}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	fableBody := []byte(`{"model":"claude-fable-5","max_tokens":8,"messages":[]}`)
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(fableBody))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(fableBody)), nil }
	req.Header.Set("Authorization", "Bearer tok-cooked")
	transport := usageLimitRetryTransport{
		base:        stub,
		server:      &server,
		logger:      logger,
		provider:    accounts.ProviderClaude,
		agent:       "claude",
		session:     "session-fable",
		account:     "cooked@example.com",
		method:      http.MethodPost,
		path:        "/v1/messages",
		maxAttempts: 4,
		fableFallback: func() (*http.Response, bool) {
			return server.claudeFableFallbackResponse(req, fableBody)
		},
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from bedrock after pool exhausted", response.StatusCode)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "serving claude fable via fallback chain") {
		t.Fatalf("expected fable fallback save log; logs=\n%s", logs)
	}
	if strings.Contains(logs, "returned to client after failover exhausted") {
		t.Fatalf("did not expect client-facing failover-exhaustion signal on fallback save; logs=\n%s", logs)
	}
}

func TestUsageLimitRetryTransportClaudeFableFallbackFailureLogsExhausted(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-fable", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`)),
			Header:     h,
		}
	}}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	fableBody := []byte(`{"model":"claude-fable-5","max_tokens":8,"messages":[]}`)
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(fableBody))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(fableBody)), nil }
	req.Header.Set("Authorization", "Bearer tok-cooked")
	transport := usageLimitRetryTransport{
		base:        stub,
		server:      &server,
		logger:      logger,
		provider:    accounts.ProviderClaude,
		agent:       "claude",
		session:     "session-fable",
		account:     "cooked@example.com",
		method:      http.MethodPost,
		path:        "/v1/messages",
		maxAttempts: 4,
		fableFallback: func() (*http.Response, bool) {
			return nil, false
		},
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want pool 429", response.StatusCode)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "claude rate-limit returned to client after failover exhausted") {
		t.Fatalf("expected client-facing failover-exhaustion signal; logs=\n%s", logs)
	}
}

func TestOauthRetryAccountSkipsAPIKeyAccountsWhenOAuthOnly(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	server.Accounts = append(server.Accounts, accounts.Account{
		ID: "apikey-fallback", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "sk-ant-pool-key",
	})
	tried := map[string]struct{}{
		"cooked@example.com": {},
		"fresh@example.com":  {},
	}
	if _, err := server.oauthRetryAccount(t.Context(), accounts.ProviderClaude, "claude", "s", "", "", tried, true); err == nil {
		t.Fatal("oauthOnly must not hand out the API-key pool account")
	}
	account, err := server.oauthRetryAccount(t.Context(), accounts.ProviderClaude, "claude", "s", "", "", tried, false)
	if err != nil {
		t.Fatalf("non-oauthOnly retry should use the API-key account: %v", err)
	}
	if account.AuthMode != accounts.AuthModeAPIKey {
		t.Fatalf("account = %+v, want the API-key fallback", account)
	}
}

// Bedrock's Anthropic schema 400s on OAuth-only fields ("context_management:
// Extra inputs are not permitted", observed live), which used to push every
// context-editing Claude Code request straight past Bedrock to the API key.
func TestClaudeFableBedrockStripsUnsupportedFields(t *testing.T) {
	var forwarded string
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		forwarded = string(b)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"type":"message"}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	s := Server{Bedrock: &BedrockConfig{
		Regions:   []string{"us-east-1"},
		Sources:   []BedrockCredentialSource{{Name: "aw0", Credentials: staticBedrockCreds()}},
		Transport: rt,
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := s.claudeFableBedrockResponse(req.Context(), []byte(`{"model":"claude-fable-5","context_management":{"edits":[]},"max_tokens":8,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if strings.Contains(forwarded, "context_management") {
		t.Fatalf("context_management not stripped for bedrock: %s", forwarded)
	}
	if !strings.Contains(forwarded, "anthropic_version") {
		t.Fatalf("bedrock body missing anthropic_version: %s", forwarded)
	}
}
