package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

type admissionSpillFixture struct {
	server   Server
	handler  http.Handler
	control  *admissionControl
	store    *session.Store
	mu       sync.Mutex
	upstream []string
}

// newAdmissionSpillFixture serves two healthy Claude subscription accounts
// through the real handler, with an admission limit of one so a test can
// saturate an account by seeding its bucket.
func newAdmissionSpillFixture(t *testing.T) *admissionSpillFixture {
	t.Helper()
	return newAdmissionSpillFixtureWith(t, []string{"a@example.com", "b@example.com"}, nil)
}

// newAdmissionSpillFixtureWith serves ids in order; respond, when set, may
// answer a request itself given the credential it carries.
func newAdmissionSpillFixtureWith(t *testing.T, ids []string, respond func(w http.ResponseWriter, token string) bool) *admissionSpillFixture {
	t.Helper()
	now := time.Now()
	var accountList []accounts.Account
	var scores []selectacct.Score
	for _, id := range ids {
		accountList = append(accountList, accounts.Account{
			ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-" + id,
		})
		scores = append(scores, scoreFromUsageWindows(accounts.ProviderClaude, id, []accounts.UsageWindow{
			fiveHour(10, now.Add(2*time.Hour)), sevenDay(10, now.Add(72*time.Hour)),
		}))
	}
	f := &admissionSpillFixture{control: &admissionControl{
		limit:      1,
		queueCap:   4,
		spillQueue: 1,
		buckets:    make(map[admissionKey]*admissionBucket),
	}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.upstream = append(f.upstream, token)
		f.mu.Unlock()
		if respond != nil && respond(w, token) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	t.Cleanup(upstream.Close)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	server := Server{
		Sessions:       store,
		ClaudeUpstream: mustParseURL(t, upstream.URL),
		Accounts:       accountList,
		SchedulerRef:   selectacct.NewSchedulerRef(selectacct.NewScheduler(scores)),
		MaxBodyBytes:   1 << 20,
		admission:      f.control,
	}
	f.server = server
	f.handler = server.Handler()
	return f
}

func (f *admissionSpillFixture) send(t *testing.T, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "http://subrouter.local/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, req)
	return response
}

// saturate makes account look like a session fan-out holds every slot and
// waiters queues behind it.
func (f *admissionSpillFixture) saturate(account string, waiters int) {
	f.control.mu.Lock()
	defer f.control.mu.Unlock()
	bucket := f.control.bucketLocked(admissionKeyFor(accounts.ProviderClaude, account, "claude-opus"))
	bucket.active = f.control.limit
	for range waiters {
		bucket.waiters = append(bucket.waiters, &admissionWaiter{ready: make(chan struct{})})
	}
}

func (f *admissionSpillFixture) lastUpstreamToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.upstream) == 0 {
		return ""
	}
	return f.upstream[len(f.upstream)-1]
}

func TestHandlerSpillsSaturatedStickyAccountToFreeAccount(t *testing.T) {
	f := newAdmissionSpillFixture(t)
	const sessionID = "session-fan-out"
	if first := f.send(t, sessionID); first.Code != http.StatusOK {
		t.Fatalf("placement status = %d, body=%s", first.Code, first.Body.String())
	}
	assignment, ok := f.store.Get("claude", sessionID)
	if !ok {
		t.Fatal("first request did not pin the session")
	}
	pinned := assignment.AccountID
	other := "a@example.com"
	if pinned == other {
		other = "b@example.com"
	}
	f.saturate(pinned, 1)

	response := f.send(t, sessionID)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the free account; body=%s", response.Code, response.Body.String())
	}
	if got := f.lastUpstreamToken(); got != "tok-"+other {
		t.Fatalf("upstream credential = %q, want the free account %q", got, "tok-"+other)
	}
	if assignment, _ := f.store.Get("claude", sessionID); assignment.AccountID != pinned {
		t.Fatalf("spill moved the session to %q; the fan-out's sticky account %q must stay", assignment.AccountID, pinned)
	}
}

func TestHandlerFullAdmissionQueueAnswersWithRetryAfter(t *testing.T) {
	f := newAdmissionSpillFixture(t)
	f.saturate("a@example.com", f.control.queueCap)
	f.saturate("b@example.com", f.control.queueCap)

	response := f.send(t, "session-pool-saturated")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", response.Code, response.Body.String())
	}
	seconds, err := strconv.Atoi(response.Header().Get("Retry-After"))
	if err != nil || seconds < minAdmissionRetryAfter || seconds > maxAdmissionRetryAfter {
		t.Fatalf("Retry-After = %q, want %d-%d seconds", response.Header().Get("Retry-After"), minAdmissionRetryAfter, maxAdmissionRetryAfter)
	}
	if got := response.Header().Get(subrouterReasonHeader); got != admissionReasonQueueFull {
		t.Fatalf("%s = %q, want %q", subrouterReasonHeader, got, admissionReasonQueueFull)
	}
	if !strings.Contains(response.Body.String(), "upstream admission queue is full") {
		t.Fatalf("body = %q", response.Body.String())
	}
	if got := f.lastUpstreamToken(); got != "" {
		t.Fatalf("saturated pool reached upstream with %q", got)
	}
}

func TestAdmissionRetryAfterScalesWithQueueAndStaysBounded(t *testing.T) {
	control := testAdmissionControl(6, 64)
	for _, tc := range []struct{ waiters, want int }{{0, 1}, {6, 1}, {7, 2}, {18, 3}, {64, 5}} {
		bucket := &admissionBucket{waiters: make([]*admissionWaiter, tc.waiters)}
		if got := control.retryAfterLocked(bucket); got != tc.want {
			t.Errorf("waiters=%d: retry after %ds, want %ds", tc.waiters, got, tc.want)
		}
	}
}

func TestAdmissionSpillReservesItsTargetSoConcurrentSpillsSpread(t *testing.T) {
	f := newAdmissionSpillFixtureWith(t, []string{"a@example.com", "b@example.com", "c@example.com"}, nil)
	f.saturate("a@example.com", 1)
	server := f.server
	server.admission = f.control
	transport := usageLimitRetryTransport{server: &server, provider: accounts.ProviderClaude, poolModel: "claude-opus"}
	transport.attempt = &upstreamAttempt{server: &server, provider: accounts.ProviderClaude, poolModel: "claude-opus",
		getBody: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("{}")), nil }}
	first, firstLease, ok := transport.admissionSpill(context.Background(), "a@example.com", map[string]struct{}{})
	if !ok {
		t.Fatal("first spill found no account")
	}
	defer firstLease.release()
	second, secondLease, ok := transport.admissionSpill(context.Background(), "a@example.com", map[string]struct{}{})
	if !ok {
		t.Fatalf("second spill found no account; the first took %s", first.ID)
	}
	defer secondLease.release()
	if first.ID == second.ID {
		t.Fatalf("both spills chose %s; the first must hold its slot so the second goes elsewhere", first.ID)
	}
}

func TestHandlerSpilledRequestNeverMovesTheSessionAfterLaterFailover(t *testing.T) {
	f := newAdmissionSpillFixtureWith(t, []string{"a@example.com", "b@example.com", "c@example.com"}, func(w http.ResponseWriter, token string) bool {
		if token != "tok-b@example.com" {
			return false
		}
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-5h-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
		return true
	})
	const sessionID = "session-spill-then-quota"
	if _, err := f.store.Put("claude", sessionID, "a@example.com", ""); err != nil {
		t.Fatal(err)
	}
	f.saturate("a@example.com", 1)

	response := f.send(t, sessionID)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failing over from the spill target; body=%s", response.Code, response.Body.String())
	}
	if got := f.lastUpstreamToken(); got != "tok-c@example.com" {
		t.Fatalf("served by %q, want c after b's quota 429", got)
	}
	if assignment, _ := f.store.Get("claude", sessionID); assignment.AccountID != "a@example.com" {
		t.Fatalf("session moved to %q; a spilled request must leave it on its sticky account", assignment.AccountID)
	}
}
