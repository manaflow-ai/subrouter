package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Cross-layer invariants of the upstream transport stack. Each of these was a
// bug while every layer tracked the current account on its own.

// upstreamStackTestServer is a Codex pool of accountCount OAuth accounts
// (codex-account-N, token oauth-token-N) with overload failover on and its
// jittered gaps removed, so a test exercises every layer without sleeping. It
// has a SchedulerRef so quota and capacity marks are recorded.
func upstreamStackTestServer(t *testing.T, poolURL *url.URL, proxies []*url.URL, accountCount int) Server {
	t.Helper()
	server := codexEgressServer(t, poolURL, proxies, accountCount)
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	noGap := func() time.Duration { return 0 }
	server.CodexOverloadFailover = &CodexOverloadFailoverConfig{
		Enabled: true, sameAccountGap: noGap, switchGap: noGap, persistGap: noGap,
	}
	return server
}

func upstreamStackToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// A is overloaded, so the capacity layer moves the request to another
// account, B. B's 401 must be charged to B and never to A, even with the
// transport replay layer sitting between the capacity and usage layers, and
// the request then completes on a third account. This runs through the
// handler, so it also proves the handler wires every layer to one state.
func TestUpstreamStackChargesSwitchedAccountFailureToThatAccount(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	rejected := ""
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := upstreamStackToken(r)
		mu.Lock()
		seen = append(seen, token)
		if token != "oauth-token-0" && rejected == "" {
			rejected = token
		}
		reject := token == rejected
		mu.Unlock()
		switch {
		case token == "oauth-token-0":
			codexEgressWriteOverloaded(w)
		case reject:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"token_expired","message":"expired"}}`)
		default:
			codexEgressWriteCompleted(w, token)
		}
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	server := upstreamStackTestServer(t, poolURL, nil, 3)
	if _, err := server.Sessions.Put("codex", "session-charge", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-charge")
	mu.Lock()
	defer mu.Unlock()
	if rejected == "" {
		t.Fatalf("the request never left account 0; pool saw %v", seen)
	}
	accountB := "codex-account-" + strings.TrimPrefix(rejected, "oauth-token-")
	if status != http.StatusOK || !strings.Contains(body, "served-from-") || strings.Contains(body, "served-from-"+rejected) {
		t.Fatalf("status=%d body=%s, want a completion from the third account (pool saw %v)", status, body, seen)
	}
	if _, marked := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, accountB, ""); !marked {
		t.Fatalf("%s answered 401 but was not marked credential-exhausted (pool saw %v)", accountB, seen)
	}
	if _, marked := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, "codex-account-0", ""); marked {
		t.Fatalf("codex-account-0 was charged for %s's 401; it was only overloaded (pool saw %v)", accountB, seen)
	}
}

// The layers share one retry budget. Here the replay layer spends one retry on
// a 408 and the capacity layer spends the last on its same-account retry, so
// the capacity layer's account switch is refused for the budget, not for any
// limit of its own, and the pool sees exactly three attempts.
func TestUpstreamStackSharesRetryBudgetAcrossLayers(t *testing.T) {
	var calls atomic.Int32
	stub := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		if calls.Add(1) == 1 {
			recorder.WriteHeader(http.StatusRequestTimeout)
		} else {
			codexEgressWriteOverloaded(recorder)
		}
		return recorder.Result(), nil
	})
	var logs bytes.Buffer
	server := upstreamStackTestServer(t, &url.URL{Scheme: "https", Host: "pool.invalid"}, nil, 3)
	server.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	req, err := http.NewRequest(http.MethodPost, "https://pool.invalid/responses", strings.NewReader(`{"model":"gpt-6-astra","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer oauth-token-0")
	budget := newAttemptBudget(2)
	transport := upstreamLayers{
		usageLimit:     &usageLimitRetryTransport{method: http.MethodPost, maxAttempts: 3},
		replayablePost: &replayablePostRetryTransport{method: http.MethodPost, maxAttempts: replayablePostMaxAttempts},
		codexOverload:  &codexOverloadFailoverTransport{},
	}.build(stub, &upstreamAttempt{
		server:    &server,
		provider:  accounts.ProviderCodex,
		agent:     "codex",
		session:   "session-budget",
		path:      "/responses",
		poolModel: "gpt-6-astra",
		account:   accounts.Account{ID: "codex-account-0", Provider: accounts.ProviderCodex, CredentialVersion: "oauth-token-0"},
		budget:    budget,
		getBody:   req.GetBody,
	})
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got := calls.Load(); got != 3 {
		t.Fatalf("pool saw %d attempts, want 3: one 408 replay and one same-account capacity retry from a budget of two", got)
	}
	if budget.consume() {
		t.Fatal("budget has retries left; the layers did not spend from the same one")
	}
	if !strings.Contains(logs.String(), "codex overload failover exhausted") || !strings.Contains(logs.String(), "why=retry_budget") {
		t.Fatalf("capacity layer did not stop on the shared budget; logs:\n%s", logs.String())
	}
}

// A session pinned to an egress whose attempt answers a usage limit drops the
// pin, and the full stack below takes over: the quota account fails over, an
// overloaded account is capacity-marked (never the quota account), and the
// request completes on the healthy one.
func TestUpstreamStackPinnedEgressDropsPinOnAccountFailure(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := upstreamStackToken(r)
		mu.Lock()
		seen = append(seen, token+"@"+r.Header.Get(codexEgressHeader))
		mu.Unlock()
		switch token {
		case "oauth-token-0":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"quota"}}`)
		case "oauth-token-1":
			codexEgressWriteOverloaded(w)
		default:
			codexEgressWriteCompleted(w, token)
		}
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	var egressCalls atomic.Int32
	fra := codexEgressTestProxy(t, "fra", &egressCalls)
	server := upstreamStackTestServer(t, poolURL, []*url.URL{fra}, 3)
	if _, err := server.Sessions.Put("codex", "session-pin", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	server.codexEgressSessions = newAzureCodexSticky()
	pinKey := azureCodexSessionKeyFor("codex", "session-pin")
	server.codexEgressSessions.pin(pinKey, 0)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-pin")
	mu.Lock()
	defer mu.Unlock()
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-2") {
		t.Fatalf("status=%d body=%s, want completion from account 2 (pool saw %v)", status, body, seen)
	}
	if len(seen) == 0 || seen[0] != "oauth-token-0@fra" {
		t.Fatalf("pool saw %v, want the pinned egress attempt on account 0 first", seen)
	}
	if _, pinned := server.codexEgressSessions.lookup(pinKey); pinned {
		t.Fatal("pin should be dropped after an account-level failure")
	}
	if _, _, marked := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", ""); marked {
		t.Fatalf("account 0 answered a usage limit but was capacity-marked (pool saw %v)", seen)
	}
	if strings.Contains(strings.Join(seen, ","), "oauth-token-1@") {
		if _, _, marked := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-1", "gpt-6-astra", ""); !marked {
			t.Fatalf("account 1 answered a capacity failure but was not capacity-marked (pool saw %v)", seen)
		}
	}
	if assignment, ok := server.Sessions.Get("codex", "session-pin"); !ok || assignment.AccountID != "codex-account-2" {
		t.Fatalf("session assignment = %+v (%t), want codex-account-2", assignment, ok)
	}
}

// The response names the account that served it, with the credential that was
// actually sent. The second case is the one a shared "current account" gets
// wrong: the usage layer moves A to B, B times out, and the replay layer
// re-sends its own input, which carries A's credentials. The usage layer must
// start from A again, so the answer is attributed to A.
func TestUpstreamStackAttributesResponseToServingAccount(t *testing.T) {
	cases := []struct {
		name string
		// respond answers the nth pool call (1-based) sent with token.
		respond func(n int, token string, w http.ResponseWriter)
		want    string
		tokens  string
	}{
		{
			name: "usage failover",
			respond: func(_ int, token string, w http.ResponseWriter) {
				if token == "oauth-token-0" {
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				codexEgressWriteCompleted(w, token)
			},
			want:   "codex-account-1",
			tokens: "oauth-token-0,oauth-token-1",
		},
		{
			name: "replay after a lower failover",
			respond: func(n int, token string, w http.ResponseWriter) {
				switch n {
				case 1:
					w.WriteHeader(http.StatusTooManyRequests)
				case 2:
					w.WriteHeader(http.StatusRequestTimeout)
				default:
					codexEgressWriteCompleted(w, token)
				}
			},
			want:   "codex-account-0",
			tokens: "oauth-token-0,oauth-token-1,oauth-token-0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tokens []string
			stub := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				token := upstreamStackToken(r)
				tokens = append(tokens, token)
				recorder := httptest.NewRecorder()
				tc.respond(len(tokens), token, recorder)
				return recorder.Result(), nil
			})
			server := upstreamStackTestServer(t, &url.URL{Scheme: "https", Host: "pool.invalid"}, nil, 2)
			req, err := http.NewRequest(http.MethodPost, "https://pool.invalid/responses", strings.NewReader(`{"model":"gpt-6-astra","input":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer oauth-token-0")
			transport := upstreamLayers{
				usageLimit:     &usageLimitRetryTransport{method: http.MethodPost, maxAttempts: 3},
				replayablePost: &replayablePostRetryTransport{method: http.MethodPost, maxAttempts: replayablePostMaxAttempts},
				codexOverload:  &codexOverloadFailoverTransport{},
			}.build(stub, &upstreamAttempt{
				server:    &server,
				provider:  accounts.ProviderCodex,
				agent:     "codex",
				session:   "session-attr",
				path:      "/responses",
				poolModel: "gpt-6-astra",
				account:   accounts.Account{ID: "codex-account-0", Provider: accounts.ProviderCodex, CredentialVersion: "oauth-token-0"},
				budget:    newAttemptBudget(replayablePostMaxAttempts - 1),
				getBody:   req.GetBody,
			})
			response, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if got := strings.Join(tokens, ","); got != tc.tokens {
				t.Fatalf("pool saw %s, want %s", got, tc.tokens)
			}
			routed, ok := routedResponseAccount(response)
			wantToken := "oauth-token-" + strings.TrimPrefix(tc.want, "codex-account-")
			if !ok || routed.ID != tc.want || routed.CredentialVersion != wantToken {
				t.Fatalf("response attributed to %+v (%t), want %s with %s", routed, ok, tc.want, wantToken)
			}
		})
	}
}
