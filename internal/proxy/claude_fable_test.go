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

// bedrockPrimaryServer builds a Server with the Bedrock gateway configured and
// FableBedrockPrimary enabled, whose Bedrock upstream returns the given status
// and body.
func bedrockPrimaryServer(status int, body string) Server {
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	return Server{
		MaxBodyBytes:        1 << 20,
		FableBedrockPrimary: true,
		Bedrock:             &BedrockConfig{Regions: []string{"us-east-1"}, Credentials: staticBedrockCreds(), Transport: rt},
	}
}

func TestServeClaudeFableBedrockPrimarySuccess(t *testing.T) {
	s := bedrockPrimaryServer(200, `{"model":"claude-fable-5","content":[{"type":"text","text":"ok"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-fable-5","max_tokens":8,"messages":[]}`))
	rec := httptest.NewRecorder()
	if !s.serveClaudeFableBedrockPrimary(rec, req) {
		t.Fatal("expected bedrock-primary to serve a 2xx Bedrock response")
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "claude-fable-5") {
		t.Fatalf("body = %q, want forwarded Bedrock body", rec.Body.String())
	}
}

func TestServeClaudeFableBedrockPrimaryFallsThroughOnNon2xx(t *testing.T) {
	s := bedrockPrimaryServer(429, `{"message":"Too many requests"}`)
	bodyStr := `{"model":"claude-fable-5","max_tokens":8,"messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(bodyStr))
	rec := httptest.NewRecorder()
	if s.serveClaudeFableBedrockPrimary(rec, req) {
		t.Fatal("expected bedrock-primary to fall through on a non-2xx Bedrock response")
	}
	if rec.Code != 200 { // httptest default; nothing should have been written
		t.Fatalf("status = %d, want untouched recorder (nothing written)", rec.Code)
	}
	// Body must be restored so the pool path can read it.
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(restored) != bodyStr {
		t.Fatalf("restored body = %q, want %q", string(restored), bodyStr)
	}
}

func fableStreamTestServer(rt bedrockRoundTripFunc) Server {
	return Server{Bedrock: &BedrockConfig{
		Regions:   []string{"us-east-1"},
		Sources:   []BedrockCredentialSource{{Name: "aw0", Credentials: staticBedrockCreds()}},
		Transport: rt,
	}}
}

var fableStreamTestBody = []byte(`{"model":"claude-fable-5","stream":true,"max_tokens":8,"messages":[]}`)

// An in-band Anthropic error event as the first frame used to slip through the
// exception-only peek, committing a 200 SSE that immediately carried the error.
// It must instead be retried; the second attempt succeeds here.
func TestClaudeFableBedrockRetriesOverloadedFirstFrame(t *testing.T) {
	good := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		buildEventStreamFrame(t, `{"type":"message_stop"}`)...,
	)
	overloaded := buildEventStreamFrame(t, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	calls := 0
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body := overloaded
		if calls >= 2 {
			body = good
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := fableStreamTestServer(rt)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := s.claudeFableBedrockResponse(req.Context(), fableStreamTestBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retry", resp.StatusCode)
	}
	sse, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sse), "event: message_stop") {
		t.Fatalf("retried stream missing message_stop: %q", sse)
	}
	if strings.Contains(string(sse), "overloaded_error") {
		t.Fatalf("first attempt's error leaked into the retried stream: %q", sse)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestClaudeFableBedrockOverloadedFirstFrameExhaustsRetries(t *testing.T) {
	overloaded := buildEventStreamFrame(t, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	calls := 0
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(overloaded)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := fableStreamTestServer(rt)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := s.claudeFableBedrockResponse(req.Context(), fableStreamTestBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 so the fallback chain can take over", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "overloaded_error") {
		t.Fatalf("503 body should carry the upstream error, got %q", respBody)
	}
	if calls != claudeFableBedrockStreamAttempts {
		t.Fatalf("upstream calls = %d, want %d", calls, claudeFableBedrockStreamAttempts)
	}
}

func TestClaudeFableBedrockInvalidRequestFirstFrameDoesNotRetry(t *testing.T) {
	invalid := buildEventStreamFrame(t, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
	calls := 0
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(invalid)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := fableStreamTestServer(rt)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := s.claudeFableBedrockResponse(req.Context(), fableStreamTestBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry for validation errors)", calls)
	}
}

func TestClaudeFableBedrockRetriesExceptionFirstFrame(t *testing.T) {
	good := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		buildEventStreamFrame(t, `{"type":"message_stop"}`)...,
	)
	throttle := buildBedrockExceptionFrame(t, "throttlingException", `{"message":"Too many requests"}`)
	calls := 0
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body := throttle
		if calls >= 2 {
			body = good
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := fableStreamTestServer(rt)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := s.claudeFableBedrockResponse(req.Context(), fableStreamTestBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retrying past the throttle exception", resp.StatusCode)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

// Overload frequently arrives after message_start: the stream opens, then dies
// before any content. That window must retry exactly like a first-frame error.
func TestClaudeFableBedrockRetriesErrorAfterMessageStart(t *testing.T) {
	bad := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		buildEventStreamFrame(t, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)...,
	)
	good := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		append(
			buildEventStreamFrame(t, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			buildEventStreamFrame(t, `{"type":"message_stop"}`)...,
		)...,
	)
	calls := 0
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body := bad
		if calls >= 2 {
			body = good
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := fableStreamTestServer(rt)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := s.claudeFableBedrockResponse(req.Context(), fableStreamTestBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retrying past a post-message_start error", resp.StatusCode)
	}
	sse, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sse), "event: message_stop") {
		t.Fatalf("retried stream missing message_stop: %q", sse)
	}
	if strings.Contains(string(sse), "overloaded_error") {
		t.Fatalf("failed attempt's frames leaked into the retried stream: %q", sse)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

// Once content has streamed, the response is committed and a later error must
// pass through unchanged; retrying would duplicate output the client already
// rendered.
func TestClaudeFableBedrockDoesNotRetryErrorAfterContent(t *testing.T) {
	stream := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		append(
			buildEventStreamFrame(t, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			append(
				buildEventStreamFrame(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
				buildEventStreamFrame(t, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)...,
			)...,
		)...,
	)
	calls := 0
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(stream)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := fableStreamTestServer(rt)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := s.claudeFableBedrockResponse(req.Context(), fableStreamTestBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want committed 200", resp.StatusCode)
	}
	sse, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sse), "content_block_start") || !strings.Contains(string(sse), "overloaded_error") {
		t.Fatalf("committed stream must pass content and the error through: %q", sse)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry after commit)", calls)
	}
}

func TestStripBedrockUnsupportedTools(t *testing.T) {
	body := []byte(`{"model":"claude-fable-5","tools":[` +
		`{"type":"web_search_20250305","name":"web_search","max_uses":8},` +
		`{"name":"Bash","description":"run a command","input_schema":{"type":"object"}},` +
		`{"type":"text_editor_20250124","name":"str_replace_editor"}` +
		`],"max_tokens":8,"messages":[]}`)
	got, dropped := stripBedrockUnsupportedTools(body)
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	s := string(got)
	if strings.Contains(s, "web_search") {
		t.Fatalf("web_search survived the strip: %s", s)
	}
	if !strings.Contains(s, `"Bash"`) || !strings.Contains(s, "text_editor_20250124") {
		t.Fatalf("client or bedrock-supported tools were dropped: %s", s)
	}

	// Only unsupported tools: the tools key disappears entirely.
	got, dropped = stripBedrockUnsupportedTools([]byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[]}`))
	if dropped != 1 || strings.Contains(string(got), "tools") {
		t.Fatalf("expected tools key removed, got dropped=%d body=%s", dropped, got)
	}

	// No tools: untouched.
	in := []byte(`{"model":"claude-fable-5","messages":[]}`)
	got, dropped = stripBedrockUnsupportedTools(in)
	if dropped != 0 || !bytes.Equal(got, in) {
		t.Fatalf("no-tools body must pass through unchanged, dropped=%d", dropped)
	}
}

// The Bedrock request body must not carry web_search after the strip, while
// the same request forwarded to the Anthropic API key path keeps its tools.
func TestClaudeFableBedrockRequestDropsWebSearchTool(t *testing.T) {
	var upstreamBody string
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		upstreamBody = string(b)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"type":"message","usage":{"output_tokens":3}}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	s := fableStreamTestServer(rt)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	body := []byte(`{"model":"claude-fable-5","max_tokens":8,"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"Bash","input_schema":{"type":"object"}}],"messages":[]}`)
	resp, err := s.claudeFableBedrockResponse(req.Context(), body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if strings.Contains(upstreamBody, "web_search") {
		t.Fatalf("web_search reached Bedrock: %s", upstreamBody)
	}
	if !strings.Contains(upstreamBody, `"Bash"`) {
		t.Fatalf("client tool was lost: %s", upstreamBody)
	}
}

// A shed between content_block_start and the first delta (seen live: 762ms in,
// zero tokens) must retry: the block-open frame carries nothing the client
// needs, so the stream is still replayable.
func TestClaudeFableBedrockRetriesErrorAfterBlockStartBeforeDelta(t *testing.T) {
	bad := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		append(
			buildEventStreamFrame(t, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
			buildEventStreamFrame(t, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)...,
		)...,
	)
	good := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		append(
			buildEventStreamFrame(t, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			append(
				buildEventStreamFrame(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
				buildEventStreamFrame(t, `{"type":"message_stop"}`)...,
			)...,
		)...,
	)
	calls := 0
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body := bad
		if calls >= 2 {
			body = good
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := fableStreamTestServer(rt)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := s.claudeFableBedrockResponse(req.Context(), fableStreamTestBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retrying past a pre-delta shed", resp.StatusCode)
	}
	sse, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(sse), "event: message_stop") || strings.Contains(string(sse), "overloaded_error") {
		t.Fatalf("retried stream wrong: %q", sse)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

// A shed during early thinking (within the commit window) retries silently:
// thinking deltas inside the window are buffered, so nothing reached the client.
func TestClaudeFableBedrockRetriesShedDuringEarlyThinking(t *testing.T) {
	bad := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		append(
			buildEventStreamFrame(t, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
			append(
				buildEventStreamFrame(t, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`),
				append(
					buildEventStreamFrame(t, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm2"}}`),
					buildEventStreamFrame(t, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)...,
				)...,
			)...,
		)...,
	)
	good := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		append(
			buildEventStreamFrame(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
			buildEventStreamFrame(t, `{"type":"message_stop"}`)...,
		)...,
	)
	calls := 0
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body := bad
		if calls >= 2 {
			body = good
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := fableStreamTestServer(rt)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := s.claudeFableBedrockResponse(req.Context(), fableStreamTestBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retrying a shed during buffered thinking", resp.StatusCode)
	}
	sse, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(sse), "overloaded_error") || strings.Contains(string(sse), "hmm") {
		t.Fatalf("failed attempt's thinking or error leaked: %q", sse)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

// Once the commit window has elapsed, a thinking delta commits the stream and
// a later shed passes through instead of retrying.
func TestClaudeFableBedrockCommitsThinkingAfterWindow(t *testing.T) {
	saved := claudeFableBedrockCommitWindow
	claudeFableBedrockCommitWindow = 0
	defer func() { claudeFableBedrockCommitWindow = saved }()

	stream := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		append(
			buildEventStreamFrame(t, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
			append(
				buildEventStreamFrame(t, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"deep"}}`),
				buildEventStreamFrame(t, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)...,
			)...,
		)...,
	)
	calls := 0
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(stream)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := fableStreamTestServer(rt)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := s.claudeFableBedrockResponse(req.Context(), fableStreamTestBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want committed 200", resp.StatusCode)
	}
	sse, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(sse), "deep") || !strings.Contains(string(sse), "overloaded_error") {
		t.Fatalf("committed stream must pass thinking and the error through: %q", sse)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry after window commit)", calls)
	}
}

// Bedrock primes each block with an empty first delta (transcript-verified:
// input_json_delta{partial_json:""}). A shed right after that priming frame
// must retry: the client received zero payload.
func TestClaudeFableBedrockRetriesShedAfterEmptyPrimingDelta(t *testing.T) {
	bad := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		append(
			buildEventStreamFrame(t, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_x","name":"Bash","input":{}}}`),
			append(
				buildEventStreamFrame(t, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`),
				buildEventStreamFrame(t, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)...,
			)...,
		)...,
	)
	good := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		append(
			buildEventStreamFrame(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
			buildEventStreamFrame(t, `{"type":"message_stop"}`)...,
		)...,
	)
	calls := 0
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body := bad
		if calls >= 2 {
			body = good
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := fableStreamTestServer(rt)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := s.claudeFableBedrockResponse(req.Context(), fableStreamTestBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retrying past an empty priming delta", resp.StatusCode)
	}
	sse, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(sse), "overloaded_error") || strings.Contains(string(sse), "toolu_x") {
		t.Fatalf("failed attempt leaked into retried stream: %q", sse)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

// A delta with real tool-input payload still commits immediately.
func TestClaudeFableBedrockCommitsOnNonEmptyToolDelta(t *testing.T) {
	stream := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		append(
			buildEventStreamFrame(t, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_y","name":"Bash","input":{}}}`),
			append(
				buildEventStreamFrame(t, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"comm"}}`),
				buildEventStreamFrame(t, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)...,
			)...,
		)...,
	)
	calls := 0
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(stream)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := fableStreamTestServer(rt)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	resp, err := s.claudeFableBedrockResponse(req.Context(), fableStreamTestBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls != 1 {
		t.Fatalf("status=%d calls=%d, want committed 200 with 1 call", resp.StatusCode, calls)
	}
	sse, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(sse), "toolu_y") || !strings.Contains(string(sse), "overloaded_error") {
		t.Fatalf("committed stream must pass tool block and error through: %q", sse)
	}
}
