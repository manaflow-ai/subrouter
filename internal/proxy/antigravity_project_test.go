package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestRewriteAntigravityProjectOnlyTopLevel(t *testing.T) {
	body := []byte(`{"project":"project-a","request":{"project":"nested-a","contents":[]}}`)
	rewritten, changed, err := rewriteAntigravityProject(body, "project-b")
	if err != nil || !changed {
		t.Fatalf("rewrite = changed=%v err=%v", changed, err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(rewritten, &envelope); err != nil {
		t.Fatal(err)
	}
	var project, nested string
	_ = json.Unmarshal(envelope["project"], &project)
	var request map[string]json.RawMessage
	_ = json.Unmarshal(envelope["request"], &request)
	_ = json.Unmarshal(request["project"], &nested)
	if project != "project-b" || nested != "nested-a" {
		t.Fatalf("project=%q nested=%q", project, nested)
	}
}

func TestAntigravityFailoverRewritesReplacementProject(t *testing.T) {
	seen := make([]string, 0, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":{"id":"project-b"}}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, string(body))
		if r.Header.Get("Authorization") == "Bearer token-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	a := accounts.Account{ID: "agy:a", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth, Token: "token-a"}
	b := accounts.Account{ID: "agy:b", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth, Token: "token-b"}
	if _, err := sessions.Put("antigravity", "s", a.ID, ""); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{a, b}, nil)
	s := Server{AccountRef: ref, Sessions: sessions, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)), AntigravityUpstream: mustParseURL(t, upstream.URL)}
	r := httptest.NewRequest(http.MethodPost, "/antigravity/v1internal:streamGenerateContent", strings.NewReader(`{"project":"project-a","request":{"contents":[]}}`))
	r.Header.Set("X-Subrouter-Agent", "antigravity")
	r.Header.Set("X-Subrouter-Session", "s")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || len(seen) != 2 {
		t.Fatalf("status=%d requests=%d body=%s", w.Code, len(seen), w.Body.String())
	}
	if !strings.Contains(seen[1], `"project":"project-b"`) {
		t.Fatalf("replacement project not rewritten: %s", seen[1])
	}
}

func TestAntigravityInitialAttemptRewritesSelectedAccountProject(t *testing.T) {
	var generationBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":{"id":"project-selected"}}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		generationBody = string(body)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	req := httptest.NewRequest(http.MethodPost, upstream.URL+"/antigravity/v1internal:generateContent", strings.NewReader(`{"project":"project-local","request":{"contents":[]}}`))
	// Exercise clients that send a one-shot body without net/http's replay hook.
	req.Header.Set("Authorization", "Bearer token-selected")
	transport := usageLimitRetryTransport{
		base:     http.DefaultTransport,
		server:   &Server{AntigravityUpstream: mustParseURL(t, upstream.URL)},
		provider: accounts.ProviderAntigravity,
		account:  "agy:selected",
		path:     req.URL.Path,
	}
	response, err := transport.RoundTrip(req)
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := "<nil>"
		if response != nil {
			status = response.Status
		}
		t.Fatalf("response=%s err=%v", status, err)
	}
	if !strings.Contains(generationBody, `"project":"project-selected"`) {
		t.Fatalf("initial generation used unbound project: %s", generationBody)
	}
}

func TestAntigravityProjectDiscoveryIsBoundToAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:loadCodeAssist" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		project := "project-a"
		if r.Header.Get("Authorization") == "Bearer token-b" {
			project = "project-b"
		}
		_, _ = io.WriteString(w, `{"cloudaicompanionProject":{"id":"`+project+`"}}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	s := &Server{}
	a := accounts.Account{ID: "agy:a", Token: "token-a"}
	b := accounts.Account{ID: "agy:b", Token: "token-b"}
	pa, err := s.antigravityProject(context.Background(), a, base)
	if err != nil || pa != "project-a" {
		t.Fatalf("a project=%q err=%v", pa, err)
	}
	pb, err := s.antigravityProject(context.Background(), b, base)
	if err != nil || pb != "project-b" {
		t.Fatalf("b project=%q err=%v", pb, err)
	}
	if strings.Contains(pa, pb) {
		t.Fatalf("account projects unexpectedly shared: %q %q", pa, pb)
	}
}

func TestAntigravityProjectDiscoveryAllowsMissingProject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"allowedTiers":[{"id":"standard-tier"}]}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	project, err := (&Server{}).antigravityProject(context.Background(), accounts.Account{ID: "agy:free", Token: "token"}, base)
	if err != nil || project != "" {
		t.Fatalf("project=%q err=%v, want an empty optional project without error", project, err)
	}
}

func TestRewriteAntigravityProjectFailsClosedOnMalformedOrInvalidEnvelope(t *testing.T) {
	for _, body := range [][]byte{[]byte("not-json"), []byte(`{"project":7}`)} {
		if _, _, err := rewriteAntigravityProject(body, "project-b"); err == nil {
			t.Fatalf("body %s did not fail", body)
		}
	}
	if body, changed, err := rewriteAntigravityProject([]byte(`{"request":{}}`), "project-b"); err != nil || changed || string(body) != `{"request":{}}` {
		t.Fatalf("missing project = %s changed=%v err=%v", body, changed, err)
	}
}
