package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestClaudeSSEPeekOnlyRetriesUnambiguousPreContentReset(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		prefix    string
		wantRetry bool
	}{
		{name: "no bytes", wantRetry: true},
		{name: "complete lifecycle events", prefix: claudeSSEMessageStart, wantRetry: true},
		{name: "visible content", prefix: claudeSSEMessageStart + claudeSSEContent, wantRetry: false},
		{name: "incomplete lifecycle event", prefix: strings.TrimSuffix(claudeSSEMessageStart, "\n"), wantRetry: false},
		{name: "malformed complete event", prefix: "event: ping\ndata: {not-json}\n\n", wantRetry: false},
		{name: "unknown data-less event", prefix: "event: vendor_notice\n\n", wantRetry: false},
		{name: "data-less content event", prefix: "event: content_block_start\n\n", wantRetry: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			response := claudeSSEResponse("")
			response.Body = &oneReadErrorBody{data: []byte(tc.prefix), err: io.ErrUnexpectedEOF}

			result := claudeStreamPeek(response)
			if result.retryableReset != tc.wantRetry {
				t.Fatalf("retryableReset=%t, want %t", result.retryableReset, tc.wantRetry)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if string(body) != tc.prefix {
				t.Fatalf("body=%q, want %q", body, tc.prefix)
			}
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("read error=%v, want original io.ErrUnexpectedEOF", err)
			}
		})
	}
}

func TestClaudePreContentSSEResetRetriesCurrentAccount(t *testing.T) {
	t.Parallel()
	const (
		model = "claude-opus-4-8"
		auth  = "Bearer same-account-token"
	)
	requestBody := []byte(`{"model":"` + model + `","stream":true,"messages":[]}`)
	var (
		calls       int
		models      []string
		authHeaders []string
	)
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		models = append(models, string(body))
		authHeaders = append(authHeaders, request.Header.Get("Authorization"))
		if calls == 1 {
			response := claudeSSEResponse("")
			response.Body = &oneReadErrorBody{data: []byte(claudeSSEMessageStart), err: io.ErrUnexpectedEOF}
			response.Request = request
			return response, nil
		}
		response := claudeSSEResponse(claudeSSEMessageStart + claudeSSEContent + claudeSSETail)
		response.Request = request
		return response, nil
	})
	var waits []time.Duration
	clock := newFakeOverloadClock()
	registry := newRetryStatusRegistry()
	server := &Server{overloadHeld: newOverloadHeldGauge(), retryStatuses: registry}
	transport := usageLimitRetryTransport{
		base: base, server: server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session", account: "same@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 1,
		poolModel: "claude-opus-4-8", budget: newAttemptBudget(0), overloadPolicy: claudeShortLadder, now: clock.Now,
		sleep: func(ctx context.Context, wait time.Duration) error {
			waits = append(waits, wait)
			status := registry.forSession("claude", "session")
			if status == nil || status.Provider != accounts.ProviderClaude || status.Model != "claude-opus-4-8" ||
				status.AccountID != "same@example.com" || status.Attempt != 2 || status.Reason != "stream_reset" ||
				!status.NextRetryAt.Equal(clock.Now().Add(wait)) {
				t.Fatalf("retry status during stream-reset backoff = %+v", status)
			}
			clock.now = clock.now.Add(wait)
			return ctx.Err()
		},
	}
	request, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", auth)

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("calls=%d waits=%v, want two calls and [1s]", calls, waits)
	}
	if status := registry.forSession("claude", "session"); status != nil {
		t.Fatalf("retry status remained after success: %+v", status)
	}
	if string(got) != claudeSSEMessageStart+claudeSSEContent+claudeSSETail {
		t.Fatalf("client body=%q, want only successful retry", got)
	}
	for i := range models {
		if models[i] != string(requestBody) || authHeaders[i] != auth {
			t.Fatalf("attempt %d body=%q auth=%q, want same model body and auth", i+1, models[i], authHeaders[i])
		}
	}
}

func TestClaudePreContentSSEResetStatusClearsOnCancel(t *testing.T) {
	t.Parallel()
	registry := newRetryStatusRegistry()
	server := &Server{overloadHeld: newOverloadHeldGauge(), retryStatuses: registry}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := 0
	transport := usageLimitRetryTransport{
		base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			response := claudeSSEResponse("")
			response.Body = &oneReadErrorBody{err: io.ErrUnexpectedEOF, closes: &closed}
			response.Request = request
			return response, nil
		}),
		server: server, provider: accounts.ProviderClaude,
		agent: "claude", session: "cancel-session", account: "same@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 1,
		poolModel: "claude-opus-4-8", budget: newAttemptBudget(0), overloadPolicy: claudeShortLadder,
		sleep: func(waitCtx context.Context, _ time.Duration) error {
			status := registry.forSession("claude", "cancel-session")
			if status == nil || status.Reason != "stream_reset" {
				t.Fatalf("retry status during canceled wait = %+v", status)
			}
			cancel()
			return waitCtx.Err()
		},
	}

	response, err := transport.RoundTrip(claudeReplayRequest(t, ctx))
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if status := registry.forSession("claude", "cancel-session"); status != nil {
		t.Fatalf("retry status remained after cancel: %+v", status)
	}
	if closed != 1 {
		t.Fatalf("upstream body closes=%d, want 1", closed)
	}
}

func TestClaudeAmbiguousOrVisibleSSEResetIsNeverRetried(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		prefix string
	}{
		{name: "visible content", prefix: claudeSSEMessageStart + claudeSSEContent},
		{name: "incomplete event", prefix: "event: ping\ndata: {\"type\":\"ping\"}"},
		{name: "malformed event", prefix: "event: ping\ndata: {not-json}\n\n"},
		{name: "unknown data-less event", prefix: "event: vendor_notice\n\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var waits []time.Duration
			server := &Server{overloadHeld: newOverloadHeldGauge()}
			transport := usageLimitRetryTransport{
				base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					calls++
					response := claudeSSEResponse("")
					response.Body = &oneReadErrorBody{data: []byte(tc.prefix), err: io.ErrUnexpectedEOF}
					response.Request = request
					return response, nil
				}),
				server: server, provider: accounts.ProviderClaude,
				agent: "claude", session: "ambiguous", account: "same@example.com",
				method: http.MethodPost, path: "/v1/messages", maxAttempts: 1,
				budget: newAttemptBudget(0), sleep: recordSleep(&waits), overloadPolicy: claudeShortLadder,
			}

			response, err := transport.RoundTrip(claudeReplayRequest(t, context.Background()))
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if string(body) != tc.prefix || !errors.Is(readErr, io.ErrUnexpectedEOF) {
				t.Fatalf("body=%q err=%v, want original prefix and io.ErrUnexpectedEOF", body, readErr)
			}
			if calls != 1 || len(waits) != 0 {
				t.Fatalf("calls=%d waits=%v, want no replay", calls, waits)
			}
		})
	}
}

func TestClaudePreContentSSEResetAfterFailoverStaysOnRoutedAccount(t *testing.T) {
	server, store := claudeFailoverServer(t)
	server.ClaudeUpstream = mustParseURL(t, "https://fresh.example.test/claude-root")
	const (
		initialAccount = "cooked@example.com"
		routedAccount  = "fresh@example.com"
	)
	if _, err := store.Put("claude", "session-sse-reset", initialAccount, ""); err != nil {
		t.Fatal(err)
	}

	var accountsSeen, urlsSeen []string
	routedCalls := 0
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		account := initialAccount
		if strings.Contains(request.Header.Get("Authorization"), "tok-fresh") {
			account = routedAccount
		}
		accountsSeen = append(accountsSeen, account)
		urlsSeen = append(urlsSeen, request.URL.String())
		if account == initialAccount {
			header := http.Header{"Anthropic-Ratelimit-Unified-Status": []string{"rejected"}}
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error"}}`)),
				Request:    request,
			}, nil
		}
		routedCalls++
		if routedCalls == 1 {
			response := claudeSSEResponse("")
			response.Body = &oneReadErrorBody{err: io.ErrUnexpectedEOF}
			response.Request = request
			return response, nil
		}
		response := claudeSSEResponse(claudeSSEMessageStart + claudeSSEContent + claudeSSETail)
		response.Request = request
		return response, nil
	})
	clock := newFakeOverloadClock()
	var waits []time.Duration
	transport := usageLimitRetryTransport{
		base: base, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-sse-reset", account: initialAccount,
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 3,
		budget: newAttemptBudget(5), overloadPolicy: overloadRetryPolicy{unbounded: true}, now: clock.Now,
		sleep: func(ctx context.Context, wait time.Duration) error {
			waits = append(waits, wait)
			clock.now = clock.now.Add(wait)
			return ctx.Err()
		},
	}
	request := claudeReplayRequest(t, context.Background())
	request.URL.Scheme = "https"
	request.URL.Host = "initial.example.test"
	request.URL.Path = "/initial/messages"
	request.URL.RawQuery = "route=a"
	request.Header.Set("Authorization", "Bearer tok-cooked")

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	wantAccounts := []string{initialAccount, routedAccount, routedAccount}
	if strings.Join(accountsSeen, ",") != strings.Join(wantAccounts, ",") {
		t.Fatalf("accounts=%v, want %v", accountsSeen, wantAccounts)
	}
	if urlsSeen[0] != "https://initial.example.test/initial/messages?route=a" ||
		urlsSeen[1] == urlsSeen[0] || urlsSeen[2] != urlsSeen[1] ||
		!strings.HasPrefix(urlsSeen[1], "https://fresh.example.test/claude-root/") {
		t.Fatalf("attempt URLs=%v, want A then the same routed B URL twice", urlsSeen)
	}
	if len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("waits=%v, want [1s]", waits)
	}
	routed, ok := routedResponseAccount(response)
	if !ok || routed.ID != routedAccount {
		t.Fatalf("final response account=%+v, %t; want %s", routed, ok, routedAccount)
	}
}

func TestClaudePreContentSSEResetExhaustionSignalsOuterAutonomousRetry(t *testing.T) {
	server, store := claudeFailoverServer(t)
	server.ClaudeUpstream = mustParseURL(t, "https://fresh.example.test/claude-root")
	const (
		initialAccount = "cooked@example.com"
		routedAccount  = "fresh@example.com"
	)
	if _, err := store.Put("claude", "autonomous", initialAccount, ""); err != nil {
		t.Fatal(err)
	}
	clock := newFakeOverloadClock()
	var (
		accountsSeen        []string
		attemptAccountsSeen []string
		urlsSeen            []string
		resetCloses         []int
		routedCalls         int
	)
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		account := initialAccount
		if strings.Contains(request.Header.Get("Authorization"), "tok-fresh") {
			account = routedAccount
		}
		accountsSeen = append(accountsSeen, account)
		attempted, _ := attemptAccount(request.Context())
		attemptAccountsSeen = append(attemptAccountsSeen, attempted.ID)
		urlsSeen = append(urlsSeen, request.URL.String())
		if account == initialAccount {
			header := http.Header{"Anthropic-Ratelimit-Unified-Status": []string{"rejected"}}
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error"}}`)),
				Request:    request,
			}, nil
		}
		routedCalls++
		if routedCalls <= 2 {
			resetCloses = append(resetCloses, 0)
			response := claudeSSEResponse("")
			response.Body = &oneReadErrorBody{err: io.ErrUnexpectedEOF, closes: &resetCloses[len(resetCloses)-1]}
			response.Request = request
			return response, nil
		}
		response := claudeSSEResponse(claudeSSEMessageStart + claudeSSEContent + claudeSSETail)
		response.Request = request
		return response, nil
	})
	policy := overloadRetryPolicy{interval: time.Second, maxWait: time.Second}
	outerSleeps := 0
	newPass := func(budget *attemptBudget) http.RoundTripper {
		inner := usageLimitRetryTransport{
			base: base, server: &server, provider: accounts.ProviderClaude,
			agent: "claude", session: "autonomous", account: initialAccount,
			method: http.MethodPost, path: "/v1/messages", maxAttempts: 3,
			poolModel: "claude-opus-4-8", budget: budget, overloadPolicy: policy, now: clock.Now,
			sleep: func(ctx context.Context, wait time.Duration) error {
				clock.now = clock.now.Add(wait)
				return ctx.Err()
			},
		}
		return replayablePostRetryTransport{
			base: inner, server: &server, provider: accounts.ProviderClaude, model: "claude-opus-4-8",
			agent: "claude", session: "autonomous", account: initialAccount,
			method: http.MethodPost, path: "/v1/messages", maxAttempts: replayablePostMaxAttempts,
			budget: budget, claudeTransientRetry: true, overloadPolicy: policy, now: clock.Now,
			sleep: func(context.Context, time.Duration) error {
				outerSleeps++
				return nil
			},
		}
	}

	request := claudeReplayRequest(t, context.Background())
	request.URL.Scheme = "https"
	request.URL.Host = "initial.example.test"
	request.URL.Path = "/initial/messages"
	request.URL.RawQuery = "route=a"
	request.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := newPass(newAttemptBudget(5)).RoundTrip(request)
	if response == nil {
		t.Fatal("exhausted pass returned no response metadata")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) || !retryablePostTransportError(err) {
		t.Fatalf("err=%v, want retryable original io.ErrUnexpectedEOF for autonomous wrapper", err)
	}
	if response.Body != http.NoBody || len(resetCloses) != 2 || resetCloses[0] != 1 || resetCloses[1] != 1 {
		t.Fatalf("exhausted bodies: response=%T close counts=%v, want closed bodies and http.NoBody", response.Body, resetCloses)
	}
	routed, ok := routedResponseAccount(response)
	if !ok || routed.ID != routedAccount {
		t.Fatalf("routed account=%+v, %t; autonomous wrapper needs the current account", routed, ok)
	}

	// Rebuild the next pass exactly as the autonomous wrapper does: preserve
	// the routed response request and carry B as the explicit attempt account.
	body, bodyErr := request.GetBody()
	if bodyErr != nil {
		t.Fatal(bodyErr)
	}
	next := response.Request.Clone(withAttemptAccount(request.Context(), routed))
	next.Body = body
	next.GetBody = request.GetBody
	next.ContentLength = request.ContentLength
	response, err = newPass(newAttemptBudget(5)).RoundTrip(next)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	wantAccounts := []string{initialAccount, routedAccount, routedAccount, routedAccount}
	if strings.Join(accountsSeen, ",") != strings.Join(wantAccounts, ",") {
		t.Fatalf("accounts=%v, want %v; outer pass must not return to A", accountsSeen, wantAccounts)
	}
	wantAttemptAccounts := []string{"", routedAccount, routedAccount, routedAccount}
	if strings.Join(attemptAccountsSeen, ",") != strings.Join(wantAttemptAccounts, ",") {
		t.Fatalf("attempt contexts=%v, want %v", attemptAccountsSeen, wantAttemptAccounts)
	}
	if urlsSeen[1] == urlsSeen[0] || urlsSeen[2] != urlsSeen[1] || urlsSeen[3] != urlsSeen[1] {
		t.Fatalf("attempt URLs=%v, want the exhausted and outer passes to retain B", urlsSeen)
	}
	if outerSleeps != 0 {
		t.Fatalf("outer replay sleeps=%d, want exhausted signal passed directly to autonomous layer", outerSleeps)
	}
}

func TestClaudeReplayableTransportPreservesRoutedAttempt(t *testing.T) {
	t.Parallel()
	const routedAccount = "fresh@example.com"
	clock := newFakeOverloadClock()
	registry := newRetryStatusRegistry()
	server := &Server{overloadHeld: newOverloadHeldGauge(), retryStatuses: registry}
	var urls, auths, attemptAccounts []string
	calls := 0
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		urls = append(urls, request.URL.String())
		auths = append(auths, request.Header.Get("Authorization"))
		attempted, _ := attemptAccount(request.Context())
		attemptAccounts = append(attemptAccounts, attempted.ID)
		if calls == 1 {
			routed := accounts.Account{ID: routedAccount, Provider: accounts.ProviderClaude}
			routedRequest := request.Clone(withAttemptAccount(request.Context(), routed))
			routedRequest.URL.Scheme = "https"
			routedRequest.URL.Host = "fresh.example.test"
			routedRequest.URL.Path = "/claude-root/v1/messages"
			routedRequest.Header.Set("Authorization", "Bearer tok-fresh")
			response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: routedRequest}
			return tagRoutedResponseAccount(response, routed), io.ErrUnexpectedEOF
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: request}, nil
	})
	transport := replayablePostRetryTransport{
		base: base, server: server, provider: accounts.ProviderClaude, model: "claude-opus-4-8",
		agent: "claude", session: "outer-routed", account: "cooked@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: replayablePostMaxAttempts,
		budget: newAttemptBudget(5), claudeTransientRetry: true, overloadPolicy: claudeShortLadder, now: clock.Now,
		sleep: func(ctx context.Context, wait time.Duration) error {
			status := registry.forSession("claude", "outer-routed")
			if status == nil || status.AccountID != routedAccount {
				t.Fatalf("outer retry status=%+v, want routed B", status)
			}
			clock.now = clock.now.Add(wait)
			return ctx.Err()
		},
	}
	request := claudeReplayRequest(t, context.Background())
	request.URL.Scheme = "https"
	request.URL.Host = "initial.example.test"
	request.URL.Path = "/initial/messages"
	request.Header.Set("Authorization", "Bearer tok-cooked")

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if calls != 2 || urls[0] != "https://initial.example.test/initial/messages" ||
		urls[1] != "https://fresh.example.test/claude-root/v1/messages" ||
		auths[1] != "Bearer tok-fresh" || attemptAccounts[1] != routedAccount {
		t.Fatalf("calls=%d urls=%v auths=%v attempts=%v, want outer retry to preserve B", calls, urls, auths, attemptAccounts)
	}
}

func TestClaudePreContentSSEResetFromHTTPUpstreamRetries(t *testing.T) {
	t.Parallel()
	const auth = "Bearer integration-account-token"
	requestBody := []byte(`{"model":"claude-sonnet-4-5","stream":true,"messages":[]}`)
	var (
		mu          sync.Mutex
		calls       int
		bodies      []string
		authorizers []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		mu.Lock()
		calls++
		call := calls
		bodies = append(bodies, string(body))
		authorizers = append(authorizers, request.Header.Get("Authorization"))
		mu.Unlock()
		if call == 1 {
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 512\r\nConnection: close\r\n\r\n%s", claudeSSEMessageStart)
			_ = rw.Flush()
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, claudeSSEMessageStart+claudeSSEContent+claudeSSETail)
	}))
	defer upstream.Close()

	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DisableKeepAlives = true
	defer base.CloseIdleConnections()
	server := &Server{overloadHeld: newOverloadHeldGauge()}
	var waits []time.Duration
	transport := usageLimitRetryTransport{
		base: base, server: server, provider: accounts.ProviderClaude,
		agent: "claude", session: "integration", account: "same@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 1,
		budget: newAttemptBudget(0), sleep: recordSleep(&waits), overloadPolicy: claudeShortLadder,
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, upstream.URL, bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", auth)

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 || len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("calls=%d waits=%v, want a real unexpected-EOF retry after 1s", calls, waits)
	}
	if string(got) != claudeSSEMessageStart+claudeSSEContent+claudeSSETail {
		t.Fatalf("client body=%q, want only successful retry", got)
	}
	for i := range bodies {
		if bodies[i] != string(requestBody) || authorizers[i] != auth {
			t.Fatalf("attempt %d body=%q auth=%q, want same model body and auth", i+1, bodies[i], authorizers[i])
		}
	}
}

type oneReadErrorBody struct {
	data   []byte
	err    error
	done   bool
	closes *int
}

func (b *oneReadErrorBody) Read(dst []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	b.done = true
	return copy(dst, b.data), b.err
}

func (b *oneReadErrorBody) Close() error {
	if b.closes != nil {
		*b.closes = *b.closes + 1
	}
	return nil
}
