package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

const capacitySwitchModel = "gpt-5.6-sol"

// capacitySwitchUpstream is a Codex websocket upstream with two accounts:
// the account whose token is in failing answers every response.create with a
// capacity failure, the other completes it. It records the token of every
// response.create it saw, in order.
type capacitySwitchUpstream struct {
	mu      sync.Mutex
	failing map[string]int // token -> capacity failures still to answer (-1: forever)
	creates []string
}

func (u *capacitySwitchUpstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.creates...)
}

func (u *capacitySwitchUpstream) handler(t *testing.T) http.Handler {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
			u.mu.Lock()
			u.creates = append(u.creates, token)
			left := u.failing[token]
			if left > 0 {
				u.failing[token] = left - 1
			}
			u.mu.Unlock()
			if left != 0 {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCapacityFailure))
				continue
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryDelta))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCompleted))
		}
	})
}

// capacitySwitchProxy is an autonomous-capable relay over two Codex accounts
// with the overload failover on, the session already on account 0.
func capacitySwitchProxy(t *testing.T, upstream *httptest.Server) (*Server, string) {
	t.Helper()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "capacity-switch", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	scheduler := selectacct.NewScheduler(nil)
	server := &Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "codex-account-0", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "codex-token-0"},
			{ID: "codex-account-1", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "codex-token-1"},
		},
		Sessions:                     store,
		Scheduler:                    scheduler,
		SchedulerRef:                 selectacct.NewSchedulerRef(scheduler),
		MaxBodyBytes:                 1 << 20,
		Logger:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		CodexOverloadFailover:        &CodexOverloadFailoverConfig{Enabled: true},
		codexWebSocketTurnRetryDelay: noTurnRetryDelay,
	}
	fastCapacityGaps(server.CodexOverloadFailover, time.Millisecond)
	proxy := httptest.NewServer(server.Handler())
	t.Cleanup(proxy.Close)
	return server, "ws" + strings.TrimPrefix(proxy.URL, "http") + "/v1/responses"
}

func capacitySwitchDial(t *testing.T, wsURL string) *websocket.Conn {
	t.Helper()
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"X-Subrouter-Session":  []string{"capacity-switch"},
		AgentRetryPolicyHeader: []string{"autonomous"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = response.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// capacitySwitchCreate is a self-contained turn with a conversation large
// enough (~50k input tokens) that the failover's size cap would once have
// kept it on its account.
func capacitySwitchCreate() []byte {
	return []byte(`{"type":"response.create","model":"` + capacitySwitchModel + `","input":[{"type":"message","role":"user","content":"` +
		strings.Repeat("abcd", 50_000) + `"}]}`)
}

// An autonomous turn that keeps hitting capacity on its account must not be
// replayed there without end: after the same quick retry the HTTP failover
// allows, the account is marked at capacity for the model and the session
// leaves it, even for a conversation above the size cap. The client sees no
// failure event, only the 1012 its reconnect handles, and the reconnect is
// served by the other account.
func TestAutonomousCodexWebSocketSwitchesAccountAfterQuickCapacityRetry(t *testing.T) {
	upstreamState := &capacitySwitchUpstream{failing: map[string]int{"codex-token-0": -1}}
	upstream := httptest.NewServer(upstreamState.handler(t))
	defer upstream.Close()
	server, wsURL := capacitySwitchProxy(t, upstream)

	client := capacitySwitchDial(t, wsURL)
	if err := client.WriteMessage(websocket.TextMessage, capacitySwitchCreate()); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, body, err := client.ReadMessage()
		if err == nil {
			if strings.Contains(string(body), "failed") || strings.Contains(string(body), `"error"`) {
				t.Fatalf("client received failure event %q", body)
			}
			t.Fatalf("client received %q before the switch, want only the reconnect close", body)
		}
		if !websocket.IsCloseError(err, websocket.CloseServiceRestart) {
			t.Fatalf("client read: %v, want close 1012 so the session reconnects elsewhere", err)
		}
		break
	}
	if got := strings.Join(upstreamState.seen(), ","); got != "codex-token-0,codex-token-0" {
		t.Fatalf("upstream saw %s, want account 0 once plus one quick retry", got)
	}
	trafficURL := "http" + strings.TrimSuffix(strings.TrimPrefix(wsURL, "ws"), "/v1/responses") + "/_subrouter/traffic"
	trafficResponse, err := http.Get(trafficURL)
	if err != nil {
		t.Fatal(err)
	}
	var traffic struct {
		CodexCapacity struct {
			Counters codexCapacityCounters `json:"counters"`
		} `json:"codex_capacity"`
	}
	decodeErr := json.NewDecoder(trafficResponse.Body).Decode(&traffic)
	_ = trafficResponse.Body.Close()
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if counters := traffic.CodexCapacity.Counters; counters.AccountSwitches != 1 || counters.SameAccountRetries != 1 {
		t.Fatalf("traffic capacity counters = %+v, want one same-account retry and one account switch", counters)
	}
	until, _, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", capacitySwitchModel, "")
	if !ok {
		t.Fatal("account 0 is not marked at capacity for the model")
	}
	if left := time.Until(until); left < 4*time.Minute {
		t.Fatalf("capacity mark expires in %s, want the ~5 minute default", left)
	}
	if _, _, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-5.5", ""); ok {
		t.Fatal("the capacity mark leaked to another model")
	}

	reconnected := capacitySwitchDial(t, wsURL)
	if err := reconnected.WriteMessage(websocket.TextMessage, capacitySwitchCreate()); err != nil {
		t.Fatal(err)
	}
	readTurn(t, reconnected)
	seen := upstreamState.seen()
	if last := seen[len(seen)-1]; last != "codex-token-1" {
		t.Fatalf("reconnected turn went to %s, want account 1", last)
	}
	if assignment, ok := server.Sessions.Get("codex", "capacity-switch"); !ok || assignment.AccountID != "codex-account-1" {
		t.Fatalf("session = %+v, want it moved to account 1", assignment)
	}
}

// With nowhere better to go (the only other account is already marked at
// capacity for the model) the turn keeps retrying behind the open socket:
// a reconnect would land on an account just as constrained.
func TestAutonomousCodexWebSocketKeepsRetryingWhenNoAccountIsFree(t *testing.T) {
	upstreamState := &capacitySwitchUpstream{failing: map[string]int{"codex-token-0": 4}}
	upstream := httptest.NewServer(upstreamState.handler(t))
	defer upstream.Close()
	server, wsURL := capacitySwitchProxy(t, upstream)
	server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-1", capacitySwitchModel, "", time.Now().Add(5*time.Minute))

	client := capacitySwitchDial(t, wsURL)
	if err := client.WriteMessage(websocket.TextMessage, capacitySwitchCreate()); err != nil {
		t.Fatal(err)
	}
	readTurn(t, client)
	for _, token := range upstreamState.seen() {
		if token != "codex-token-0" {
			t.Fatalf("upstream saw %v, want every attempt on account 0", upstreamState.seen())
		}
	}
}

// A failure that does not name capacity (a generic server error) keeps the
// existing in-place retry: only capacity moves the session.
func TestAutonomousCodexWebSocketGenericServerErrorDoesNotSwitch(t *testing.T) {
	const generic = `{"type":"response.failed","response":{"error":{"code":"server_error","message":"An error occurred while processing your request."}}}`
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var mu sync.Mutex
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			mu.Lock()
			seen = append(seen, token)
			n := len(seen)
			mu.Unlock()
			if n <= 4 {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(generic))
				continue
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryDelta))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCompleted))
		}
	}))
	defer upstream.Close()
	_, wsURL := capacitySwitchProxy(t, upstream)

	client := capacitySwitchDial(t, wsURL)
	if err := client.WriteMessage(websocket.TextMessage, capacitySwitchCreate()); err != nil {
		t.Fatal(err)
	}
	readTurn(t, client)
	mu.Lock()
	defer mu.Unlock()
	for _, token := range seen {
		if token != "codex-token-0" {
			t.Fatalf("upstream saw %v, want a generic server error retried on account 0", seen)
		}
	}
}

// The failure-rate taint holds an account out of the model that keeps
// failing on it, not out of the whole pool: the same account still serves
// other models.
func TestCodexCapacityTaintIsScopedToTheFailingModel(t *testing.T) {
	poolURL, _ := codexCapacityPool(t, func(string, int) bool { return false })
	server := codexOverloadServer(t, poolURL, 2, true)
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	tainted := false
	for i := 0; i < 8 && !tainted; i++ {
		verdict := server.noteCodexCapacityFailure("codex-account-0", "codex:session-"+string(rune('a'+i)), "gpt-6-astra", "", "server_is_overloaded", time.Minute)
		tainted = verdict.Tainted
	}
	if !tainted {
		t.Fatal("repeated capacity failures across sessions never tainted the account")
	}
	if _, _, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", ""); !ok {
		t.Fatal("the tainted account is not held out of the failing model")
	}
	if _, _, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-5.5", ""); ok {
		t.Fatal("the taint marked another model")
	}
	tried := map[string]struct{}{"codex-account-1": {}}
	next, err := server.oauthRetryCandidate(context.Background(), accounts.ProviderCodex, "codex", "session-other-model", "", "gpt-5.5", tried, false, false)
	if err != nil || next.ID != "codex-account-0" {
		t.Fatalf("candidate for another model = %q, %v; want the tainted account still eligible", next.ID, err)
	}
}
