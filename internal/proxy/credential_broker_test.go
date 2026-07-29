package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
)

type fakeCredentialBroker struct {
	lease       broker.Lease
	leaseErr    error
	leaseInputs chan broker.LeaseRequest
	reports     chan broker.LeaseOutcome
	reportBlock chan struct{}
	invalidated chan string
}

func (b *fakeCredentialBroker) Lease(
	_ context.Context,
	input broker.LeaseRequest,
) (broker.Lease, error) {
	if b.leaseInputs != nil {
		b.leaseInputs <- input
	}
	return b.lease, b.leaseErr
}

func (b *fakeCredentialBroker) Report(
	ctx context.Context,
	_ string,
	outcome broker.LeaseOutcome,
	_ int,
) error {
	if b.reportBlock != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-b.reportBlock:
		}
	}
	b.reports <- outcome
	return nil
}

func (b *fakeCredentialBroker) InvalidateLease(leaseID string) {
	if b.invalidated != nil {
		b.invalidated <- leaseID
	}
}

func TestCredentialBrokerKeepsProviderTrafficOnLocalProxy(t *testing.T) {
	const requestBody = `{"model":"gpt-5","input":"local egress"}`
	upstreamBody := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer leased-access" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "provider-account" {
			t.Errorf("ChatGPT account = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		upstreamBody <- string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	leased := &fakeCredentialBroker{
		lease: broker.Lease{
			ID: "lease-a",
			Account: account.Account{
				ID:        "shared-a",
				Provider:  account.ProviderCodex,
				AuthMode:  account.AuthModeOAuth,
				Token:     "leased-access",
				AccountID: "provider-account",
			},
			ExpiresAt: time.Now().Add(5 * time.Minute),
		},
		leaseInputs: make(chan broker.LeaseRequest, 1),
		reports:     make(chan broker.LeaseOutcome, 1),
	}
	var localRefreshCalls atomic.Int32
	handler := Server{
		CodexUpstream:    upstreamURL,
		APIUpstream:      upstreamURL,
		CredentialBroker: leased,
		RefreshAccountFn: func(
			context.Context,
			accounts.Account,
		) (accounts.Account, error) {
			localRefreshCalls.Add(1)
			return accounts.Account{}, nil
		},
		MaxBodyBytes: 1 << 20,
	}.Handler()

	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:31415/v1/responses",
		bytes.NewBufferString(requestBody),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Subrouter-Agent", "codex")
	request.Header.Set("X-Subrouter-Session", "session-local")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if localRefreshCalls.Load() != 0 {
		t.Fatalf("local refresh called %d times", localRefreshCalls.Load())
	}
	select {
	case got := <-upstreamBody:
		if got != requestBody {
			t.Fatalf("upstream body = %q, want %q", got, requestBody)
		}
	case <-time.After(time.Second):
		t.Fatal("provider upstream did not receive the request")
	}
	select {
	case input := <-leased.leaseInputs:
		if input.SessionID != "session-local" ||
			input.Provider != account.ProviderCodex {
			t.Fatalf("lease input = %+v", input)
		}
	case <-time.After(time.Second):
		t.Fatal("central broker was not asked for an access lease")
	}
	select {
	case outcome := <-leased.reports:
		if outcome != broker.LeaseSuccess {
			t.Fatalf("lease outcome = %q", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("lease outcome was not reported")
	}
}

func TestClaudeLeaseOutcomesHonorUnifiedQuotaHeaders(t *testing.T) {
	for _, test := range []struct {
		name             string
		status           int
		unifiedStatus    string
		wantOutcome      broker.LeaseOutcome
		wantInvalidation bool
	}{
		{
			name:             "rejected 200 is exhausted",
			status:           http.StatusOK,
			unifiedStatus:    "rejected",
			wantOutcome:      broker.LeaseRateLimited,
			wantInvalidation: true,
		},
		{
			name:             "allowed warning 429 rotates within the model pool",
			status:           http.StatusTooManyRequests,
			unifiedStatus:    "allowed_warning",
			wantOutcome:      broker.LeaseRateLimited,
			wantInvalidation: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set(
						"anthropic-ratelimit-unified-status",
						test.unifiedStatus,
					)
					w.WriteHeader(test.status)
				},
			))
			defer upstream.Close()
			upstreamURL, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			leased := &fakeCredentialBroker{
				lease: broker.Lease{
					ID: "lease-claude",
					Account: account.Account{
						ID:       "shared-claude",
						Provider: account.ProviderClaude,
						AuthMode: account.AuthModeOAuth,
						Token:    "leased-access",
					},
					ExpiresAt: time.Now().Add(5 * time.Minute),
				},
				leaseInputs: make(chan broker.LeaseRequest, 1),
				reports:     make(chan broker.LeaseOutcome, 1),
				invalidated: make(chan string, 1),
			}
			handler := Server{
				ClaudeUpstream:   upstreamURL,
				CredentialBroker: leased,
				MaxBodyBytes:     1 << 20,
			}.Handler()
			response := httptest.NewRecorder()
			request := httptest.NewRequest(
				http.MethodPost,
				"http://127.0.0.1:31415/v1/messages",
				bytes.NewBufferString(`{"model":"claude-opus"}`),
			)
			request.Header.Set("X-Subrouter-Agent", "claude")
			handler.ServeHTTP(response, request)

			select {
			case outcome := <-leased.reports:
				if outcome != test.wantOutcome {
					t.Fatalf(
						"outcome = %q, want %q",
						outcome,
						test.wantOutcome,
					)
				}
			case <-time.After(time.Second):
				t.Fatal("lease outcome was not reported")
			}
			select {
			case leaseID := <-leased.invalidated:
				if !test.wantInvalidation {
					t.Fatalf("transient response invalidated %q", leaseID)
				}
			default:
				if test.wantInvalidation {
					t.Fatal("exhausted Claude lease was not invalidated")
				}
			}
		})
	}
}

func TestChatGPTBackendRequestsRequireOAuthLease(t *testing.T) {
	leased := &fakeCredentialBroker{
		leaseErr:    errors.New("stop after lease selection"),
		leaseInputs: make(chan broker.LeaseRequest, 1),
		reports:     make(chan broker.LeaseOutcome, 1),
	}
	handler := Server{
		CredentialBroker: leased,
		MaxBodyBytes:     1 << 20,
	}.Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(
			http.MethodPost,
			"http://127.0.0.1:31415/backend-api/codex/responses",
			bytes.NewBufferString(`{"model":"gpt-5"}`),
		),
	)

	select {
	case input := <-leased.leaseInputs:
		if input.RequiredAuthMode != account.AuthModeOAuth {
			t.Fatalf(
				"required auth mode = %q, want oauth",
				input.RequiredAuthMode,
			)
		}
	case <-time.After(time.Second):
		t.Fatal("central broker was not asked for a credential lease")
	}
}

func TestCloudProxyRequiresTheSignedInUsersLocalToken(t *testing.T) {
	var leaseCalls atomic.Int32
	brokerClient := &fakeCredentialBroker{
		leaseErr:    errors.New("must not be reached"),
		reports:     make(chan broker.LeaseOutcome, 1),
		leaseInputs: make(chan broker.LeaseRequest, 1),
	}
	handler := Server{
		CredentialBroker: brokerClient,
		LocalProxyToken:  "stack-local-token",
		MaxBodyBytes:     1 << 20,
	}.Handler()

	for name, authorization := range map[string]string{
		"missing": "",
		"wrong":   "Bearer another-user",
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodPost,
				"http://127.0.0.1:31415/v1/responses",
				bytes.NewBufferString(`{"model":"gpt-5"}`),
			)
			if authorization != "" {
				request.Header.Set("Authorization", authorization)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.Code)
			}
			select {
			case <-brokerClient.leaseInputs:
				leaseCalls.Add(1)
			default:
			}
		})
	}
	if leaseCalls.Load() != 0 {
		t.Fatalf("unauthorized callers reached broker %d times", leaseCalls.Load())
	}

	authorized := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:31415/v1/responses",
		bytes.NewBufferString(`{"model":"gpt-5"}`),
	)
	authorized.Header.Set("Authorization", "Bearer stack-local-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authorized)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("authorized status = %d, want broker response 503", response.Code)
	}
	select {
	case <-brokerClient.leaseInputs:
	case <-time.After(time.Second):
		t.Fatal("authorized caller did not reach broker")
	}
}

func TestCredentialBrokerWaitsForCentralRefreshAfterUnauthorized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired", http.StatusUnauthorized)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	reportBlock := make(chan struct{})
	leased := &fakeCredentialBroker{
		lease: broker.Lease{
			ID: "lease-expired",
			Account: account.Account{
				ID:       "shared-a",
				Provider: account.ProviderCodex,
				AuthMode: account.AuthModeOAuth,
				Token:    "expired-access",
			},
			ExpiresAt: time.Now().Add(5 * time.Minute),
		},
		leaseInputs: make(chan broker.LeaseRequest, 1),
		reports:     make(chan broker.LeaseOutcome, 1),
		reportBlock: reportBlock,
		invalidated: make(chan string, 1),
	}
	handler := Server{
		CodexUpstream:    upstreamURL,
		APIUpstream:      upstreamURL,
		CredentialBroker: leased,
		MaxBodyBytes:     1 << 20,
	}.Handler()
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(
			response,
			httptest.NewRequest(
				http.MethodPost,
				"http://127.0.0.1:31415/v1/responses",
				bytes.NewBufferString(`{"model":"gpt-5"}`),
			),
		)
	}()

	select {
	case leaseID := <-leased.invalidated:
		if leaseID != "lease-expired" {
			t.Fatalf("invalidated lease = %q", leaseID)
		}
	case <-time.After(time.Second):
		t.Fatal("failed lease was not invalidated")
	}
	select {
	case <-done:
		t.Fatal("proxy returned before central unauthorized handling completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(reportBlock)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("proxy did not return after central handling completed")
	}
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	select {
	case outcome := <-leased.reports:
		if outcome != broker.LeaseUnauthorized {
			t.Fatalf("lease outcome = %q", outcome)
		}
	default:
		t.Fatal("unauthorized outcome was not reported")
	}
}

func TestCredentialBrokerReportsWebSocketUsageLimitBeforeClientReconnect(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade upstream: %v", err)
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read upstream request: %v", err)
			return
		}
		if err := conn.WriteMessage(
			websocket.TextMessage,
			[]byte(`{"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`),
		); err != nil {
			t.Errorf("write upstream quota failure: %v", err)
		}
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	leased := &fakeCredentialBroker{
		lease: broker.Lease{
			ID: "lease-websocket",
			Account: account.Account{
				ID:       "shared-codex",
				Provider: account.ProviderCodex,
				AuthMode: account.AuthModeOAuth,
				Token:    "leased-access",
			},
			ExpiresAt: time.Now().Add(5 * time.Minute),
		},
		reports:     make(chan broker.LeaseOutcome, 2),
		invalidated: make(chan string, 1),
	}
	subrouter := httptest.NewServer(Server{
		Upstream:         upstreamURL,
		CredentialBroker: leased,
		MaxBodyBytes:     1 << 20,
	}.Handler())
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}

	select {
	case outcome := <-leased.reports:
		if outcome != broker.LeaseRateLimited {
			t.Fatalf("first websocket lease outcome = %q, want %q", outcome, broker.LeaseRateLimited)
		}
	case <-time.After(time.Second):
		t.Fatal("websocket quota failure was not reported")
	}
	select {
	case leaseID := <-leased.invalidated:
		if leaseID != "lease-websocket" {
			t.Fatalf("invalidated lease = %q", leaseID)
		}
	case <-time.After(time.Second):
		t.Fatal("websocket quota failure did not invalidate the lease")
	}
}

func TestCredentialLeaseOutcomeTreatsForbiddenAsProviderScopedFailure(t *testing.T) {
	if got := credentialLeaseOutcome(
		accounts.ProviderCodex,
		http.StatusForbidden,
		nil,
	); got != broker.LeaseProviderError {
		t.Fatalf("403 outcome = %q, want %q", got, broker.LeaseProviderError)
	}
}

func TestCredentialBrokerFailureNeverUsesLocalFableSecrets(t *testing.T) {
	leased := &fakeCredentialBroker{
		leaseErr: errors.New("team vault unavailable"),
		reports:  make(chan broker.LeaseOutcome, 1),
	}
	var localFallbackCalls atomic.Int32
	handler := Server{
		CredentialBroker:  leased,
		ClaudeFableAPIKey: "sk-ant-local-fallback",
		Transport: proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
			localFallbackCalls.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewBufferString(`{"ok":true}`)),
			}, nil
		}),
		MaxBodyBytes: 1 << 20,
	}.Handler()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:31415/v1/messages",
		bytes.NewBufferString(`{"model":"claude-fable-5","messages":[]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Subrouter-Agent", "claude")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want fail-closed 503", response.Code)
	}
	if localFallbackCalls.Load() != 0 {
		t.Fatalf("local fallback used %d times in team-vault mode", localFallbackCalls.Load())
	}
}
