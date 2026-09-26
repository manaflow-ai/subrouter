package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// codexOverloadPool is a fake pool where a named set of OAuth tokens is
// "overloaded": those accounts open every stream with server_is_overloaded,
// every other account completes. It records the token order it saw.
func codexOverloadPool(t *testing.T, overloaded ...string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	bad := map[string]bool{}
	for _, token := range overloaded {
		bad[token] = true
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, token)
		mu.Unlock()
		if bad[token] {
			codexEgressWriteOverloaded(w)
			return
		}
		codexEgressWriteCompleted(w, token)
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func codexOverloadServer(t *testing.T, poolURL *url.URL, accountCount int, enabled bool) Server {
	t.Helper()
	server := codexEgressServer(t, poolURL, nil, accountCount)
	if enabled {
		server.CodexOverloadFailover = &CodexOverloadFailoverConfig{Enabled: true}
	}
	return server
}

// The first account answers server_is_overloaded; the same request completes
// on the second account, the session sticks to it, and the overloaded account
// is out of routing for this pool.
func TestCodexOverloadFailoverSwitchesAccountAndSticks(t *testing.T) {
	pool, seen := codexOverloadPool(t, "oauth-token-0")
	poolURL, _ := url.Parse(pool.URL)
	server := codexOverloadServer(t, poolURL, 2, true)
	// Arrive as a sticky session already on account 0, the way a live
	// conversation would.
	if _, err := server.Sessions.Put("codex", "session-a", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-a")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("status=%d body=%s, want completion from account 1", status, body)
	}
	if strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("overloaded stream leaked to the client: %s", body)
	}
	if got := seen(); len(got) != 2 || got[0] != "oauth-token-0" || got[1] != "oauth-token-1" {
		t.Fatalf("pool saw %v, want account 0 then account 1", got)
	}

	// Second turn of the same session: straight to the account that served it.
	status, body = codexEgressPost(t, proxy.URL, "session-a")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("second turn status=%d body=%s", status, body)
	}
	if got := seen(); len(got) != 3 || got[2] != "oauth-token-1" {
		t.Fatalf("pool saw %v, want the second turn to go only to account 1", got)
	}

	// A brand-new session must also avoid the overloaded account while the
	// mark is fresh.
	status, body = codexEgressPost(t, proxy.URL, "session-b")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("new session status=%d body=%s", status, body)
	}
	if got := seen(); got[len(got)-1] != "oauth-token-1" {
		t.Fatalf("new session was routed to the overloaded account: %v", got)
	}
}

// Every account overloaded: the pool's own failure is returned after the
// bounded number of switches, not an endless loop.
func TestCodexOverloadFailoverBoundedWhenAllAccountsFail(t *testing.T) {
	pool, seen := codexOverloadPool(t, "oauth-token-0", "oauth-token-1", "oauth-token-2", "oauth-token-3", "oauth-token-4", "oauth-token-5")
	poolURL, _ := url.Parse(pool.URL)
	server := codexOverloadServer(t, poolURL, 6, true)
	server.CodexOverloadFailover.MaxAccounts = 2
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-c")
	if status != http.StatusOK || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s, want the pool failure passed through", status, body)
	}
	if got := seen(); len(got) != 3 {
		t.Fatalf("pool saw %d attempts %v, want first account plus 2 switches", len(got), got)
	}
}

// Accounts first, then egress: when every account fails, the regional egress
// still gets its turn and serves the request.
func TestCodexOverloadFailoverFallsThroughToEgress(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if region := r.Header.Get(codexEgressHeader); region != "" {
			codexEgressWriteCompleted(w, region)
			return
		}
		codexEgressWriteOverloaded(w)
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	var calls atomic.Int32
	fra := codexEgressTestProxy(t, "fra", &calls)
	server := codexEgressServer(t, poolURL, []*url.URL{fra}, 2)
	server.CodexOverloadFailover = &CodexOverloadFailoverConfig{Enabled: true}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-d")
	if status != http.StatusOK || !strings.Contains(body, "served-from-fra") {
		t.Fatalf("status=%d body=%s, want the egress to serve after both accounts failed", status, body)
	}
	if calls.Load() != 1 {
		t.Fatalf("egress calls=%d, want 1", calls.Load())
	}
}

// Off by default: one attempt, the failure reaches the client unchanged.
func TestCodexOverloadFailoverOffByDefault(t *testing.T) {
	pool, seen := codexOverloadPool(t, "oauth-token-0", "oauth-token-1")
	poolURL, _ := url.Parse(pool.URL)
	proxy := httptest.NewServer(codexOverloadServer(t, poolURL, 2, false).Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-e")
	if status != http.StatusOK || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if got := seen(); len(got) != 1 {
		t.Fatalf("pool saw %v, want a single attempt with failover off", got)
	}
}

// Quota failures are not capacity failures: the usage-limit layer owns them.
func TestCodexOverloadFailoverLeavesQuotaToUsageLayer(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"quota"}}`)
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	proxy := httptest.NewServer(codexOverloadServer(t, poolURL, 1, true).Handler())
	defer proxy.Close()
	status, _ := codexEgressPost(t, proxy.URL, "session-f")
	if status != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want the 429 passed through", status)
	}
}

// The overload layer switches accounts above the usage-limit layer. After it
// moves the request from A (overloaded) to B, a 401 from B must be charged to
// B, not A: the usage layer has to start from the account the overload layer
// actually selected, so it marks B credential-dead, does not replay B, fails
// over to C, and the response and session land on C.
func TestCodexOverloadSwitchChargesUsageFailureToSelectedAccount(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-g", "codex-a", ""); err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{
			{ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-a"},
			{ID: "codex-b", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-b"},
			{ID: "codex-c", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-c"},
		},
		Sessions:              store,
		SchedulerRef:          selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		CodexOverloadFailover: &CodexOverloadFailoverConfig{Enabled: true},
	}
	// A is overloaded. Whichever account the overload layer switches to
	// (call it B) answers 401; any other account completes.
	var seen []string
	rejected := ""
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		seen = append(seen, token)
		if token != "tok-a" && rejected == "" {
			rejected = token
		}
		recorder := httptest.NewRecorder()
		switch token {
		case "tok-a":
			codexEgressWriteOverloaded(recorder)
		case rejected:
			recorder.Header().Set("Content-Type", "application/json")
			recorder.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(recorder, `{"error":{"code":"token_expired","message":"expired"}}`)
		default:
			codexEgressWriteCompleted(recorder, token)
		}
		return recorder.Result()
	}}
	transport := codexOverloadFailoverTransport{
		base: usageLimitRetryTransport{
			base:              stub,
			server:            &server,
			provider:          accounts.ProviderCodex,
			agent:             "codex",
			session:           "session-g",
			account:           "codex-a",
			accountCredential: "tok-a",
			method:            http.MethodPost,
			path:              "/responses",
			maxAttempts:       3,
			poolModel:         "gpt-6-astra",
		},
		server:    &server,
		agent:     "codex",
		session:   "session-g",
		account:   "codex-a",
		poolModel: "gpt-6-astra",
	}
	body := `{"model":"gpt-6-astra","input":[]}`
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.example/backend-api/codex/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-a")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()

	accountB := "codex-" + strings.TrimPrefix(rejected, "tok-")
	accountC, tokenC := "codex-b", "tok-b"
	if accountB == "codex-b" {
		accountC, tokenC = "codex-c", "tok-c"
	}
	if strings.Join(seen, ",") != "tok-a,"+rejected+","+tokenC {
		t.Fatalf("upstream saw %v, want A, B (%s) once, then C (%s)", seen, rejected, tokenC)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(payload), "served-from-"+tokenC) {
		t.Fatalf("status=%d body=%s, want completion from account C", response.StatusCode, payload)
	}
	if _, marked := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, accountB, ""); !marked {
		t.Fatal("account B answered 401 but was not marked credential-exhausted")
	}
	if _, marked := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, "codex-a", ""); marked {
		t.Fatal("account A was marked credential-exhausted for B's 401; A was only overloaded")
	}
	routed, ok := routedResponseAccount(response)
	if !ok || routed.ID != accountC || routed.CredentialVersion != tokenC {
		t.Fatalf("response attributed to %+v (%t), want %s", routed, ok, accountC)
	}
	if assignment, ok := store.Get("codex", "session-g"); !ok || assignment.AccountID != accountC {
		t.Fatalf("session assignment = %+v (%t), want %s", assignment, ok, accountC)
	}
}

func TestCodexOverloadRerouteBudget(t *testing.T) {
	counts := newCodexOverloadReroutes()
	for i := 0; i < codexOverloadMaxWebSocketReroutes; i++ {
		if !counts.allow("s", codexOverloadMaxWebSocketReroutes) {
			t.Fatalf("reroute %d refused inside budget", i+1)
		}
	}
	if counts.allow("s", codexOverloadMaxWebSocketReroutes) {
		t.Fatal("reroute allowed past budget")
	}
	if !counts.allow("other", codexOverloadMaxWebSocketReroutes) {
		t.Fatal("another session must have its own budget")
	}
}
