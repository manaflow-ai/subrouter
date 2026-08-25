package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// The outbound transport streams responses, so it must not have an overall
// deadline — but the wait for response headers has to be bounded, or an
// upstream that accepts a request and goes silent holds the client request,
// its goroutine, and its upload-limiter slot forever.
func TestOutboundTransportBoundsResponseHeaderWait(t *testing.T) {
	transport := NewOutboundTransport()
	if transport.ResponseHeaderTimeout <= 0 {
		t.Fatal("outbound transport has no ResponseHeaderTimeout; a header blackhole hangs requests forever")
	}
}

// Ordinary-sized POSTs must not contend for the upload limiter: a slot spans
// the whole nested retry chain (failover, backoff sleeps, the Bedrock
// commit-window peek, header wait), so gating every POST serialized all
// message traffic proxy-wide behind 4 slots.
func TestSmallPostsBypassUploadLimiter(t *testing.T) {
	limiter := make(chan struct{}, 1)
	limiter <- struct{}{} // limiter fully occupied

	served := false
	transport := replayablePostRetryTransport{
		base: bedrockRoundTripFunc(func(*http.Request) (*http.Response, error) {
			served = true
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		}),
		maxAttempts: 1,
		limiter:     limiter,
	}

	small, err := http.NewRequest(http.MethodPost, "https://example.invalid/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	small.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{}`)), nil }
	response, err := transport.RoundTrip(small)
	if err != nil {
		t.Fatalf("small POST should bypass a full limiter, got %v", err)
	}
	response.Body.Close()
	if !served {
		t.Fatal("small POST never reached the base transport")
	}

	bigBody := strings.Repeat("x", replayablePostLimiterMinBytes)
	big, err := http.NewRequest(http.MethodPost, "https://example.invalid/v1/messages", strings.NewReader(bigBody))
	if err != nil {
		t.Fatal(err)
	}
	big.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(bigBody)), nil }
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := transport.RoundTrip(big.WithContext(ctx)); err == nil {
		t.Fatal("large POST should wait on the full limiter and fail on context timeout")
	}
}

// A stream that stays alive but never produces a decisive frame (pings,
// unparseable frames) must not buffer forever while the client has received
// no response headers at all.
func TestBedrockPeekForcesCommitOnDeadline(t *testing.T) {
	saved := claudeFableBedrockPeekForceCommitAfter
	claudeFableBedrockPeekForceCommitAfter = 30 * time.Millisecond
	defer func() { claudeFableBedrockPeekForceCommitAfter = saved }()

	ping := buildEventStreamFrame(t, `{"type":"ping"}`)
	pr, pw := io.Pipe()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				pw.Close()
				return
			case <-time.After(5 * time.Millisecond):
				if _, err := pw.Write(ping); err != nil {
					return
				}
			}
		}
	}()

	peek := peekBedrockStreamUntilCommit(pr)
	if peek.outcome != bedrockPeekCommit {
		t.Fatalf("outcome = %v, want commit forced by deadline", peek.outcome)
	}
	if peek.commitReason != "forced_deadline" {
		t.Fatalf("commitReason = %q, want forced_deadline", peek.commitReason)
	}
}

func TestBedrockPeekForcesCommitOnBufferCap(t *testing.T) {
	saved := claudeFableBedrockPeekMaxBytes
	claudeFableBedrockPeekMaxBytes = 256
	defer func() { claudeFableBedrockPeekMaxBytes = saved }()

	ping := buildEventStreamFrame(t, `{"type":"ping"}`)
	var stream []byte
	for len(stream) < 4096 {
		stream = append(stream, ping...)
	}
	peek := peekBedrockStreamUntilCommit(bytes.NewReader(stream))
	if peek.outcome != bedrockPeekCommit {
		t.Fatalf("outcome = %v, want commit forced by buffer cap", peek.outcome)
	}
	if peek.commitReason != "forced_buffer_cap" {
		t.Fatalf("commitReason = %q, want forced_buffer_cap", peek.commitReason)
	}
}

// Unknown (future) delta types are invisible like thinking deltas and must be
// gated by the commit window, not commit the stream instantly — an instant
// commit reintroduces the unreplayable post-commit shed the window absorbs.
func TestBedrockFrameDecisionGatesUnknownDeltaTypes(t *testing.T) {
	frame := bedrockFrame{payload: []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"redacted_thinking_delta"}}`)}
	if decisive, _, _ := bedrockFrameDecision(frame, 0); decisive {
		t.Fatal("unknown delta type committed inside the window")
	}
	decisive, failure, reason := bedrockFrameDecision(frame, claudeFableBedrockCommitWindow)
	if !decisive || failure {
		t.Fatalf("unknown delta type after window: decisive=%v failure=%v", decisive, failure)
	}
	if !strings.Contains(reason, "window_expired") {
		t.Fatalf("reason = %q, want window_expired_*", reason)
	}
	if !bedrockFrameIsThinkingDelta(frame) {
		t.Fatal("unknown invisible delta must anchor the commit window")
	}
}

// The gateway streaming usage writer must carry the cache_creation TTL split;
// without it 1h cache writes are priced at the 5m rate.
func TestBedrockStreamUsageWriterCarriesCacheCreationSplit(t *testing.T) {
	start := buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":25,"ephemeral_1h_input_tokens":75}}}}`)
	stop := buildEventStreamFrame(t, `{"type":"message_delta","usage":{"output_tokens":5}}`)

	p := newBedrockStreamUsageWriter()
	p.Write(append(append([]byte{}, start...), stop...))
	usage, ok := p.Usage()
	if !ok {
		t.Fatal("expected usage extracted")
	}
	if usage.CacheCreation.Ephemeral5m != 25 || usage.CacheCreation.Ephemeral1h != 75 {
		t.Fatalf("cache creation split = %+v, want 25/75", usage.CacheCreation)
	}
}

// A panic inside the score refresh must release the refresh claim; before,
// the swallowed panic left refreshing=true forever and usage scores froze
// for the life of the process.
func TestUsageRefreshReleasesClaimOnPanic(t *testing.T) {
	accountStore := accounts.CodexStore{Dir: t.TempDir()}
	if err := accountStore.SaveStored(proxyStoredOAuthAccount("acct@example.com", "acct", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	loaded, err := accountStore.List()
	if err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	schedulerRef.SetUpdatedAt(time.Time{})
	server := Server{
		AccountRef:    NewAccountRef(accountStore, loaded, nil),
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: time.Nanosecond,
		ScoreAccounts: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			panic("boom")
		},
	}
	func() {
		defer func() { _ = recover() }()
		server.refreshUsageScoresIfStale(context.Background())
	}()

	refreshed := false
	server.ScoreAccounts = func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
		refreshed = true
		return []selectacct.Score{{AccountID: "acct@example.com", Headroom: 0.5, ShortHeadroom: 0.5}}, 1
	}
	schedulerRef.SetUpdatedAt(time.Time{})
	server.refreshUsageScoresIfStale(context.Background())
	if !refreshed {
		t.Fatal("refresh claim was never released after a panic; usage scores are frozen")
	}
}

// With the whole Claude OAuth pool exhausted, a sticky session must fail
// selection directly (routing to the fallback chain) instead of logging a
// "rerouting" to another exhausted account that is never persisted — that
// false log repeated once per request for as long as the pool stayed cooked.
func TestClaudeExhaustedPoolStickyFailsWithoutRerouteLog(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "session-1", "a@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	server := Server{
		Accounts: []accounts.Account{
			{ID: "a@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-a"},
			{ID: "b@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-b"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "a@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
			{AccountID: "b@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
		})),
		Logger:       slog.New(slog.NewTextHandler(&logBuf, nil)),
		MaxBodyBytes: 1024,
	}
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "session-1")
	if _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "session-1", req); err == nil {
		t.Fatal("expected selection to fail with the whole pool exhausted")
	}
	if strings.Contains(logBuf.String(), "rerouting cold sticky session") {
		t.Fatalf("logged a reroute that never happens:\n%s", logBuf.String())
	}
	assignment, ok := store.Get("claude", "session-1")
	if !ok || assignment.AccountID != "a@example.com" {
		t.Fatalf("sticky assignment = %+v, want unchanged a@example.com", assignment)
	}
}
