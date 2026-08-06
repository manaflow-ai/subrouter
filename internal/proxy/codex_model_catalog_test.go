package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
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
