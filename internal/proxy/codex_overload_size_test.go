package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// codexSizedBody is a Codex turn whose conversation is about tokens input
// tokens long by the proxy's 4-bytes-per-token estimate.
func codexSizedBody(sessionID string, tokens int) string {
	return `{"model":"gpt-6-astra","session_id":"` + sessionID + `","input":[{"type":"message","role":"user","content":"` +
		strings.Repeat("abcd", tokens) + `"}]}`
}

func codexSizedPost(t *testing.T, proxyURL, body, encoding string) (int, string) {
	t.Helper()
	var reader io.Reader = strings.NewReader(body)
	if encoding == "zstd" {
		var buf bytes.Buffer
		encoder, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = encoder.Write([]byte(body))
		_ = encoder.Close()
		reader = &buf
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, proxyURL+"/responses", reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	out, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(out)
}

// codexSizeServer is a failover-enabled server whose account 0 sheds the
// first failures requests and whose account 1 always serves.
func codexSizeServer(t *testing.T, failures int, configure func(*CodexOverloadFailoverConfig)) (Server, *httptest.Server, func() []codexCapacityAttempt) {
	t.Helper()
	poolURL, seen := codexCapacityPool(t, func(token string, index int) bool {
		return token == "oauth-token-0" && index < failures
	})
	server := codexOverloadServer(t, poolURL, 2, true)
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
	if configure != nil {
		configure(server.CodexOverloadFailover)
	}
	if _, err := server.Sessions.Put("codex", "session-size", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	t.Cleanup(proxy.Close)
	return server, proxy, seen
}

// A short conversation keeps today's failover: one quick retry on its
// account, then another account, and the session moves with it.
func TestCodexOverloadFailoverSmallConversationSwitches(t *testing.T) {
	server, proxy, seen := codexSizeServer(t, 100, nil)
	status, body := codexSizedPost(t, proxy.URL, codexSizedBody("session-size", 2_000), "")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("status=%d body=%s, want the failover to serve from account 1", status, body)
	}
	if got := strings.Join(tokens(seen()), ","); got != "oauth-token-0,oauth-token-0,oauth-token-1" {
		t.Fatalf("pool saw %s, want account 0 twice then account 1", got)
	}
	if assignment, ok := server.Sessions.Get("codex", "session-size"); !ok || assignment.AccountID != "codex-account-1" {
		t.Fatalf("session = %+v, want it moved to account 1", assignment)
	}
}

// A long conversation stays on its account through capacity failures even
// with the failover on: the same-account ladder waits the shedding out, and
// the account is not marked, so the next turn does not evict it either.
func TestCodexOverloadFailoverLargeConversationStaysOnAccount(t *testing.T) {
	for _, encoding := range []string{"", "zstd"} {
		t.Run("encoding="+encoding, func(t *testing.T) {
			server, proxy, seen := codexSizeServer(t, 4, nil)
			status, body := codexSizedPost(t, proxy.URL, codexSizedBody("session-size", 50_000), encoding)
			if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") {
				t.Fatalf("status=%d body=%s, want the session's own account to serve after the wait", status, body)
			}
			if got := strings.Join(tokens(seen()), ","); got != "oauth-token-0,oauth-token-0,oauth-token-0,oauth-token-0,oauth-token-0" {
				t.Fatalf("pool saw %s, want every attempt on account 0", got)
			}
			if assignment, ok := server.Sessions.Get("codex", "session-size"); !ok || assignment.AccountID != "codex-account-0" {
				t.Fatalf("session = %+v, want it kept on account 0", assignment)
			}
			if _, _, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", ""); ok {
				t.Fatal("a kept conversation marked its account at capacity")
			}
		})
	}
}

// The ladder a kept conversation takes is the failover-off one, bounded by
// its own limits rather than the failover's two-attempt shape.
func TestCodexOverloadFailoverLargeConversationExhaustsSameAccountLadder(t *testing.T) {
	_, proxy, seen := codexSizeServer(t, 1000, nil)
	status, body := codexSizedPost(t, proxy.URL, codexSizedBody("session-size", 50_000), "")
	if status != http.StatusOK || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s, want the capacity failure once the ladder is spent", status, body)
	}
	got := tokens(seen())
	if len(got) != codexTestStayRetries+1 {
		t.Fatalf("pool saw %d attempts, want %d (the same-account ladder)", len(got), codexTestStayRetries+1)
	}
	for _, token := range got {
		if token != "oauth-token-0" {
			t.Fatalf("pool saw %v, want only account 0", got)
		}
	}
}

// SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT=0 is today's behavior: a long
// conversation switches like a short one. A raised cap does too.
func TestCodexOverloadFailoverMaxInputKnob(t *testing.T) {
	for name, configure := range map[string]func(*CodexOverloadFailoverConfig){
		"unlimited": func(c *CodexOverloadFailoverConfig) { c.FailoverMaxInputUnlimited = true },
		"raised":    func(c *CodexOverloadFailoverConfig) { c.FailoverMaxInput = 100_000 },
	} {
		t.Run(name, func(t *testing.T) {
			_, proxy, seen := codexSizeServer(t, 100, configure)
			status, body := codexSizedPost(t, proxy.URL, codexSizedBody("session-size", 50_000), "")
			if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
				t.Fatalf("status=%d body=%s, want the failover to switch", status, body)
			}
			if got := strings.Join(tokens(seen()), ","); got != "oauth-token-0,oauth-token-0,oauth-token-1" {
				t.Fatalf("pool saw %s, want account 0 twice then account 1", got)
			}
		})
	}
}

func TestCodexOverloadFailoverKeepsAccount(t *testing.T) {
	var off *CodexOverloadFailoverConfig
	if off.failoverKeepsAccount(1 << 30) {
		t.Fatal("no config kept an account: there is no failover to skip")
	}
	on := &CodexOverloadFailoverConfig{Enabled: true}
	for estimate, want := range map[int64]bool{0: false, 1_000: false, codexOverloadDefaultFailoverMaxInput: false, codexOverloadDefaultFailoverMaxInput + 1: true} {
		if got := on.failoverKeepsAccount(estimate); got != want {
			t.Errorf("default cap, estimate %d: keeps=%v, want %v", estimate, got, want)
		}
	}
	unlimited := &CodexOverloadFailoverConfig{Enabled: true, FailoverMaxInputUnlimited: true}
	if unlimited.failoverKeepsAccount(1 << 30) {
		t.Fatal("unlimited cap kept an account")
	}
}

// The websocket reroute follows the same cap. The connection learns the
// conversation's size from the input tokens its finished turns report (a
// chained turn's response.create carries only its new input) or from a
// large response.create: past the cap a capacity failure passes through on
// the same account, unmarked, instead of closing 1012.
func TestCodexWebSocketOverloadRerouteRespectsConversationSize(t *testing.T) {
	failed := `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity."}}}`
	completed := func(inputTokens int) string {
		return `{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":` + strconv.Itoa(inputTokens) + `,"output_tokens":5}}}`
	}
	cases := []struct {
		name string
		// firstUsage, when positive, is the input tokens of a first turn
		// that completes; the second turn then fails.
		firstUsage  int
		createBytes int
		wantReroute bool
	}{
		{name: "small conversation reroutes", firstUsage: 1_000, wantReroute: true},
		{name: "reported usage past the cap stays", firstUsage: 200_000, wantReroute: false},
		{name: "large response.create stays", createBytes: 200_000, wantReroute: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
			var mu sync.Mutex
			var connections []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				connections = append(connections, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
				mu.Unlock()
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if tc.firstUsage > 0 {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(completed(tc.firstUsage)))
				}
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(failed))
				_, _, _ = conn.ReadMessage()
			}))
			defer upstream.Close()
			upstreamURL, _ := url.Parse(upstream.URL)
			server := codexEgressServer(t, upstreamURL, nil, 2)
			server.CodexOverloadFailover = &CodexOverloadFailoverConfig{Enabled: true}
			server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
			if _, err := server.Sessions.Put("codex", "ws-size", "codex-account-0", ""); err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(server.Handler())
			defer proxy.Close()
			wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/backend-api/codex/responses"
			conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Session-Id": []string{"ws-size"}})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			create := `{"type":"response.create","model":"gpt-6-astra","input":[]}`
			if tc.firstUsage > 0 {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(create)); err != nil {
					t.Fatal(err)
				}
				if _, body, err := conn.ReadMessage(); err != nil || !strings.Contains(string(body), "response.completed") {
					t.Fatalf("first turn body=%q err=%v", body, err)
				}
			}
			second := create
			if tc.createBytes > 0 {
				second = `{"type":"response.create","model":"gpt-6-astra","input":[{"type":"message","content":"` + strings.Repeat("x", tc.createBytes) + `"}]}`
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(second)); err != nil {
				t.Fatal(err)
			}
			_, body, err := conn.ReadMessage()
			_, _, marked := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "")
			if tc.wantReroute {
				var closeErr *websocket.CloseError
				if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseServiceRestart {
					t.Fatalf("body=%q err=%v, want a 1012 reroute", body, err)
				}
				if !marked {
					t.Fatal("rerouted turn did not mark its account")
				}
				return
			}
			if err != nil || !strings.Contains(string(body), "server_is_overloaded") {
				t.Fatalf("body=%q err=%v, want the capacity failure passed through on the same connection", body, err)
			}
			if marked {
				t.Fatal("a kept conversation marked its account at capacity")
			}
			if assignment, ok := server.Sessions.Get("codex", "ws-size"); !ok || assignment.AccountID != "codex-account-0" {
				t.Fatalf("session = %+v, want it kept on account 0", assignment)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(connections) != 1 {
				t.Fatalf("upstream connections %v, want one", connections)
			}
		})
	}
}

// Capacity marks left on an account by other sessions evict its sticky
// sessions at placement, but not one too large for the failover to move: it
// keeps its account (and its prompt cache) while a small session moves.
func TestCodexCapacityEvictionSkipsLargeStickyConversation(t *testing.T) {
	server, proxy, seen := codexSizeServer(t, 0, nil)
	if _, err := server.Sessions.Put("codex", "session-small", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute)
	for range selectacct.CapacityStickyEvictFailures {
		server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
	}

	status, body := codexSizedPost(t, proxy.URL, codexSizedBody("session-size", 50_000), "")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") {
		t.Fatalf("large: status=%d body=%s, want it kept on account 0", status, body)
	}
	if assignment, ok := server.Sessions.Get("codex", "session-size"); !ok || assignment.AccountID != "codex-account-0" {
		t.Fatalf("large session = %+v, want it kept on account 0", assignment)
	}

	// The large session's success cleared the marks; mark again so the
	// small session meets the same evicting account.
	for range selectacct.CapacityStickyEvictFailures {
		server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
	}
	status, body = codexSizedPost(t, proxy.URL, codexSizedBody("session-small", 2_000), "")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("small: status=%d body=%s, want it moved off the evicting account", status, body)
	}
	if got := strings.Join(tokens(seen()), ","); got != "oauth-token-0,oauth-token-1" {
		t.Fatalf("pool saw %s, want the large turn on account 0 and the small one on account 1", got)
	}
}

// Knob at 0: a large sticky session is evicted by marks like before.
func TestCodexCapacityEvictionUnlimitedMovesLargeConversation(t *testing.T) {
	server, proxy, _ := codexSizeServer(t, 0, func(c *CodexOverloadFailoverConfig) { c.FailoverMaxInputUnlimited = true })
	until := time.Now().Add(time.Minute)
	for range selectacct.CapacityStickyEvictFailures {
		server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
	}
	status, body := codexSizedPost(t, proxy.URL, codexSizedBody("session-size", 50_000), "")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("status=%d body=%s, want the evicting account left", status, body)
	}
}

func TestCodexWebSocketUsageCandidate(t *testing.T) {
	cases := map[string]bool{
		`{"type":"response.completed","response":{"usage":{"input_tokens":12,"output_tokens":3}}}`: true,
		`{"type":"response.done","response":{"usage":{"input_tokens":12}}}`:                        true,
		`{"type":"response.output_text.delta","delta":"hello"}`:                                    false,
		`{"type":"response.completed","response":{"id":"r"}}`:                                      false,
		`{"type":"response.created","response":{"usage":{"input_tokens":0}}}`:                      false,
	}
	for body, want := range cases {
		if got := codexWebSocketUsageCandidate([]byte(body)); got != want {
			t.Errorf("codexWebSocketUsageCandidate(%s) = %v, want %v", body, got, want)
		}
	}
}
