package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// codexWriteDeltaThenFailed is a 2xx stream that shows output and then fails
// the turn. The failure is post-output, so no layer may replay it, and the
// turn never completed, so it must not pin the session either.
func codexWriteDeltaThenFailed(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
	_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"boom\"}}}\n\n")
}

// codexStickyCommitServer is a two-account Codex pool with the session stored
// on the first account. first answers the first account's requests; every
// other account streams a delta then response.failed.
func codexStickyCommitServer(t *testing.T, firstID string, first http.HandlerFunc, failover *CodexOverloadFailoverConfig) (http.Handler, *session.Store) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer first-token" {
			first(w, r)
			return
		}
		codexWriteDeltaThenFailed(w)
	}))
	t.Cleanup(upstream.Close)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-1", firstID, ""); err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: mustParseURL(t, upstream.URL),
		Accounts: []accounts.Account{
			{ID: firstID, AuthMode: accounts.AuthModeOAuth, Token: "first-token"},
			{ID: "compatible@example.com", AuthMode: accounts.AuthModeOAuth, Token: "second-token"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: firstID, Headroom: 0.80, ShortHeadroom: 0.80},
			{AccountID: "compatible@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
		})),
		MaxBodyBytes:          1 << 20,
		CodexOverloadFailover: failover,
	}.Handler()
	return handler, store
}

func codexStickyCommitPost(t *testing.T, handler http.Handler) (int, string) {
	t.Helper()
	proxy := httptest.NewServer(handler)
	defer proxy.Close()
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

// The usage-limit layer below the capacity layer fails a request over for
// quota or model compatibility and commits the move only once the stream
// completes. The capacity layer must not commit that move early on 2xx
// headers: a stream that fails after output would otherwise pin the session
// to the account that failed it.
func TestCodexCapacityLayerDoesNotCommitLowerLayerFailoverEarly(t *testing.T) {
	cases := []struct {
		name    string
		firstID string
		first   http.HandlerFunc
	}{
		{"model compatibility", "incompatible@example.com", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}`)
		}},
		{"quota", "quota@example.com", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"quota"}}`)
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			handler, store := codexStickyCommitServer(t, test.firstID, test.first, nil)
			status, body := codexStickyCommitPost(t, handler)
			if status != http.StatusOK || !strings.Contains(body, "response.failed") {
				t.Fatalf("status=%d body=%s, want the alternate's failed stream", status, body)
			}
			assignment, ok := store.Get("codex", "session-1")
			if !ok || assignment.AccountID != test.firstID {
				t.Fatalf("session assignment = %+v, want it to stay on %s after a stream that failed", assignment, test.firstID)
			}
		})
	}
}

// With the failover on, a switch the capacity layer made itself commits only
// once the new account completes the turn, not on its 2xx headers.
func TestCodexCapacityFailoverCommitsOwnSwitchOnlyOnCompletion(t *testing.T) {
	failover := &CodexOverloadFailoverConfig{Enabled: true}
	fastCapacityGaps(failover, time.Millisecond)
	handler, store := codexStickyCommitServer(t, "overloaded@example.com", func(w http.ResponseWriter, _ *http.Request) {
		codexEgressWriteOverloaded(w)
	}, failover)
	status, body := codexStickyCommitPost(t, handler)
	if status != http.StatusOK || !strings.Contains(body, "partial") {
		t.Fatalf("status=%d body=%s, want the switched account's stream", status, body)
	}
	assignment, ok := store.Get("codex", "session-1")
	if !ok || assignment.AccountID != "overloaded@example.com" {
		t.Fatalf("session assignment = %+v, want it to stay on overloaded@example.com after a stream that failed", assignment)
	}
}
