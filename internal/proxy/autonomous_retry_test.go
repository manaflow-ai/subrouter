package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

type autonomousRetryRoundTripper func(*http.Request) (*http.Response, error)

func (f autonomousRetryRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestAgentRetryPolicyFor(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if got := agentRetryPolicyFor(request); got != agentRetryBounded {
		t.Fatalf("missing header policy = %v, want bounded", got)
	}
	request.Header.Set(AgentRetryPolicyHeader, " AUTONOMOUS ")
	if got := agentRetryPolicyFor(request); got != agentRetryAutonomous {
		t.Fatalf("autonomous header policy = %v", got)
	}
	request.Header.Set(AgentRetryPolicyHeader, "bounded")
	if got := agentRetryPolicyFor(request); got != agentRetryBounded {
		t.Fatalf("bounded header policy = %v", got)
	}
}

func TestAutonomousRetryScopeExclusions(t *testing.T) {
	for name, inScope := range map[string]bool{
		"pooled":            autonomousRetryInScope(agentRetryAutonomous, false, false, false),
		"bounded":           autonomousRetryInScope(agentRetryBounded, false, false, false),
		"no-retry canary":   autonomousRetryInScope(agentRetryAutonomous, true, false, false),
		"forced account":    autonomousRetryInScope(agentRetryAutonomous, false, true, false),
		"broker/lease path": autonomousRetryInScope(agentRetryAutonomous, false, false, true),
	} {
		want := name == "pooled"
		if inScope != want {
			t.Errorf("%s in scope = %t, want %t", name, inScope, want)
		}
	}
}

func TestAutonomousAgentRetryTransportRetriesUntilSuccess(t *testing.T) {
	attempts := 0
	var waits []time.Duration
	const requestBody = `{"model":"gpt-6-astra","input":"request"}`
	transport := autonomousAgentRetryTransport{
		provider: accounts.ProviderCodex,
		base: autonomousRetryRoundTripper(func(request *http.Request) (*http.Response, error) {
			attempts++
			body, readErr := io.ReadAll(request.Body)
			if readErr != nil || string(body) != requestBody {
				t.Fatalf("attempt %d model body = %q, err %v", attempts, body, readErr)
			}
			switch attempts {
			case 1:
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("busy"))}, nil
			case 2:
				return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader("slow"))}, nil
			case 3:
				return nil, errors.New("connection reset by peer")
			default:
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			}
		}),
		sleep: func(_ context.Context, wait time.Duration) bool {
			waits = append(waits, wait)
			return true
		},
	}
	request, err := http.NewRequest(http.MethodPost, "http://example.test/v1/responses", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response = %#v, err %v", response, err)
	}
	if attempts != 4 {
		t.Fatalf("attempts = %d, want 4", attempts)
	}
	wantWaits := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(waits) != len(wantWaits) {
		t.Fatalf("waits = %v, want %v", waits, wantWaits)
	}
	for i := range wantWaits {
		if waits[i] != wantWaits[i] {
			t.Fatalf("waits = %v, want %v", waits, wantWaits)
		}
	}
}

func TestAutonomousAgentRetryStatusLivesOnlyDuringBackoff(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	server := &Server{retryStatuses: newRetryStatusRegistry()}
	attempts := 0
	transport := autonomousAgentRetryTransport{
		server: server, provider: accounts.ProviderCodex, model: "gpt-6-astra",
		agent: "codex", session: "session-retry", account: "codex-account-1",
		now: func() time.Time { return now },
		base: autonomousRetryRoundTripper(func(*http.Request) (*http.Response, error) {
			attempts++
			status := http.StatusServiceUnavailable
			if attempts == 2 {
				status = http.StatusOK
			}
			return &http.Response{StatusCode: status, Body: http.NoBody}, nil
		}),
		sleep: func(context.Context, time.Duration) bool {
			status := server.retryStatuses.forSession("codex", "session-retry")
			if status == nil || status.Provider != accounts.ProviderCodex || status.Model != "gpt-6-astra" ||
				status.AccountID != "codex-account-1" || status.Attempt != 2 || status.Reason != "http_503" ||
				!status.NextRetryAt.Equal(now.Add(time.Second)) {
				t.Fatalf("retry status during autonomous wait = %+v", status)
			}
			return true
		},
	}
	request, err := http.NewRequest(http.MethodPost, "http://example.test/v1/responses", strings.NewReader("request"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	if status := server.retryStatuses.forSession("codex", "session-retry"); status != nil {
		t.Fatalf("retry status after success = %+v, want nil", status)
	}
}

func TestAutonomousAgentRetryPreservesRotatedAccount(t *testing.T) {
	server := &Server{retryStatuses: newRetryStatusRegistry()}
	attempts := 0
	transport := autonomousAgentRetryTransport{
		server: server, provider: accounts.ProviderClaude, model: "claude-fable-5",
		agent: "claude", session: "session-rotated", account: "account-original",
		base: autonomousRetryRoundTripper(func(request *http.Request) (*http.Response, error) {
			attempts++
			if attempts == 1 {
				rotatedRequest := request.Clone(request.Context())
				rotatedRequest.Header = request.Header.Clone()
				rotatedRequest.Header.Set("Authorization", "Bearer rotated-token")
				response := &http.Response{
					StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: http.NoBody,
					Request: rotatedRequest,
				}
				return tagRoutedResponseAccount(response, accounts.Account{
					ID: "account-rotated", Provider: accounts.ProviderClaude, CredentialVersion: "rotated-v1",
				}), nil
			}
			if got := request.Header.Get("Authorization"); got != "Bearer rotated-token" {
				t.Fatalf("second pass authorization = %q, want rotated account", got)
			}
			if selected, ok := attemptAccount(request.Context()); !ok || selected.ID != "account-rotated" {
				t.Fatalf("second pass attempt account = %+v, %t", selected, ok)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
		}),
		sleep: func(context.Context, time.Duration) bool {
			status := server.retryStatuses.forSession("claude", "session-rotated")
			if status == nil || status.AccountID != "account-rotated" {
				t.Fatalf("retry attribution = %+v, want rotated account", status)
			}
			return true
		},
	}
	request, err := http.NewRequest(http.MethodPost, "http://example.test/v1/messages", strings.NewReader(`{"model":"claude-fable-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer original-token")
	response, err := transport.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusOK || attempts != 2 {
		t.Fatalf("response = %#v, err = %v, attempts = %d", response, err, attempts)
	}
}

func TestAutonomousAgentRetryPreservesRotatedAccountAfterReset(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	budget := newAttemptBudget(1)
	var auths []string
	pool := autonomousRetryRoundTripper(func(request *http.Request) (*http.Response, error) {
		auth := request.Header.Get("Authorization")
		auths = append(auths, auth)
		switch len(auths) {
		case 1:
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Anthropic-Ratelimit-Unified-Status": []string{"rejected"}},
				Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error"}}`)),
			}, nil
		case 2:
			if !strings.Contains(auth, "tok-fresh") {
				t.Fatalf("reset attempt auth = %q, want rotated account", auth)
			}
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
		default:
			if !strings.Contains(auth, "tok-fresh") {
				t.Fatalf("recovery attempt auth = %q, want rotated account", auth)
			}
			if selected, ok := attemptAccount(request.Context()); !ok || selected.ID != "fresh@example.com" {
				t.Fatalf("recovery attempt account = %+v, %t", selected, ok)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
		}
	})
	inner := usageLimitRetryTransport{
		base: pool, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-reset", account: "cooked@example.com",
		accountCredential: server.Accounts[0].CredentialIdentity(), method: http.MethodPost,
		path: "/v1/messages", maxAttempts: 2, budget: budget,
	}
	transport := autonomousAgentRetryTransport{
		base: inner, provider: accounts.ProviderClaude, agent: "claude", session: "session-reset",
		account: "cooked@example.com", budget: budget, retriesPerPass: 1,
		sleep: func(context.Context, time.Duration) bool { return true },
	}
	request, err := http.NewRequest(http.MethodPost, "http://example.test/v1/messages", strings.NewReader(`{"model":"claude-fable-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := transport.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	if got := strings.Join(auths, ","); got != "Bearer tok-cooked,Bearer tok-fresh,Bearer tok-fresh" {
		t.Fatalf("account attempts = %s", got)
	}
}

func TestAutonomousAgentRetryReplenishesBoundedPoolPass(t *testing.T) {
	budget := newAttemptBudget(1)
	attempts := 0
	transport := autonomousAgentRetryTransport{
		provider: accounts.ProviderCodex, account: "account-a", budget: budget, retriesPerPass: 1,
		base: autonomousRetryRoundTripper(func(request *http.Request) (*http.Response, error) {
			attempts++
			if !budget.consume() {
				t.Fatalf("pool pass %d did not receive its fresh bounded allowance", attempts)
			}
			if budget.consume() {
				t.Fatalf("pool pass %d allowance multiplied inside one pass", attempts)
			}
			if attempts == 1 {
				rotated := request.Clone(withAttemptAccount(request.Context(), accounts.Account{ID: "account-b", Provider: accounts.ProviderCodex}))
				rotated.Header = request.Header.Clone()
				rotated.Header.Set("Authorization", "Bearer account-b")
				response := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: http.NoBody, Request: rotated}
				return tagRoutedResponseAccount(response, accounts.Account{ID: "account-b", Provider: accounts.ProviderCodex}), nil
			}
			if request.Header.Get("Authorization") != "Bearer account-b" {
				t.Fatalf("second pool pass lost last route: %q", request.Header.Get("Authorization"))
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
		}),
		sleep: func(context.Context, time.Duration) bool { return true },
	}
	request, err := http.NewRequest(http.MethodPost, "http://example.test/v1/responses", strings.NewReader("request"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer account-a")
	response, err := transport.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusOK || attempts != 2 {
		t.Fatalf("response = %#v, err = %v, attempts = %d", response, err, attempts)
	}
}

func TestReplayableTransportPreservesRoutedResponseOnStreamReset(t *testing.T) {
	attempts := 0
	base := autonomousRetryRoundTripper(func(request *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			rotated := request.Clone(withAttemptAccount(request.Context(), accounts.Account{ID: "account-b", Provider: accounts.ProviderCodex}))
			rotated.Header = request.Header.Clone()
			rotated.Header.Set("Authorization", "Bearer account-b")
			response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: rotated}
			return tagRoutedResponseAccount(response, accounts.Account{ID: "account-b", Provider: accounts.ProviderCodex}), io.ErrUnexpectedEOF
		}
		if request.Header.Get("Authorization") != "Bearer account-b" {
			t.Fatalf("stream-reset retry authorization = %q", request.Header.Get("Authorization"))
		}
		if selected, ok := attemptAccount(request.Context()); !ok || selected.ID != "account-b" {
			t.Fatalf("stream-reset retry account = %+v, %t", selected, ok)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody}, nil
	})
	transport := replayablePostRetryTransport{
		base: base, maxAttempts: 2, budget: newAttemptBudget(1),
		sleep: func(context.Context, time.Duration) error { return nil },
	}
	request, err := http.NewRequest(http.MethodPost, "http://example.test/v1/responses", strings.NewReader("request"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer account-a")
	response, err := transport.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusNoContent || attempts != 2 {
		t.Fatalf("response = %#v, err = %v, attempts = %d", response, err, attempts)
	}
}

func TestRetryablePostTransportErrorUnwrapsNetworkFailures(t *testing.T) {
	timeout := &net.DNSError{Err: "timeout", Name: "upstream.example", IsTimeout: true}
	for name, test := range map[string]struct {
		err  error
		want bool
	}{
		"eof":                 {err: io.EOF, want: true},
		"wrapped eof":         {err: fmt.Errorf("read response: %w", io.EOF), want: true},
		"connection reset":    {err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, want: true},
		"connection refused":  {err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, want: true},
		"network timeout":     {err: timeout, want: true},
		"caller cancellation": {err: fmt.Errorf("round trip: %w", context.Canceled), want: false},
		"terminal error":      {err: errors.New("invalid URL"), want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := retryablePostTransportError(test.err); got != test.want {
				t.Fatalf("retryablePostTransportError(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestAutonomousAgentRetryRecoversFromAbruptHTTPUpstream(t *testing.T) {
	attempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server cannot hijack connection")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	transport := autonomousAgentRetryTransport{
		base:  upstream.Client().Transport,
		sleep: func(context.Context, time.Duration) bool { return true },
	}
	request, err := http.NewRequest(http.MethodPost, upstream.URL, strings.NewReader("request"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusNoContent || attempts != 2 {
		t.Fatalf("response = %#v, err = %v, attempts = %d", response, err, attempts)
	}
}

func TestAutonomousAgentRetryTransportStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	transport := autonomousAgentRetryTransport{
		base: autonomousRetryRoundTripper(func(*http.Request) (*http.Response, error) {
			attempts++
			return &http.Response{StatusCode: http.StatusBadGateway, Body: http.NoBody}, nil
		}),
		sleep: func(context.Context, time.Duration) bool {
			cancel()
			return false
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.test/v1/messages", strings.NewReader("request"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
	if response == nil || response.StatusCode != http.StatusBadGateway || attempts != 1 {
		t.Fatalf("response = %#v, attempts = %d", response, attempts)
	}
}

func TestAutonomousAgentRetryTransportPassesNonTransientResponse(t *testing.T) {
	transport := autonomousAgentRetryTransport{base: autonomousRetryRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadRequest, Body: http.NoBody}, nil
	})}
	request, err := http.NewRequest(http.MethodPost, "http://example.test/v1/messages", strings.NewReader("request"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("response = %#v, err %v", response, err)
	}
}

func TestAutonomousAgentRetryTransportDoesNotRetryTerminalQuota429(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider accounts.Provider
		header   http.Header
		body     string
	}{
		{
			name: "claude rejected", provider: accounts.ProviderClaude,
			header: http.Header{"Anthropic-Ratelimit-Unified-Status": []string{"rejected"}},
			body:   `{"type":"error","error":{"type":"rate_limit_error"}}`,
		},
		{
			name: "codex quota", provider: accounts.ProviderCodex,
			header: http.Header{"Content-Type": []string{"application/json"}},
			body:   `{"error":{"type":"usage_limit_reached","message":"quota"}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			transport := autonomousAgentRetryTransport{
				provider: test.provider,
				base: autonomousRetryRoundTripper(func(*http.Request) (*http.Response, error) {
					attempts++
					return &http.Response{StatusCode: http.StatusTooManyRequests, Header: test.header.Clone(), Body: io.NopCloser(strings.NewReader(test.body))}, nil
				}),
				sleep: func(context.Context, time.Duration) bool {
					t.Fatal("terminal quota response was retried")
					return false
				},
			}
			request, err := http.NewRequest(http.MethodPost, "http://example.test/v1/responses", strings.NewReader("request"))
			if err != nil {
				t.Fatal(err)
			}
			response, err := transport.RoundTrip(request)
			if err != nil || response.StatusCode != http.StatusTooManyRequests || attempts != 1 {
				t.Fatalf("response = %#v, err = %v, attempts = %d", response, err, attempts)
			}
		})
	}
}

func TestAutonomousAgentRetryTransportRetriesSafeFirstEventFailures(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider accounts.Provider
		reason   string
		failed   func() *http.Response
	}{
		{
			name: "codex", provider: accounts.ProviderCodex, reason: "pool_stream_failed",
			failed: func() *http.Response {
				recorder := httptest.NewRecorder()
				codexEgressWriteOverloaded(recorder)
				return recorder.Result()
			},
		},
		{
			name: "claude", provider: accounts.ProviderClaude, reason: "stream_overloaded",
			failed: func() *http.Response {
				return claudeSSEResponse(claudeSSEMessageStart + claudeSSEOverloadedEvent)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{retryStatuses: newRetryStatusRegistry()}
			attempts := 0
			transport := autonomousAgentRetryTransport{
				server: server, provider: test.provider, agent: test.name, session: "safe-stream",
				base: autonomousRetryRoundTripper(func(*http.Request) (*http.Response, error) {
					attempts++
					if attempts == 1 {
						return test.failed(), nil
					}
					return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody}, nil
				}),
				sleep: func(context.Context, time.Duration) bool {
					status := server.retryStatuses.forSession(test.name, "safe-stream")
					if status == nil || status.Reason != test.reason || status.Attempt != 2 {
						t.Fatalf("retry status = %+v, want reason %q attempt 2", status, test.reason)
					}
					return true
				},
			}
			request, err := http.NewRequest(http.MethodPost, "http://example.test/v1/responses", strings.NewReader("request"))
			if err != nil {
				t.Fatal(err)
			}
			response, err := transport.RoundTrip(request)
			if err != nil || response.StatusCode != http.StatusNoContent || attempts != 2 {
				t.Fatalf("response = %#v, err = %v, attempts = %d", response, err, attempts)
			}
		})
	}
}

func TestAutonomousAgentRetryTransportDoesNotReplayPartialStream(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider accounts.Provider
		stream   string
	}{
		{
			name: "codex", provider: accounts.ProviderCodex,
			stream: "event: response.output_text.delta\ndata: {\"delta\":\"partial\"}\n\n" +
				"event: response.failed\ndata: {\"error\":{\"code\":\"server_error\"}}\n\n",
		},
		{
			name: "claude", provider: accounts.ProviderClaude,
			stream: claudeSSEMessageStart + claudeSSEContent + claudeSSEOverloadedEvent,
		},
		{
			name: "codex malformed event before failure", provider: accounts.ProviderCodex,
			stream: "event: response.future\ndata: {not-json}\n\n" +
				"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\"}}}\n\n",
		},
		{
			name: "codex unknown event before failure", provider: accounts.ProviderCodex,
			stream: "event: response.future\ndata: {\"type\":\"response.future\",\"value\":\"possibly visible\"}\n\n" +
				"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\"}}}\n\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			transport := autonomousAgentRetryTransport{
				provider: test.provider,
				base: autonomousRetryRoundTripper(func(*http.Request) (*http.Response, error) {
					attempts++
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       io.NopCloser(strings.NewReader(test.stream)),
					}, nil
				}),
			}
			request, err := http.NewRequest(http.MethodPost, "http://example.test/v1/responses", strings.NewReader("request"))
			if err != nil {
				t.Fatal(err)
			}
			response, err := transport.RoundTrip(request)
			if err != nil || response.StatusCode != http.StatusOK || attempts != 1 {
				t.Fatalf("response = %#v, err = %v, attempts = %d", response, err, attempts)
			}
			body, readErr := io.ReadAll(response.Body)
			if readErr != nil || string(body) != test.stream {
				t.Fatalf("preserved body = %q, err = %v", body, readErr)
			}
		})
	}
}

func TestAutonomousAgentRetryTransportPreservesAmbiguousCodexStreamError(t *testing.T) {
	const stream = "event: response.future\ndata: {not-json}"
	attempts := 0
	transport := autonomousAgentRetryTransport{
		provider: accounts.ProviderCodex,
		base: autonomousRetryRoundTripper(func(*http.Request) (*http.Response, error) {
			attempts++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &oneReadErrorBody{data: []byte(stream), err: io.ErrUnexpectedEOF},
			}, nil
		}),
	}
	request, err := http.NewRequest(http.MethodPost, "http://example.test/v1/responses", strings.NewReader("request"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil || attempts != 1 {
		t.Fatalf("response = %#v, err = %v, attempts = %d", response, err, attempts)
	}
	body, readErr := io.ReadAll(response.Body)
	if string(body) != stream || !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("preserved body = %q, err = %v; want exact body and unexpected EOF", body, readErr)
	}
}

func TestAutonomousAgentRetryTransportPreservesUnknownDataLessCodexFrameAndReset(t *testing.T) {
	const stream = "event: response.future\n\n"
	attempts := 0
	transport := autonomousAgentRetryTransport{
		provider: accounts.ProviderCodex,
		base: autonomousRetryRoundTripper(func(*http.Request) (*http.Response, error) {
			attempts++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &oneReadErrorBody{data: []byte(stream), err: io.ErrUnexpectedEOF},
			}, nil
		}),
	}
	request, err := http.NewRequest(http.MethodPost, "http://example.test/v1/responses", strings.NewReader("request"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil || attempts != 1 {
		t.Fatalf("response = %#v, err = %v, attempts = %d", response, err, attempts)
	}
	body, readErr := io.ReadAll(response.Body)
	if string(body) != stream || !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("preserved body = %q, err = %v; want exact frame and unexpected EOF", body, readErr)
	}
}

func TestAutonomousAgentRetryTransportPreservesUnknownCodexEventNameDespiteSafePayload(t *testing.T) {
	const stream = "event: response.future\ndata: {\"type\":\"response.created\"}\n\n"
	attempts := 0
	transport := autonomousAgentRetryTransport{
		provider: accounts.ProviderCodex,
		base: autonomousRetryRoundTripper(func(*http.Request) (*http.Response, error) {
			attempts++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &oneReadErrorBody{data: []byte(stream), err: io.ErrUnexpectedEOF},
			}, nil
		}),
	}
	request, err := http.NewRequest(http.MethodPost, "http://example.test/v1/responses", strings.NewReader("request"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil || attempts != 1 {
		t.Fatalf("response = %#v, err = %v, attempts = %d", response, err, attempts)
	}
	body, readErr := io.ReadAll(response.Body)
	if string(body) != stream || !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("preserved body = %q, err = %v; want exact frame and unexpected EOF", body, readErr)
	}
}

func TestAutonomousCodexStackKeepsRotatedAccountAfterNilReset(t *testing.T) {
	server := Server{
		Accounts: []accounts.Account{
			{ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-a"},
			{ID: "codex-b", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-b"},
		},
		SchedulerRef:          selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		CodexOverloadFailover: &CodexOverloadFailoverConfig{stayRetryLimit: 1},
		retryStatuses:         newRetryStatusRegistry(),
	}
	budget := newAttemptBudget(1)
	var auths []string
	pool := autonomousRetryRoundTripper(func(request *http.Request) (*http.Response, error) {
		auth := request.Header.Get("Authorization")
		auths = append(auths, auth)
		switch len(auths) {
		case 1:
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"usage_limit_reached","message":"quota"}}`)),
			}, nil
		case 2:
			if auth != "Bearer tok-b" {
				t.Fatalf("reset attempt auth = %q, want B", auth)
			}
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
		default:
			if auth != "Bearer tok-b" {
				t.Fatalf("recovery attempt auth = %q, want B", auth)
			}
			if selected, ok := attemptAccount(request.Context()); !ok || selected.ID != "codex-b" {
				t.Fatalf("recovery attempt account = %+v, %t", selected, ok)
			}
			return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody}, nil
		}
	})
	usage := usageLimitRetryTransport{
		base: pool, server: &server, provider: accounts.ProviderCodex,
		agent: "codex", session: "codex-reset", account: "codex-a", accountCredential: "tok-a",
		method: http.MethodPost, path: "/responses", maxAttempts: 2, poolModel: "gpt-6-astra", budget: budget,
	}
	replayable := replayablePostRetryTransport{
		base: usage, server: &server, provider: accounts.ProviderCodex,
		agent: "codex", session: "codex-reset", account: "codex-a", maxAttempts: 1, budget: budget,
	}
	overload := codexOverloadFailoverTransport{
		base: replayable, server: &server, agent: "codex", session: "codex-reset",
		account: "codex-a", poolModel: "gpt-6-astra", budget: budget,
		sleep: func(context.Context, time.Duration) bool { return true },
	}
	transport := autonomousAgentRetryTransport{
		base: overload, server: &server, provider: accounts.ProviderCodex,
		agent: "codex", session: "codex-reset", account: "codex-a", budget: budget, retriesPerPass: 1,
		sleep: func(context.Context, time.Duration) bool {
			t.Fatal("Codex overload layer did not recover inside the composed pool pass")
			return false
		},
	}
	request, err := http.NewRequest(http.MethodPost, "https://chatgpt.example/backend-api/codex/responses", strings.NewReader(`{"model":"gpt-6-astra","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer tok-a")
	response, err := transport.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusNoContent {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	if got := strings.Join(auths, ","); got != "Bearer tok-a,Bearer tok-b,Bearer tok-b" {
		t.Fatalf("account attempts = %s", got)
	}
}

func TestAutonomousPolicyKeepsSameProviderAccountRotation(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-rotate", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var auths []string
	pool := autonomousRetryRoundTripper(func(request *http.Request) (*http.Response, error) {
		auths = append(auths, request.Header.Get("Authorization"))
		if strings.Contains(request.Header.Get("Authorization"), "tok-cooked") {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Anthropic-Ratelimit-Unified-Status": []string{"rejected"}},
				Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error"}}`)),
			}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
	})
	inner := usageLimitRetryTransport{
		base: pool, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-rotate", account: "cooked@example.com",
		accountCredential: server.Accounts[0].CredentialIdentity(), method: http.MethodPost,
		path: "/v1/messages", maxAttempts: 4, budget: newAttemptBudget(3),
	}
	transport := autonomousAgentRetryTransport{
		base: inner, provider: accounts.ProviderClaude, agent: "claude", session: "session-rotate",
	}
	request, err := http.NewRequest(http.MethodPost, "http://example.test/v1/messages", strings.NewReader(`{"model":"claude-fable-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := transport.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	if len(auths) != 2 || !strings.Contains(auths[0], "tok-cooked") || !strings.Contains(auths[1], "tok-fresh") {
		t.Fatalf("account attempts = %v, want same-provider cooked then fresh", auths)
	}
}

func TestAutonomousRetryBackoffCaps(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second}
	for i, expected := range want {
		if got := autonomousRetryBackoff(i + 1); got != expected {
			t.Fatalf("attempt %d backoff = %s, want %s", i+1, got, expected)
		}
	}
}
