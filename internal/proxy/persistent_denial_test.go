package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func persistentDenialServer(t *testing.T) Server {
	t.Helper()
	server, store := claudeFailoverServer(t)
	server.AccountRef = NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, server.Accounts, nil)
	server.Accounts = nil
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) { return account, nil }
	server.accountListSnapshotContext(context.Background())
	if _, err := store.Put("claude", "persistent-denial", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	return server
}

func persistentDenialTransport(server *Server, stub http.RoundTripper) usageLimitRetryTransport {
	return usageLimitRetryTransport{
		base: stub, server: server, provider: accounts.ProviderClaude,
		agent: "claude", session: "persistent-denial", account: "cooked@example.com",
		accountCredential: "tok-cooked", method: http.MethodPost, path: "/v1/messages", maxAttempts: 3,
	}
}

func TestPersistentAuthorizationDenialQuarantinesCredentialAndSkipsLaterCalls(t *testing.T) {
	server := persistentDenialServer(t)
	var calls []string
	stub := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		auth := req.Header.Get("Authorization")
		calls = append(calls, auth)
		if auth == "Bearer tok-cooked" {
			return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: io.NopCloser(bytes.NewBufferString(`{"type":"error","error":{"type":"permission_error","details":{"error_code":"oauth_not_allowed_for_organization"}}}`)), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewBufferString(`{"ok":true}`)), Request: req}, nil
	})

	first, err := persistentDenialTransport(&server, stub).RoundTrip(newProxyTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if len(calls) != 2 || calls[0] != "Bearer tok-cooked" || calls[1] != "Bearer tok-fresh" {
		t.Fatalf("first failover calls = %v, want cooked then fresh", calls)
	}
	if _, blocked := server.SchedulerRef.ExplicitBlockedUntilFor(accounts.ProviderClaude, "cooked@example.com", "", nowForTest()); !blocked {
		t.Fatal("persistent denial did not block the exact credential")
	}

	second, err := persistentDenialTransport(&server, stub).RoundTrip(newProxyTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if len(calls) != 3 || calls[2] != "Bearer tok-fresh" {
		t.Fatalf("later calls = %v, want no second cooked request", calls)
	}
}

func TestNonPersistentClaudeForbiddenFailsOverWithoutQuarantine(t *testing.T) {
	server := persistentDenialServer(t)
	var calls []string
	stub := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.Header.Get("Authorization"))
		if len(calls) == 1 {
			return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: io.NopCloser(bytes.NewBufferString(`{"type":"error","error":{"type":"permission_error","message":"scope missing"}}`)), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewBufferString(`{"ok":true}`)), Request: req}, nil
	})
	response, err := persistentDenialTransport(&server, stub).RoundTrip(newProxyTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(calls) != 2 || calls[1] != "Bearer tok-fresh" {
		t.Fatalf("calls = %v, want one failover to fresh", calls)
	}
	if _, blocked := server.SchedulerRef.ExplicitBlockedUntilFor(accounts.ProviderClaude, "cooked@example.com", "", nowForTest()); blocked {
		t.Fatal("ordinary Claude 403 must not quarantine the credential")
	}
}

func TestPersistentAuthorizationDenialInSSEFailsOverBeforeClientBytes(t *testing.T) {
	server := persistentDenialServer(t)
	var calls int
	stub := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return claudeSSEResponse("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"permission_error\",\"details\":{\"error_code\":\"oauth_not_allowed_for_organization\"}}}\n\n"), nil
		}
		return claudeSSEResponse(claudeSSEMessageStart + claudeSSEContent + claudeSSETail), nil
	})
	response, err := persistentDenialTransport(&server, stub).RoundTrip(newProxyTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || string(body) != claudeSSEMessageStart+claudeSSEContent+claudeSSETail {
		t.Fatalf("calls=%d body=%q, want one denied stream hidden before healthy retry", calls, string(body))
	}
}

func TestPersistentAuthorizationDenialClearsOnCredentialUpdate(t *testing.T) {
	server := persistentDenialServer(t)
	server.markPersistentAuthorizationDenial(accounts.Account{
		ID: "cooked@example.com", Provider: accounts.ProviderClaude, CredentialVersion: "tok-cooked",
	})
	if _, blocked := server.SchedulerRef.ExplicitBlockedUntilFor(accounts.ProviderClaude, "cooked@example.com", "", nowForTest()); !blocked {
		t.Fatal("denial was not recorded before credential update")
	}
	rotated := accounts.Account{ID: "cooked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-repaired"}
	server.AccountRef.replace(rotated)
	server.accountListSnapshotContext(context.Background())
	if _, blocked := server.SchedulerRef.ExplicitBlockedUntilFor(accounts.ProviderClaude, "cooked@example.com", "", nowForTest()); blocked {
		t.Fatal("fresh credential update did not restore eligibility")
	}
}

func TestPersistentAuthorizationDenialConcurrentCallersDoNotReuseCredential(t *testing.T) {
	server := persistentDenialServer(t)
	server.markPersistentAuthorizationDenial(accounts.Account{
		ID: "cooked@example.com", Provider: accounts.ProviderClaude, CredentialVersion: "tok-cooked",
	})
	var mu sync.Mutex
	var calls []string
	stub := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		calls = append(calls, req.Header.Get("Authorization"))
		mu.Unlock()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewBufferString(`{"ok":true}`)), Request: req}, nil
	})
	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := persistentDenialTransport(&server, stub).RoundTrip(newProxyTestRequest(t))
			if err != nil {
				errs <- err
				return
			}
			response.Body.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != callers {
		t.Fatalf("concurrent upstream calls = %d, want %d", len(calls), callers)
	}
	for _, auth := range calls {
		if auth != "Bearer tok-fresh" {
			t.Fatalf("concurrent caller reused denied credential: %q", auth)
		}
	}
}

func newProxyTestRequest(t *testing.T) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewBufferString(`{"model":"claude-opus-4-8"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewBufferString(`{"model":"claude-opus-4-8"}`)), nil
	}
	request.Header.Set("Authorization", "Bearer tok-cooked")
	return request
}

func nowForTest() time.Time { return time.Now() }
