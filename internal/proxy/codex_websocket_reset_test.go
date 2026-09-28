package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestAutonomousCodexWebSocketResetBeforeOutputReconnectsSameRoute(t *testing.T) {
	type arrival struct {
		auth string
		body string
	}
	arrivals := make(chan arrival, 2)
	var attempts atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, body, err := conn.ReadMessage()
		if err != nil {
			return
		}
		attempt := attempts.Add(1)
		arrivals <- arrival{auth: r.Header.Get("Authorization"), body: string(body)}
		if attempt == 1 {
			// No close frame: the proxy sees an abnormal boundary read after the
			// complete response.create and before any upstream output.
			_ = conn.UnderlyingConn().Close()
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"model":"gpt-6-astra"}}`))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	server := codexEgressServer(t, upstreamURL, nil, 2)
	server.CodexOverloadFailover.persistGap = func() time.Duration { return 150 * time.Millisecond }
	server.codexOverloadRerouteCounts = newCodexOverloadReroutes()
	server.retryStatuses = newRetryStatusRegistry()
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/backend-api/codex/responses"
	header := http.Header{
		"Session-Id":           []string{"ws-reset-same-route"},
		AgentRetryPolicyHeader: []string{"autonomous"},
	}
	create := `{"type":"response.create","model":"gpt-6-astra","input":"same prompt"}`
	first, response, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err := first.WriteMessage(websocket.TextMessage, []byte(create)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	var firstArrival arrival
	select {
	case firstArrival = <-arrivals:
	case <-time.After(3 * time.Second):
		t.Fatal("first upstream attempt did not arrive")
	}
	wantAccount := strings.Replace(firstArrival.auth, "Bearer oauth-token-", "codex-account-", 1)

	deadline := time.Now().Add(3 * time.Second)
	var status *RetryStatus
	for status == nil && time.Now().Before(deadline) {
		status = server.retryStatuses.forSession("codex", "ws-reset-same-route")
		if status == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if status == nil || status.Provider != accounts.ProviderCodex || status.Model != "gpt-6-astra" ||
		status.AccountID != wantAccount || status.Attempt != 2 ||
		status.Reason != "websocket_reset" || !status.NextRetryAt.After(time.Now()) {
		t.Fatalf("retry status = %+v, want websocket_reset attempt 2 on the same route", status)
	}
	_ = first.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, body, err := first.ReadMessage(); err == nil {
		t.Fatalf("first connection received %q, want 1012", body)
	} else {
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseServiceRestart {
			t.Fatalf("first close = %v, want 1012", err)
		}
	}
	_ = first.Close()
	if status := server.retryStatuses.forSession("codex", "ws-reset-same-route"); status != nil {
		t.Fatalf("retry status leaked after wait: %+v", status)
	}

	second, secondResponse, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer second.Close()
	if secondResponse != nil && secondResponse.Body != nil {
		defer secondResponse.Body.Close()
	}
	if err := second.WriteMessage(websocket.TextMessage, []byte(create)); err != nil {
		t.Fatalf("reconnect write: %v", err)
	}
	_ = second.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, body, err := second.ReadMessage()
	if err != nil || !strings.Contains(string(body), "response.completed") {
		t.Fatalf("reconnect response = %q, err = %v; want completed", body, err)
	}

	secondArrival := <-arrivals
	if !strings.HasPrefix(firstArrival.auth, "Bearer oauth-token-") || secondArrival.auth != firstArrival.auth {
		t.Fatalf("upstream auths = %q, %q; want the same account credential", firstArrival.auth, secondArrival.auth)
	}
	if firstArrival.body != create || secondArrival.body != create {
		t.Fatalf("upstream creates = %q, %q; want the same model/prompt", firstArrival.body, secondArrival.body)
	}
}

func TestCodexWebSocketResetSafetyBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		autonomous bool
		sendCreate bool
		sendDelta  bool
		closeCode  int
		closeText  string
	}{
		{name: "visible output", autonomous: true, sendCreate: true, sendDelta: true},
		{name: "idle connection", autonomous: true},
		{name: "bounded traffic", sendCreate: true},
		{name: "terminal close with retry-looking reason", autonomous: true, sendCreate: true, closeCode: websocket.CloseInternalServerErr, closeText: "unexpected EOF"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if test.sendCreate {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
				}
				if test.sendDelta {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"partial"}`))
				}
				if test.closeCode != 0 {
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(test.closeCode, test.closeText), time.Now().Add(time.Second))
					return
				}
				_ = conn.UnderlyingConn().Close()
			}))
			defer upstream.Close()
			upstreamURL, _ := url.Parse(upstream.URL)
			server := codexEgressServer(t, upstreamURL, nil, 1)
			server.CodexOverloadFailover.persistGap = func() time.Duration { return time.Millisecond }
			server.codexOverloadRerouteCounts = newCodexOverloadReroutes()
			server.retryStatuses = newRetryStatusRegistry()
			proxy := httptest.NewServer(server.Handler())
			defer proxy.Close()

			header := http.Header{"Session-Id": []string{"ws-reset-safety-" + test.name}}
			if test.autonomous {
				header.Set(AgentRetryPolicyHeader, "autonomous")
			}
			conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxy.URL, "http")+"/backend-api/codex/responses", header)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			if response != nil && response.Body != nil {
				defer response.Body.Close()
			}
			if test.sendCreate {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra"}`)); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			if test.sendDelta {
				if _, body, err := conn.ReadMessage(); err != nil || !strings.Contains(string(body), "partial") {
					t.Fatalf("delta = %q, err = %v", body, err)
				}
			}
			_, _, readErr := conn.ReadMessage()
			var closeErr *websocket.CloseError
			if !errors.As(readErr, &closeErr) || closeErr.Code != websocket.CloseInternalServerErr {
				t.Fatalf("close = %v, want 1011 (no replay)", readErr)
			}
			if test.closeText != "" && closeErr.Text != test.closeText {
				t.Fatalf("close reason = %q, want upstream reason %q", closeErr.Text, test.closeText)
			}
			if status := server.retryStatuses.forSession("codex", "ws-reset-safety-"+test.name); status != nil {
				t.Fatalf("unexpected retry status: %+v", status)
			}
		})
	}
}

func TestAutonomousWebSocketResetClassifierRejectsUnsafeFailures(t *testing.T) {
	boundaryReset := &webSocketMessageBoundaryReadError{err: io.ErrUnexpectedEOF}
	active := func() *webSocketModelState {
		return &webSocketModelState{
			autonomousRetrying: true,
			pending:            []string{"gpt-6-astra"},
			pendingStarts:      []time.Time{time.Now()},
		}
	}
	if !active().autonomousWebSocketReset(context.Background(), boundaryReset) {
		t.Fatal("safe boundary reset was not retryable")
	}
	tests := []struct {
		name  string
		state *webSocketModelState
		ctx   context.Context
		err   error
	}{
		{name: "mid-message reset", state: active(), ctx: context.Background(), err: io.ErrUnexpectedEOF},
		{name: "normal close", state: active(), ctx: context.Background(), err: &webSocketMessageBoundaryReadError{err: &websocket.CloseError{Code: websocket.CloseNormalClosure}}},
		{name: "terminal close with retry-looking reason", state: active(), ctx: context.Background(), err: &webSocketMessageBoundaryReadError{err: &websocket.CloseError{Code: websocket.CloseInternalServerErr, Text: "unexpected EOF"}}},
		{name: "idle", state: &webSocketModelState{autonomousRetrying: true}, ctx: context.Background(), err: boundaryReset},
		{name: "bounded", state: &webSocketModelState{pending: []string{"gpt-6-astra"}, pendingStarts: []time.Time{time.Now()}}, ctx: context.Background(), err: boundaryReset},
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests = append(tests, struct {
		name  string
		state *webSocketModelState
		ctx   context.Context
		err   error
	}{name: "canceled", state: active(), ctx: canceled, err: boundaryReset})
	visible := active()
	visible.outputForwarded = true
	tests = append(tests, struct {
		name  string
		state *webSocketModelState
		ctx   context.Context
		err   error
	}{name: "visible", state: visible, ctx: context.Background(), err: boundaryReset})
	completed := active()
	completed.complete()
	tests = append(tests, struct {
		name  string
		state *webSocketModelState
		ctx   context.Context
		err   error
	}{name: "completed", state: completed, ctx: context.Background(), err: boundaryReset})
	clientEnded := active()
	clientEnded.markClientRelayDone()
	tests = append(tests, struct {
		name  string
		state *webSocketModelState
		ctx   context.Context
		err   error
	}{name: "client ended", state: clientEnded, ctx: context.Background(), err: boundaryReset})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.state.autonomousWebSocketReset(test.ctx, test.err) {
				t.Fatal("unsafe websocket failure was retryable")
			}
		})
	}
}
