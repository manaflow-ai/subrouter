package proxy

import (
	"bytes"
	"io"
	"log/slog"
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

// A websocket dial that the upstream rejects used to produce no log line at
// all, so a client reporting "Connection reset without closing handshake" left
// nothing behind to diagnose. The upstream's status and headers identify the
// rejecting layer without logging an arbitrary response body that could reflect
// request credentials.
func TestWebSocketDialFailureIsLoggedWithUpstreamDetail(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "cloudflare")
		w.Header().Set("Cf-Mitigated", "challenge")
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<html>reflected Authorization: Bearer secret-token</html>")
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:       "codex-account",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "oauth-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
	}.Handler()

	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	request, err := http.NewRequest(http.MethodGet, proxy.URL+"/v1/realtime?intent=quicksilver", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)

	got := logs.String()
	if !strings.Contains(got, "websocket upstream dial failed") {
		t.Fatalf("no dial-failure log line emitted; the failure would be invisible.\nlogs:\n%s", got)
	}
	for _, want := range []string{"status=403", "cf_mitigated=challenge", "upstream_server=cloudflare"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q, which is what identifies the rejecting layer.\nlogs:\n%s", want, got)
		}
	}
	if strings.Contains(got, "secret-token") {
		t.Errorf("log contains a credential reflected in the upstream body.\nlogs:\n%s", got)
	}
}

func TestWebSocketDialFailureHelpersHandleNilResponse(t *testing.T) {
	if got := websocketResponseHeader(nil, "Server"); got != "" {
		t.Fatalf("nil response header = %q, want empty", got)
	}
}
