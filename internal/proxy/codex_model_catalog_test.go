package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestCodexModelCatalogUsesOAuthWithoutChangingResponseAssignment(t *testing.T) {
	const catalog = `{"models":[{"slug":"gpt-5.6-sol","display_name":"GPT-5.6 Sol"}]}`
	var codexHits atomic.Int32
	codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		codexHits.Add(1)
		if request.URL.Path != "/models" {
			t.Errorf("Codex upstream path = %q, want /models", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Errorf("Codex upstream authorization = %q, want OAuth token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, catalog)
	}))
	defer codexUpstream.Close()

	var apiHits atomic.Int32
	apiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-5.6-sol"}]}`)
	}))
	defer apiUpstream.Close()

	parseURL := func(raw string) *url.URL {
		t.Helper()
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "response-session", "apikey:paid", ""); err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: parseURL(codexUpstream.URL),
		APIUpstream:   parseURL(apiUpstream.URL),
		Accounts: []accounts.Account{
			{ID: "oauth@example.com", AuthMode: accounts.AuthModeOAuth, Token: "oauth-token", AccountID: "chatgpt-account"},
			{ID: "apikey:paid", AuthMode: accounts.AuthModeAPIKey, Token: "api-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "oauth@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
			{AccountID: "apikey:paid", Headroom: 0.9, ShortHeadroom: 0.9},
		}),
		MaxBodyBytes: 1024,
	}.Handler()

	request := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.146.1", nil)
	request.Header.Set("X-Subrouter-Agent", "codex")
	request.Header.Set("X-Subrouter-Session", "response-session")
	request.Header.Set("X-Subrouter-Account-ID", "apikey:paid")
	request.Header.Set("X-Subrouter-Model", "gpt-5.6-sol")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	if response.Body.String() != catalog {
		t.Fatalf("catalog body = %s, want %s", response.Body.String(), catalog)
	}
	if got := codexHits.Load(); got != 1 {
		t.Fatalf("Codex upstream hits = %d, want 1", got)
	}
	if got := apiHits.Load(); got != 0 {
		t.Fatalf("API upstream hits = %d, want 0", got)
	}
	assignment, ok := store.Get("codex", "response-session")
	if !ok || assignment.AccountID != "apikey:paid" {
		t.Fatalf("response assignment = %#v, %t; want API-key assignment preserved", assignment, ok)
	}
}

func TestCodexModelCatalogRefreshFailoverStaysOnOAuth(t *testing.T) {
	var codexAuth string
	codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		codexAuth = request.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[]}`)
	}))
	defer codexUpstream.Close()
	var apiHits atomic.Int32
	apiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiHits.Add(1)
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	defer apiUpstream.Close()

	parseURL := func(raw string) *url.URL {
		t.Helper()
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", codexModelCatalogSessionID, "stale@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var refreshed []string
	handler := Server{
		CodexUpstream: parseURL(codexUpstream.URL),
		APIUpstream:   parseURL(apiUpstream.URL),
		Accounts: []accounts.Account{
			{ID: "stale@example.com", AuthMode: accounts.AuthModeOAuth, Token: "stale-token"},
			{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth, Token: "healthy-token"},
			{ID: "apikey:paid", AuthMode: accounts.AuthModeAPIKey, Token: "api-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "stale@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
			{AccountID: "healthy@example.com", Headroom: 0.7, ShortHeadroom: 0.7},
			{AccountID: "apikey:paid", Headroom: 0.99, ShortHeadroom: 0.99},
		}),
		RefreshAccountFn: func(_ context.Context, selected accounts.Account) (accounts.Account, error) {
			refreshed = append(refreshed, selected.ID)
			if selected.ID == "stale@example.com" {
				return selected, &accounts.CodexStoredRefreshFailureError{Failure: accounts.CodexRefreshFailure{
					StatusCode:   http.StatusUnauthorized,
					ProviderCode: "refresh_token_reused",
				}}
			}
			return selected, nil
		},
		MaxBodyBytes: 1024,
	}.Handler()

	request := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.146.1", nil)
	request.Header.Set("X-Subrouter-Agent", "codex")
	request.Header.Set("X-Subrouter-Account-ID", "apikey:paid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	if codexAuth != "Bearer healthy-token" {
		t.Fatalf("Codex upstream authorization = %q, want healthy OAuth token", codexAuth)
	}
	if apiHits.Load() != 0 {
		t.Fatal("catalog refresh failover selected an API-key upstream")
	}
	if len(refreshed) != 2 || refreshed[0] != "stale@example.com" || refreshed[1] != "healthy@example.com" {
		t.Fatalf("refreshed accounts = %v, want stale then healthy OAuth", refreshed)
	}
}

func TestCodexModelCatalogDoesNotFallBackToAPIKeyAfterOAuthRefreshFailure(t *testing.T) {
	codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("terminal OAuth credential reached the Codex upstream")
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer codexUpstream.Close()
	var apiHits atomic.Int32
	apiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiHits.Add(1)
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	defer apiUpstream.Close()

	parseURL := func(raw string) *url.URL {
		t.Helper()
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", codexModelCatalogSessionID, "stale@example.com", ""); err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: parseURL(codexUpstream.URL),
		APIUpstream:   parseURL(apiUpstream.URL),
		Accounts: []accounts.Account{
			{ID: "stale@example.com", AuthMode: accounts.AuthModeOAuth, Token: "stale-token"},
			{ID: "apikey:paid", AuthMode: accounts.AuthModeAPIKey, Token: "api-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "stale@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
			{AccountID: "apikey:paid", Headroom: 0.99, ShortHeadroom: 0.99},
		}),
		RefreshAccountFn: func(_ context.Context, selected accounts.Account) (accounts.Account, error) {
			if selected.ID != "stale@example.com" {
				return selected, nil
			}
			return selected, &accounts.CodexStoredRefreshFailureError{Failure: accounts.CodexRefreshFailure{
				StatusCode:   http.StatusUnauthorized,
				ProviderCode: "refresh_token_reused",
			}}
		},
		MaxBodyBytes: 1024,
	}.Handler()

	request := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.146.1", nil)
	request.Header.Set("X-Subrouter-Agent", "codex")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when no OAuth catalog credential remains; body = %s", response.Code, response.Body.String())
	}
	if apiHits.Load() != 0 {
		t.Fatal("catalog request fell back to the incompatible API-key model list")
	}
}

func TestOpenAIModelListWithoutClientVersionKeepsForcedAPIKey(t *testing.T) {
	const modelList = `{"object":"list","data":[{"id":"gpt-5.6-sol"}]}`
	var codexHits atomic.Int32
	codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		codexHits.Add(1)
		http.Error(w, "unexpected Codex catalog request", http.StatusInternalServerError)
	}))
	defer codexUpstream.Close()

	var apiHits atomic.Int32
	apiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		apiHits.Add(1)
		if got := request.Header.Get("Authorization"); got != "Bearer api-token" {
			t.Errorf("API upstream authorization = %q, want API token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, modelList)
	}))
	defer apiUpstream.Close()

	parseURL := func(raw string) *url.URL {
		t.Helper()
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: parseURL(codexUpstream.URL),
		APIUpstream:   parseURL(apiUpstream.URL),
		Accounts: []accounts.Account{
			{ID: "apikey:paid", AuthMode: accounts.AuthModeAPIKey, Token: "api-token"},
		},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("X-Subrouter-Agent", "codex")
	request.Header.Set("X-Subrouter-Account-ID", "apikey:paid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != modelList {
		t.Fatalf("response = %d %s, want API model list", response.Code, response.Body.String())
	}
	if got := apiHits.Load(); got != 1 {
		t.Fatalf("API upstream hits = %d, want 1", got)
	}
	if got := codexHits.Load(); got != 0 {
		t.Fatalf("Codex upstream hits = %d, want 0", got)
	}
}

func TestCodexModelCatalogBrokerLeaseRequiresOAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer leased-oauth-token" {
			t.Errorf("upstream authorization = %q, want leased OAuth token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[]}`)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	leased := &fakeCredentialBroker{
		lease: broker.Lease{
			ID: "catalog-lease",
			Account: account.Account{
				ID:        "oauth@example.com",
				Provider:  account.ProviderCodex,
				AuthMode:  account.AuthModeOAuth,
				Token:     "leased-oauth-token",
				AccountID: "chatgpt-account",
			},
			ExpiresAt: time.Now().Add(5 * time.Minute),
		},
		leaseInputs: make(chan broker.LeaseRequest, 1),
		reports:     make(chan broker.LeaseOutcome, 1),
	}
	handler := Server{
		CodexUpstream:    upstreamURL,
		APIUpstream:      upstreamURL,
		CredentialBroker: leased,
		RefreshAccountFn: func(context.Context, accounts.Account) (accounts.Account, error) {
			t.Fatal("brokered catalog request attempted a local credential refresh")
			return accounts.Account{}, nil
		},
		MaxBodyBytes: 1024,
	}.Handler()

	request := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.146.1", nil)
	request.Header.Set("X-Subrouter-Agent", "codex")
	request.Header.Set("X-Subrouter-Session", "response-session")
	request.Header.Set("X-Subrouter-Account-ID", "apikey:paid")
	request.Header.Set("X-Subrouter-Model", "gpt-5.6-sol")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	select {
	case input := <-leased.leaseInputs:
		if input.RequiredAuthMode != account.AuthModeOAuth {
			t.Fatalf("required auth mode = %q, want OAuth", input.RequiredAuthMode)
		}
		if input.PreferAccountID != "" {
			t.Fatalf("preferred account = %q, want empty", input.PreferAccountID)
		}
		if input.Model != "" {
			t.Fatalf("lease model = %q, want empty", input.Model)
		}
		if input.SessionID != codexModelCatalogSessionID {
			t.Fatalf("lease session = %q, want %q", input.SessionID, codexModelCatalogSessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("catalog request did not obtain a broker lease")
	}
}
