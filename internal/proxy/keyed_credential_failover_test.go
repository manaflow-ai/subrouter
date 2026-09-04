package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestKeyedCredentialFailureFailsOverToHealthyCredential(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider accounts.Provider
		agent    string
		path     string
		status   int
		body     string
	}{
		{
			name: "generic provider 401", provider: accounts.ProviderOpenRouter,
			agent: "openrouter", path: "/openrouter/chat/completions",
			status: http.StatusUnauthorized, body: `{"error":{"type":"authentication_error"}}`,
		},
		{
			name: "kimi structured credential rejection", provider: accounts.ProviderKimi,
			agent: "kimi", path: "/kimi/v1/messages",
			status: http.StatusForbidden, body: `{"error":{"code":"invalid_api_key","message":"invalid authentication"}}`,
		},
		{
			name: "kimi 401 invalid key", provider: accounts.ProviderKimi,
			agent: "kimi", path: "/kimi/v1/messages",
			status: http.StatusUnauthorized, body: `{"error":{"code":"invalid_api_key","message":"invalid authentication"}}`,
		},
		{
			name: "antigravity generic quota 429", provider: accounts.ProviderAntigravity,
			agent: "antigravity", path: "/antigravity/v1internal:streamGenerateContent",
			status: http.StatusTooManyRequests, body: `{"error":{"status":"RESOURCE_EXHAUSTED"}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			const sessionID = "revoked-key"
			authMode := accounts.AuthModeAPIKey
			if test.provider == accounts.ProviderAntigravity {
				authMode = accounts.AuthModeOAuth
			}
			revoked := accounts.Account{ID: test.agent + ":a-revoked", Provider: test.provider, AuthMode: authMode, Token: "revoked-key"}
			healthy := accounts.Account{ID: test.agent + ":z-healthy", Provider: test.provider, AuthMode: authMode, Token: "healthy-key"}
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				calls++
				switch request.Header.Get("Authorization") {
				case "Bearer revoked-key":
					w.WriteHeader(test.status)
					_, _ = io.WriteString(w, test.body)
				case "Bearer healthy-key":
					_, _ = io.WriteString(w, `{"ok":true}`)
				default:
					t.Errorf("unexpected Authorization %q", request.Header.Get("Authorization"))
					w.WriteHeader(http.StatusUnauthorized)
				}
			}))
			defer upstream.Close()

			sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sessions.Put(test.agent, sessionID, revoked.ID, ""); err != nil {
				t.Fatal(err)
			}
			ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{revoked, healthy}, nil)
			scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
			server := Server{
				AccountRef: ref, Sessions: sessions, SchedulerRef: scheduler,
				MaxBodyBytes: 1024,
			}
			switch test.provider {
			case accounts.ProviderOpenRouter:
				server.OpenRouterUpstream = mustParseURL(t, upstream.URL+"/v1")
			case accounts.ProviderKimi:
				server.KimiUpstream = mustParseURL(t, upstream.URL+"/coding/v1")
			case accounts.ProviderAntigravity:
				server.AntigravityUpstream = mustParseURL(t, upstream.URL)
			}
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(`{"model":"test","messages":[]}`))
			request.Header.Set("X-Subrouter-Agent", test.agent)
			request.Header.Set("X-Subrouter-Session", sessionID)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK || calls != 2 {
				t.Fatalf("status=%d calls=%d body=%s, want healthy second attempt", response.Code, calls, response.Body.String())
			}
			if test.provider == accounts.ProviderAntigravity {
				if scheduler.Get().Exhausted(schedulerAccountProvider(test.provider), revoked.ID) {
					t.Fatal("transient AGY 429 incorrectly exhausted account")
				}
			} else if !scheduler.Get().Exhausted(schedulerAccountProvider(test.provider), revoked.ID) {
				t.Fatal("rejected credential was not excluded")
			}
			if scheduler.Get().Exhausted(schedulerAccountProvider(test.provider), healthy.ID) {
				t.Fatal("healthy credential was excluded")
			}
			assignment, ok := sessions.Get(test.agent, sessionID)
			if !ok || assignment.AccountID != healthy.ID {
				t.Fatalf("sticky assignment = %+v, want healthy credential", assignment)
			}
		})
	}
}

func TestLateKeyedUnauthorizedDoesNotPoisonRotatedCredentialGeneration(t *testing.T) {
	old := accounts.Account{ID: "openrouter:rotating", Provider: accounts.ProviderOpenRouter, AuthMode: accounts.AuthModeAPIKey, Token: "old-key"}
	rotated := old
	rotated.Token = "rotated-key"
	healthy := accounts.Account{ID: "openrouter:healthy", Provider: accounts.ProviderOpenRouter, AuthMode: accounts.AuthModeAPIKey, Token: "healthy-key"}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{old, healthy}, nil)
	scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := Server{AccountRef: ref, SchedulerRef: scheduler}
	server.accountListSnapshotContext(context.Background())

	calls := 0
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			ref.replace(rotated)
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"type":"authentication_error"}}`)), Request: request}, nil
		}
		if request.Header.Get("Authorization") != "Bearer healthy-key" {
			t.Fatalf("retry Authorization = %q, want healthy credential", request.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: request}, nil
	})
	transport := usageLimitRetryTransport{
		base: base, server: &server, provider: accounts.ProviderOpenRouter,
		account: old.ID, accountCredential: old.CredentialIdentity(), maxAttempts: 2,
	}
	request := httptest.NewRequest(http.MethodPost, "https://openrouter.test/chat/completions", strings.NewReader(`{}`))
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{}`)), nil }
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if calls != 2 {
		t.Fatalf("calls = %d, want one failover", calls)
	}
	if scheduler.Get().Exhausted(accounts.ProviderOpenRouter, rotated.ID) {
		t.Fatal("late rejection of old key poisoned the rotated credential")
	}
}

func TestNonReplayableKeyedUnauthorizedMarksCredentialWithoutRetry(t *testing.T) {
	const sessionID = "nonreplayable-revoked-key"
	revoked := accounts.Account{ID: "openrouter:revoked", Provider: accounts.ProviderOpenRouter, AuthMode: accounts.AuthModeAPIKey, Token: "revoked-key"}
	healthy := accounts.Account{ID: "openrouter:healthy", Provider: accounts.ProviderOpenRouter, AuthMode: accounts.AuthModeAPIKey, Token: "healthy-key"}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error"}}`)
	}))
	defer upstream.Close()
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("openrouter", sessionID, revoked.ID, ""); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{revoked, healthy}, nil)
	scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := Server{
		AccountRef: ref, Sessions: sessions, SchedulerRef: scheduler,
		OpenRouterUpstream: mustParseURL(t, upstream.URL+"/v1"), MaxBodyBytes: 1024,
	}
	request := httptest.NewRequest(http.MethodGet, "/openrouter/models", nil)
	request.Header.Set("X-Subrouter-Agent", "openrouter")
	request.Header.Set("X-Subrouter-Session", sessionID)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || calls != 1 {
		t.Fatalf("status=%d calls=%d, non-replayable request must not retry", response.Code, calls)
	}
	if !scheduler.Get().Exhausted(accounts.ProviderOpenRouter, revoked.ID) {
		t.Fatal("non-replayable rejection did not exclude the exact credential")
	}
	assignment, _ := sessions.Get("openrouter", sessionID)
	if assignment.AccountID != revoked.ID {
		t.Fatalf("non-replayable rejection moved sticky assignment: %+v", assignment)
	}
}

func TestNonReplayableKimiPlanCapabilityDoesNotCookCredential(t *testing.T) {
	const sessionID = "nonreplayable-kimi-capability"
	primary := accounts.Account{ID: "kimi:primary", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "primary-key"}
	secondary := accounts.Account{ID: "kimi:secondary", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Token: "secondary-key"}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Your current subscription does not have access to k3. Upgrade to an Moderato plan or above. Upgrade: https://www.kimi.com/membership/pricing?from=server_k3_error"}}`)
	}))
	defer upstream.Close()
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("kimi", sessionID, primary.ID, ""); err != nil {
		t.Fatal(err)
	}
	scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := Server{
		Accounts: []accounts.Account{primary, secondary}, Sessions: sessions, SchedulerRef: scheduler,
		KimiUpstream: mustParseURL(t, upstream.URL+"/coding/v1"), MaxBodyBytes: 1024,
	}
	request := httptest.NewRequest(http.MethodGet, "/kimi/v1/messages", nil)
	request.Header.Set("X-Subrouter-Agent", "kimi")
	request.Header.Set("X-Subrouter-Session", sessionID)
	request.Header.Set("X-Subrouter-Model", "kimi-k3")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || calls != 1 {
		t.Fatalf("status=%d calls=%d, non-replayable request must not retry", response.Code, calls)
	}
	if _, ok := scheduler.ModelIncompatibleUntilFor(accounts.ProviderKimi, primary.ID, "kimi-k3"); !ok {
		t.Fatal("Kimi plan rejection was not recorded for the rejected model")
	}
	if _, ok := scheduler.ExhaustedUntilFor(accounts.ProviderKimi, primary.ID, ""); ok {
		t.Fatal("Kimi plan rejection cooked the credential account-wide")
	}
	assignment, ok := sessions.Get("kimi", sessionID)
	if !ok || assignment.AccountID != secondary.ID {
		t.Fatalf("sticky assignment = %+v, want next model-capable account", assignment)
	}
}

func TestKeyedCredentialFailoverTriesEachCredentialAtMostOnce(t *testing.T) {
	first := accounts.Account{ID: "openrouter:first", Provider: accounts.ProviderOpenRouter, AuthMode: accounts.AuthModeAPIKey, Token: "first-key"}
	second := accounts.Account{ID: "openrouter:second", Provider: accounts.ProviderOpenRouter, AuthMode: accounts.AuthModeAPIKey, Token: "second-key"}
	calls := map[string]int{}
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls[request.Header.Get("Authorization")]++
		return &http.Response{
			StatusCode: http.StatusUnauthorized, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":{"type":"authentication_error"}}`)), Request: request,
		}, nil
	})
	server := Server{
		Accounts:     []accounts.Account{first, second},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
	}
	transport := usageLimitRetryTransport{
		base: base, server: &server, provider: accounts.ProviderOpenRouter,
		account: first.ID, accountCredential: first.CredentialIdentity(), maxAttempts: 6,
	}
	request := httptest.NewRequest(http.MethodPost, "https://openrouter.test/chat/completions", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer first-key")
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{}`)), nil }
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want final credential rejection", response.StatusCode)
	}
	if calls["Bearer first-key"] != 1 || calls["Bearer second-key"] != 1 || len(calls) != 2 {
		t.Fatalf("credential calls = %#v, want each credential exactly once", calls)
	}
}

func TestKeyedCredentialClassifierDoesNotConfuseQuotaOrModelErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "401 authoritative", status: http.StatusUnauthorized, body: `{}`, want: true},
		{name: "structured auth", status: http.StatusForbidden, body: `{"error":{"code":"invalid_api_key"}}`, want: true},
		{name: "model error", status: http.StatusBadRequest, body: `{"error":{"code":"model_not_found"}}`},
		{name: "quota forbidden", status: http.StatusForbidden, body: `{"error":{"message":"You've reached your weekly (7-day) usage limit."}}`},
		{name: "payment required", status: http.StatusPaymentRequired, body: `{"error":{"message":"insufficient balance"}}`},
		{name: "rate limit", status: http.StatusTooManyRequests, body: `{"error":{"type":"rate_limit_error"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: test.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body))}
			got, err := responseKeyedCredentialFailure(response)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("credential failure = %v, want %v", got, test.want)
			}
			preserved, err := io.ReadAll(response.Body)
			if err != nil || string(preserved) != test.body {
				t.Fatalf("body was not preserved: %q, err=%v", preserved, err)
			}
		})
	}
}
