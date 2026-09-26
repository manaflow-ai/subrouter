package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func codexCapacityPostBody(t *testing.T, proxyURL, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, proxyURL+"/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	out, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(out)
}

// A capacity failure is not quota: it leaves no exhaustion mark, and the
// account's first success on the model clears its capacity mark.
func TestCodexCapacityMarkIsNotQuotaAndClearsOnSuccess(t *testing.T) {
	poolURL, _ := codexCapacityPool(t, func(_ string, index int) bool { return index == 0 })
	server := codexOverloadServer(t, poolURL, 2, true)
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
	if _, err := server.Sessions.Put("codex", "session-clear", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-clear", "a", nil)
	if err != nil || status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") {
		t.Fatalf("status=%d body=%s err=%v", status, body, err)
	}
	if _, ok := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra"); ok {
		t.Fatal("capacity failure left a quota exhaustion mark")
	}
	if _, _, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", ""); ok {
		t.Fatal("capacity mark survived the account's success on the same model")
	}
}

// Failures are marked in the request's (model, service tier) pool.
func TestCodexCapacityMarkCarriesServiceTier(t *testing.T) {
	poolURL, _ := codexCapacityPool(t, func(token string, _ int) bool { return token == "oauth-token-0" })
	server := codexOverloadServer(t, poolURL, 2, true)
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
	if _, err := server.Sessions.Put("codex", "session-tier", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexCapacityPostBody(t, proxy.URL, `{"model":"gpt-6-astra","service_tier":"priority","session_id":"session-tier","input":[]}`)
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("status=%d body=%s", status, body)
	}
	_, failures, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "priority")
	if !ok || failures < 2 {
		t.Fatalf("priority-tier mark failures=%d ok=%v, want the repeated failures recorded", failures, ok)
	}
	if _, _, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", ""); ok {
		t.Fatal("priority-tier failure marked the default tier")
	}
}

// A single capacity failure lowers the account's rank for new sessions but
// keeps the sticky session (and its prompt cache) where it is; the same
// account failing again moves the session.
func TestCodexCapacityMarkKeepsStickySessionUntilRepeatedFailure(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(string, int) bool { return false })
	server := codexOverloadServer(t, poolURL, 2, true)
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	if _, err := server.Sessions.Put("codex", "session-sticky", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()
	until := time.Now().Add(time.Minute)
	server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)

	if status, body, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-sticky", "a", nil); err != nil || status != http.StatusOK {
		t.Fatalf("status=%d body=%s err=%v", status, body, err)
	}
	if got := tokens(seen()); got[len(got)-1] != "oauth-token-0" {
		t.Fatalf("sticky session left its account after one capacity failure: %v", got)
	}
	// That success cleared the mark; a brand-new session placed while the
	// account is marked avoids it.
	server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
	for i := range 8 {
		if _, _, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-new-"+string(rune('a'+i)), "b", nil); err != nil {
			t.Fatal(err)
		}
		if got := tokens(seen()); got[len(got)-1] != "oauth-token-1" {
			t.Fatalf("new session placed on the capacity-marked account: %v", got)
		}
	}
	server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
	if status, body, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-sticky", "c", nil); err != nil || status != http.StatusOK {
		t.Fatalf("status=%d body=%s err=%v", status, body, err)
	}
	if got := tokens(seen()); got[len(got)-1] != "oauth-token-1" {
		t.Fatalf("sticky session stayed on an account that kept failing: %v", got)
	}
}

// Over the websocket transport the capacity mark uses the turn's model and
// tier from response.create, is not an account-wide quota mark, and the
// 1012 reconnect lands on the same account first.
func TestCodexWebSocketCapacityMarkKeepsSessionOnAccount(t *testing.T) {
	failed := `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity."}}}`
	var mu sync.Mutex
	var connections []string
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connections = append(connections, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		first := len(connections) == 1
		mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if first {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(failed))
		} else {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r"}}`))
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	server := codexEgressServer(t, upstreamURL, nil, 2)
	server.CodexOverloadFailover = &CodexOverloadFailoverConfig{Enabled: true}
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	if _, err := server.Sessions.Put("codex", "ws-capacity", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()
	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/backend-api/codex/responses"
	create := `{"type":"response.create","model":"gpt-6-astra","service_tier":"priority"}`

	turn := func() (string, error) {
		conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Session-Id": []string{"ws-capacity"}})
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte(create)); err != nil {
			return "", err
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, body, err := conn.ReadMessage()
		return string(body), err
	}
	_, err := turn()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseServiceRestart {
		t.Fatalf("first turn err = %v, want a 1012 reroute", err)
	}
	if _, failures, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "priority"); !ok || failures != 1 {
		t.Fatalf("websocket capacity mark failures=%d ok=%v, want one priority-tier mark", failures, ok)
	}
	if _, ok := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, "codex-account-0", ""); ok {
		t.Fatal("websocket capacity failure left an account-wide exhaustion mark")
	}
	body, err := turn()
	if err != nil || !strings.Contains(body, "response.completed") {
		t.Fatalf("reconnect body=%q err=%v", body, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(connections) != 2 || connections[1] != "oauth-token-0" {
		t.Fatalf("upstream connections %v, want the reconnect on the same account", connections)
	}
	if _, _, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "priority"); ok {
		t.Fatal("websocket success did not clear the capacity mark")
	}
}
