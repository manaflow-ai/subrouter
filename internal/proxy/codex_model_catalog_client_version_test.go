package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// chatgpt.com/backend-api/codex/models answers 400 "client_version Field
// required" without the query parameter. Codex always sends it, but curl
// monitors and OpenAI SDK clients listing models through sr do not, so sr
// forwarded every one of their polls into that 400.
func TestCodexModelListWithoutClientVersionReachesChatGPTWithOne(t *testing.T) {
	for _, tc := range []struct {
		name        string
		target      string
		wantVersion string
	}{
		{name: "missing", target: "/v1/models", wantVersion: "0.162.0"},
		{name: "blank", target: "/v1/models?client_version=", wantVersion: "0.162.0"},
		{name: "caller value kept", target: "/v1/models?client_version=0.146.1", wantVersion: "0.146.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotVersion string
			codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/models" {
					t.Errorf("Codex upstream path = %q, want /models", request.URL.Path)
				}
				gotVersion = request.URL.Query().Get("client_version")
				w.Header().Set("Content-Type", "application/json")
				if gotVersion == "" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"detail":[{"type":"missing","loc":["query","client_version"],"msg":"Field required"}]}`)
					return
				}
				_, _ = io.WriteString(w, `{"models":[]}`)
			}))
			defer codexUpstream.Close()
			upstreamURL, err := url.Parse(codexUpstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			handler := Server{
				CodexUpstream: upstreamURL,
				Accounts: []accounts.Account{
					{ID: "oauth@example.com", AuthMode: accounts.AuthModeOAuth, Token: "oauth-token", AccountID: "chatgpt-account"},
				},
				Sessions: store,
				Scheduler: selectacct.NewScheduler([]selectacct.Score{
					{AccountID: "oauth@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
				}),
				MaxBodyBytes: 1024,
			}.Handler()

			request := httptest.NewRequest(http.MethodGet, tc.target, nil)
			request.Header.Set("X-Subrouter-Agent", "codex")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
			}
			if gotVersion != tc.wantVersion {
				t.Fatalf("upstream client_version = %q, want %q", gotVersion, tc.wantVersion)
			}
		})
	}
}

// The OpenAI API /v1/models an API-key account reaches takes no
// client_version, so a plain model list stays untouched there.
func TestCodexModelListOnAPIKeyAccountGetsNoClientVersion(t *testing.T) {
	var gotPath, gotQuery string
	apiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		gotQuery = request.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	defer apiUpstream.Close()
	upstreamURL, err := url.Parse(apiUpstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		APIUpstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "apikey:paid", AuthMode: accounts.AuthModeAPIKey, Token: "api-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "apikey:paid", Headroom: 0.9, ShortHeadroom: 0.9},
		}),
		MaxBodyBytes: 1024,
	}.Handler()

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("X-Subrouter-Agent", "codex")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	if gotPath != "/v1/models" || gotQuery != "" {
		t.Fatalf("API upstream request = %q?%q, want /v1/models with no query", gotPath, gotQuery)
	}
}
