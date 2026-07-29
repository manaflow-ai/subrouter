package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// Codex's web-search POST carries a query, not a mutation, so a transport-level
// failure is safe to replay. It was missing from the allowlist, so every TLS
// record failure, ephemeral-port exhaustion, and broken pipe on /alpha/search
// reached the client as a 502 while the identical failure on /responses was
// absorbed by the retry transport.
func TestAlphaSearchPostIsRetryable(t *testing.T) {
	post := func(path string) *http.Request {
		return &http.Request{Method: http.MethodPost, URL: &url.URL{Path: path}}
	}

	for _, path := range []string{"/alpha/search", "/v1/alpha/search"} {
		if !retryableUpstreamPostRequest(accounts.ProviderCodex, post(path)) {
			t.Errorf("POST %s is not retryable; a transport blip becomes a user-visible 502", path)
		}
	}
	// The allowlist stays an allowlist: unrelated codex POSTs must not replay.
	for _, path := range []string{"/alpha/search/save", "/codex/analytics-events/events"} {
		if retryableUpstreamPostRequest(accounts.ProviderCodex, post(path)) {
			t.Errorf("POST %s became retryable; only known-safe paths may replay", path)
		}
	}
	if retryableUpstreamPostRequest(accounts.ProviderCodex, &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Path: "/alpha/search"},
	}) {
		t.Error("GET /alpha/search took the replayable-POST path")
	}
}

// End to end: the first upstream attempt dies at the transport layer, exactly
// like the "tls: bad record MAC" and "can't assign requested address" failures
// observed in production. With the path allowlisted the proxy replays the
// buffered body and the client sees the successful second attempt.
func TestAlphaSearchTransportFailureIsRetried(t *testing.T) {
	var attempts atomic.Int32
	var secondBody atomic.Value

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			// Reset the connection so the proxy's transport reports the failure,
			// matching the "connection reset by peer" / "broken pipe" class seen
			// in production rather than a clean upstream error response.
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test server does not support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			_ = conn.Close()
			return
		}
		body, _ := io.ReadAll(r.Body)
		secondBody.Store(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL)
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
		MaxBodyBytes: 1 << 20,
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
	}.Handler()

	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	const query = `{"query":"ephemeral port exhaustion"}`
	response, err := http.Post(proxy.URL+"/alpha/search", "application/json", strings.NewReader(query))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; the transport failure was surfaced instead of retried.\nbody: %s\nlogs:\n%s",
			response.StatusCode, body, logs.String())
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("upstream attempts = %d, want 2", got)
	}
	if got, _ := secondBody.Load().(string); got != query {
		t.Fatalf("replayed body = %q, want %q; the retry must resend the original query", got, query)
	}
}
