package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	// Load shedding usually clears on a quick retry, so the session's own
	// account gets one more try before the request moves.
	if got := seen(); len(got) != 3 || got[0] != "oauth-token-0" || got[1] != "oauth-token-0" || got[2] != "oauth-token-1" {
		t.Fatalf("pool saw %v, want account 0 twice then account 1", got)
	}

	// Second turn of the same session: straight to the account that served it.
	status, body = codexEgressPost(t, proxy.URL, "session-a")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("second turn status=%d body=%s", status, body)
	}
	if got := seen(); len(got) != 4 || got[3] != "oauth-token-1" {
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
	if got := seen(); len(got) != 4 {
		t.Fatalf("pool saw %d attempts %v, want first account, one same-account retry, plus 2 switches", len(got), got)
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
	// The default ladder retries A once before switching.
	if strings.Join(seen, ",") != "tok-a,tok-a,"+rejected+","+tokenC {
		t.Fatalf("upstream saw %v, want A twice, B (%s) once, then C (%s)", seen, rejected, tokenC)
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

// Codex has answered "Selected model is at capacity" as a 400 or 429 JSON
// body, as an SSE error on a non-2xx status, and as a 2xx JSON error body. All
// of them are capacity failures; client errors and quota codes never are, even
// when their message happens to mention capacity.
func TestCodexCapacityClassifierRecognizesBodiesOnAnyStatus(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		want        bool
	}{
		{"400 json server_is_overloaded", 400, "application/json", `{"error":{"code":"server_is_overloaded","message":"busy"}}`, true},
		{"429 json server_overloaded type", 429, "application/json", `{"error":{"type":"server_overloaded","message":"busy"}}`, true},
		{"400 json at-capacity message", 400, "application/json", `{"error":{"message":"Selected model is at capacity. Please try a different model.","code":null}}`, true},
		{"400 json temporarily overloaded", 400, "application/json; charset=utf-8", `{"error":{"message":"The engine is temporarily overloaded"}}`, true},
		{"429 json slow_down", 429, "application/json", `{"error":{"code":"slow_down"}}`, true},
		{"2xx json error body", 200, "application/json", `{"error":{"code":"server_is_overloaded","message":"busy"}}`, true},
		{"2xx json failed response object", 200, "application/json", `{"object":"response","status":"failed","error":{"code":"slow_down","message":"x"}}`, true},
		{"400 sse error event", 400, "text/event-stream", "data: {\"type\":\"error\",\"code\":\"server_is_overloaded\",\"message\":\"busy\"}\n\n", true},
		{"503 status", 503, "application/json", `{}`, true},
		{"400 context length", 400, "application/json", `{"error":{"code":"context_length_exceeded","message":"model is at capacity"}}`, false},
		{"400 invalid_* code with capacity words", 400, "application/json", `{"error":{"code":"invalid_value","message":"temporarily overloaded"}}`, false},
		{"400 invalid_request_error type", 400, "application/json", `{"error":{"type":"invalid_request_error","message":"model is at capacity"}}`, false},
		{"429 usage limit", 429, "application/json", `{"error":{"type":"usage_limit_reached","message":"quota"}}`, false},
		{"429 rate limit code", 429, "application/json", `{"error":{"code":"rate_limit_exceeded"}}`, false},
		{"429 insufficient quota", 429, "application/json", `{"error":{"code":"insufficient_quota"}}`, false},
		{"400 unknown code", 400, "application/json", `{"error":{"code":"something_new"}}`, false},
		{"2xx json success", 200, "application/json", `{"object":"response","status":"completed","error":null,"output":[{"type":"message","content":[{"type":"output_text","text":"model is at capacity"}]}]}`, false},
		{"404 plain", 404, "text/plain", `not found`, false},
	}
	for _, test := range cases {
		response := &http.Response{
			StatusCode: test.status,
			Header:     http.Header{"Content-Type": []string{test.contentType}},
			Body:       io.NopCloser(strings.NewReader(test.body)),
		}
		failed, _, replaced := codexOverloadFailure(response)
		if failed != test.want {
			t.Errorf("%s: capacity = %v, want %v", test.name, failed, test.want)
		}
		rest, err := io.ReadAll(replaced.Body)
		if err != nil || string(rest) != test.body {
			t.Errorf("%s: body after classification = %q (err %v), want the original", test.name, rest, err)
		}
	}
}

// A capacity failure answered as a 400 JSON body moves the request to another
// account exactly like the in-stream form does.
func TestCodexOverloadFailoverOnCapacityBodyWithClientStatus(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == "oauth-token-0" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity. Please try a different model."}}`)
			return
		}
		codexEgressWriteCompleted(w, "oauth-token-1")
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	server := codexOverloadServer(t, poolURL, 2, true)
	if _, err := server.Sessions.Put("codex", "session-400", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-400")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("status=%d body=%s, want the second account to serve after a 400 capacity body", status, body)
	}
}

// Codex shows nothing for a reasoning item until its first delta, so a
// capacity failure that lands after an output_item.added (or any other
// non-visible event) is still a pre-output failure the peek must catch. Once a
// delta or finished item has been seen, the failure belongs to the client.
func TestCodexStreamPeekContinuesUntilFirstVisibleOutput(t *testing.T) {
	failed := "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\"}}}\n\n"
	cases := []struct {
		name string
		body string
		want codexFailureClass
	}{
		{"after reasoning item", "data: {\"type\":\"response.created\"}\n\n" +
			"data: {\"type\":\"response.in_progress\"}\n\n" +
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\"}}\n\n" +
			"data: {\"type\":\"response.reasoning_summary_part.added\"}\n\n" + failed, codexFailureServer},
		{"after preamble message item and rate limit event", "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
			"data: {\"type\":\"codex.rate_limits\"}\n\n" +
			": keepalive\n\n" +
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\"}}\n\n" +
			"data: {\"type\":\"response.content_part.added\"}\n\n" + failed, codexFailureServer},
		{"after a text delta", "data: {\"type\":\"response.created\"}\n\n" +
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" + failed, codexFailureNone},
		{"after a reasoning summary delta", "data: {\"type\":\"response.created\"}\n\n" +
			"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking\"}\n\n" + failed, codexFailureNone},
		{"after a finished item", "data: {\"type\":\"response.created\"}\n\n" +
			"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\"}}\n\n" + failed, codexFailureNone},
	}
	for _, test := range cases {
		response := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(test.body)),
		}
		class, replaced := azureCodexStreamFailure(response)
		if class != test.want {
			t.Errorf("%s: class = %v, want %v", test.name, class, test.want)
		}
		rest, err := io.ReadAll(replaced.Body)
		if err != nil || string(rest) != test.body {
			t.Errorf("%s: restitched body = %q (err %v), want the original", test.name, rest, err)
		}
	}
}

// An upstream that goes quiet before any output (a long think with no
// summary) must not hold the stream hostage: the peek gives up after its time
// cap and hands back every byte, including ones that arrive later.
func TestCodexStreamPeekIsTimeBounded(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	head := "data: {\"type\":\"response.created\"}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\"}}\n\n"
	go func() { _, _ = io.WriteString(writer, head) }()
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       reader,
	}
	type result struct {
		class    codexFailureClass
		response *http.Response
	}
	done := make(chan result, 1)
	started := time.Now()
	go func() {
		class, replaced := azureCodexStreamFailure(response)
		done <- result{class, replaced}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("stream peek blocked on a silent upstream past its time cap")
	}
	if elapsed := time.Since(started); elapsed < 2*time.Second {
		t.Fatalf("peek returned after %v, want it to wait for output up to its cap", elapsed)
	}
	if got.class != codexFailureNone {
		t.Fatalf("class = %v, want none after the time cap", got.class)
	}
	tail := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n"
	go func() {
		_, _ = io.WriteString(writer, tail)
		_ = writer.Close()
	}()
	rest, err := io.ReadAll(got.response.Body)
	if err != nil || string(rest) != head+tail {
		t.Fatalf("body after timeout = %q (err %v), want %q", rest, err, head+tail)
	}
}

// The capacity mark follows the upstream's own retry hint (Retry-After,
// retry_after_ms, resets_in_seconds), clamped to [30s, 5m] with jitter, and
// falls back to the configured TTL when there is none.
func TestCodexOverloadMarkTTLHonorsRetryHints(t *testing.T) {
	cases := []struct {
		name     string
		header   string
		body     string
		min, max time.Duration
	}{
		{"retry-after header", "200", `{"error":{"code":"server_is_overloaded"}}`, 160 * time.Second, 240 * time.Second},
		{"retry_after_ms body", "", `{"error":{"code":"server_is_overloaded","retry_after_ms":60000}}`, 48 * time.Second, 72 * time.Second},
		{"resets_in_seconds body", "", `{"error":{"code":"slow_down","resets_in_seconds":90}}`, 72 * time.Second, 108 * time.Second},
		{"short hint clamps up", "", `{"error":{"code":"server_is_overloaded","retry_after_ms":2000}}`, 30 * time.Second, 30 * time.Second},
		{"long hint clamps down", "3600", `{"error":{"code":"server_is_overloaded"}}`, 5 * time.Minute, 5 * time.Minute},
		{"no hint uses the default", "", `{"error":{"code":"server_is_overloaded"}}`, 96 * time.Second, 144 * time.Second},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == "oauth-token-0" {
					w.Header().Set("Content-Type", "application/json")
					if test.header != "" {
						w.Header().Set("Retry-After", test.header)
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, test.body)
					return
				}
				codexEgressWriteCompleted(w, "ok")
			}))
			defer pool.Close()
			poolURL, _ := url.Parse(pool.URL)
			server := codexOverloadServer(t, poolURL, 2, true)
			server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
			sessionID := fmt.Sprintf("session-ttl-%d", index)
			if _, err := server.Sessions.Put("codex", sessionID, "codex-account-0", ""); err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(server.Handler())
			defer proxy.Close()
			started := time.Now()
			if status, body := codexEgressPost(t, proxy.URL, sessionID); status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, body)
			}
			until, _, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "")
			if !ok {
				t.Fatal("overloaded account was not marked")
			}
			ttl := until.Sub(started)
			slack := 2 * time.Second
			if ttl < test.min-slack || ttl > test.max+slack {
				t.Fatalf("mark ttl = %v, want within [%v, %v]", ttl, test.min, test.max)
			}
		})
	}
}

func TestCodexRetryHintJSONShapes(t *testing.T) {
	cases := map[string]time.Duration{
		`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","retry_after_ms":1500}}}`: 1500 * time.Millisecond,
		`{"error":{"retry_after":"12s"}}`:           12 * time.Second,
		`{"error":{"resets_in_seconds":"45"}}`:      45 * time.Second,
		`{"type":"error","retry_after":7}`:          7 * time.Second,
		`{"error":{"code":"server_is_overloaded"}}`: 0,
		`not json`: 0,
	}
	for body, want := range cases {
		if got := codexRetryHintJSON([]byte(body)); got != want {
			t.Errorf("codexRetryHintJSON(%s) = %v, want %v", body, got, want)
		}
	}
}

// Switching accounts right away piles onto a pool that is shedding load; the
// failover waits a short jittered beat between accounts.
func TestCodexOverloadFailoverBacksOffBetweenAccounts(t *testing.T) {
	var mu sync.Mutex
	var arrivals []time.Time
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == "oauth-token-0" {
			codexEgressWriteOverloaded(w)
			return
		}
		codexEgressWriteCompleted(w, "ok")
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	server := codexOverloadServer(t, poolURL, 2, true)
	if _, err := server.Sessions.Put("codex", "session-backoff", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()
	if status, body := codexEgressPost(t, proxy.URL, "session-backoff"); status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(arrivals) != 3 {
		t.Fatalf("pool saw %d attempts, want 3 (first, same-account retry, switch)", len(arrivals))
	}
	if gap := arrivals[1].Sub(arrivals[0]); gap < 250*time.Millisecond || gap > 1500*time.Millisecond {
		t.Fatalf("gap before the same-account retry = %v, want a 250-750ms backoff", gap)
	}
	if gap := arrivals[2].Sub(arrivals[1]); gap < 100*time.Millisecond || gap > 1500*time.Millisecond {
		t.Fatalf("gap before the switch = %v, want a 100-400ms backoff", gap)
	}
}

// The regional egress treats a 2xx JSON capacity body like an in-stream one.
func TestCodexEgressReplaysJSONCapacityBody(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if region := r.Header.Get(codexEgressHeader); region != "" {
			codexEgressWriteCompleted(w, region)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"error":{"code":"server_is_overloaded","message":"busy"}}`)
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	var calls atomic.Int32
	fra := codexEgressTestProxy(t, "fra", &calls)
	proxy := httptest.NewServer(codexEgressServer(t, poolURL, []*url.URL{fra}, 1).Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-json")
	if status != http.StatusOK || !strings.Contains(body, "served-from-fra") {
		t.Fatalf("status=%d body=%s, want the egress to serve after a JSON capacity body", status, body)
	}
}
