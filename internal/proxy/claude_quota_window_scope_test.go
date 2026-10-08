package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestClaudeQuotaRejectionIsScopedToRequestedModel(t *testing.T) {
	fableOnly := http.Header{}
	fableOnly.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	fableOnly.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	fableOnly.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	fableOnly.Set("Anthropic-Ratelimit-Unified-7d_Oi-Status", "rejected")

	if !claudeResponseRejectedForPool(fableOnly, agentclaude.FableFeature) {
		t.Fatal("Fable request did not recognize its exhausted 7d_oi bucket")
	}
	for _, model := range []string{agentclaude.OpusFeature, agentclaude.SonnetFeature, "claude-haiku-4-5"} {
		if claudeResponseRejectedForPool(fableOnly, model) {
			t.Fatalf("healthy %s turn was rejected by an unrelated Fable bucket", model)
		}
		if claudeAccountExhaustedByResponseForPool(http.StatusOK, fableOnly, model) {
			t.Fatalf("healthy %s turn marked exhausted by Fable bucket", model)
		}
	}

	opusOnly := http.Header{}
	opusOnly.Set("Anthropic-Ratelimit-Unified-7d_Opus-Status", "rejected")
	if !claudeResponseRejectedForPool(opusOnly, "claude-opus-4-8[1m]") {
		t.Fatal("versioned Opus request missed its model-specific rejection")
	}
	if claudeResponseRejectedForPool(opusOnly, agentclaude.SonnetFeature) {
		t.Fatal("Opus-only rejection disabled Sonnet")
	}
	sonnetOnly := http.Header{}
	sonnetOnly.Set("Anthropic-Ratelimit-Unified-7d_Sonnet-Status", "rejected")
	if !claudeResponseRejectedForPool(sonnetOnly, "claude-sonnet-5") {
		t.Fatal("versioned Sonnet request missed its model-specific rejection")
	}
	if claudeResponseRejectedForPool(sonnetOnly, agentclaude.OpusFeature) {
		t.Fatal("Sonnet-only rejection disabled Opus")
	}

	accountWide := http.Header{}
	accountWide.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
	for _, model := range []string{agentclaude.FableFeature, agentclaude.OpusFeature, agentclaude.SonnetFeature} {
		if !claudeResponseRejectedForPool(accountWide, model) {
			t.Fatalf("account-wide weekly rejection failed to block %s", model)
		}
	}
}

func TestClaudeQuotaExpiryUsesLatestBindingWindowReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	header := http.Header{}
	header.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
	header.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(now.Add(30*time.Minute).Unix(), 10))
	header.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
	header.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(now.Add(4*time.Hour).Unix(), 10))
	if got, want := claudeExhaustionExpiry(header, now), now.Add(4*time.Hour); !got.Equal(want) {
		t.Fatalf("both binding windows: got %v, want latest %v", got, want)
	}

	fable := http.Header{}
	fable.Set("Anthropic-Ratelimit-Unified-7d_Oi-Status", "rejected")
	fable.Set("Anthropic-Ratelimit-Unified-7d_Oi-Reset", strconv.FormatInt(now.Add(3*time.Hour).Unix(), 10))
	fable.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	fable.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	// The aggregate reset belongs to a healthy window, not the exhausted
	// Fable-only bucket; it must not extend Fable's real recovery clock.
	fable.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(now.Add(7*time.Hour).Unix(), 10))
	if got := claudeExhaustionExpiryForPool(fable, now, agentclaude.FableFeature); !got.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("Fable-specific reset = %v, want +3h", got)
	}
	if got := claudeExhaustionExpiryForPool(fable, now, agentclaude.OpusFeature); !got.Equal(now.Add(selectacct.DefaultExhaustedTTL)) {
		t.Fatalf("unrelated Fable deadline affected Opus: %v", got)
	}

	opus := http.Header{}
	opus.Set("Anthropic-Ratelimit-Unified-7d_Opus-Status", "rejected")
	opus.Set("Anthropic-Ratelimit-Unified-7d_Opus-Reset", strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10))
	if got := claudeExhaustionExpiryForPool(opus, now, agentclaude.OpusFeature); !got.Equal(now.Add(5 * time.Hour)) {
		t.Fatalf("Opus-specific reset = %v, want +5h", got)
	}
	opus.Set("Retry-After", "21600")
	if got := claudeExhaustionExpiryForPool(opus, now, agentclaude.OpusFeature); !got.Equal(now.Add(6 * time.Hour)) {
		t.Fatalf("longer provider Retry-After was ignored: %v", got)
	}
}

func TestHealthyOpusResponseDoesNotFailOverOnFableRejection(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "healthy-opus", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	calls := 0
	transport := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		calls++
		headers := http.Header{}
		headers.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		headers.Set("Anthropic-Ratelimit-Unified-7d_Oi-Status", "rejected")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     headers,
			Body:       io.NopCloser(strings.NewReader(`{"id":"opus-answered"}`)),
			Request:    req,
		}
	}}
	proxy := usageLimitRetryTransport{
		base:        transport,
		server:      &server,
		provider:    accounts.ProviderClaude,
		agent:       "claude",
		session:     "healthy-opus",
		account:     "cooked@example.com",
		method:      http.MethodPost,
		path:        "/v1/messages",
		poolModel:   agentclaude.OpusFeature,
		maxAttempts: 3,
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewBufferString(`{"model":"claude-opus-4-8"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := proxy.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || calls != 1 {
		t.Fatalf("healthy Opus response=%d upstream attempts=%d, want 200 with one attempt", response.StatusCode, calls)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Contains(body, []byte("opus-answered")) {
		t.Fatalf("Opus output lost after unrelated Fable quota rejection: %q, err=%v", body, err)
	}
}

func TestClaudeProviderResetBecomesModelScopedSchedulerHold(t *testing.T) {
	now := time.Now().UTC()
	account := accounts.Account{ID: "account", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}
	server := Server{SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: account.ID, Provider: accounts.ProviderClaude, Headroom: 0.8, ShortHeadroom: 0.8,
	}}))}
	fableReset := now.Add(3 * time.Hour).Truncate(time.Second)
	fable := http.Header{}
	fable.Set("Anthropic-Ratelimit-Unified-7d_Oi-Status", "rejected")
	fable.Set("Anthropic-Ratelimit-Unified-7d_Oi-Reset", strconv.FormatInt(fableReset.Unix(), 10))
	server.markAccountExhaustedFromResponseForAccount(account, agentclaude.FableFeature, http.StatusTooManyRequests, fable)
	until, blocked := server.SchedulerRef.ExplicitBlockedUntilFor(accounts.ProviderClaude, account.ID, agentclaude.FableFeature, now)
	if !blocked || !until.Equal(fableReset) {
		t.Fatalf("Fable hold=(%s,%v), want provider reset %s", until, blocked, fableReset)
	}
	if until, blocked := server.SchedulerRef.ExplicitBlockedUntilFor(accounts.ProviderClaude, account.ID, agentclaude.OpusFeature, now); blocked {
		t.Fatalf("Fable quota incorrectly blocks Opus until %s", until)
	}
	weeklyReset := now.Add(4 * time.Hour).Truncate(time.Second)
	weekly := http.Header{}
	weekly.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
	weekly.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(weeklyReset.Unix(), 10))
	server.markAccountExhaustedFromResponseForAccount(account, agentclaude.OpusFeature, http.StatusTooManyRequests, weekly)
	for _, model := range []string{agentclaude.FableFeature, agentclaude.OpusFeature, agentclaude.SonnetFeature} {
		until, blocked := server.SchedulerRef.ExplicitBlockedUntilFor(accounts.ProviderClaude, account.ID, model, now)
		if !blocked || !until.Equal(weeklyReset) {
			t.Fatalf("account-wide hold for %s = (%s,%v), want %s", model, until, blocked, weeklyReset)
		}
	}
}

func TestClaudeWeeklyUtilizationDoesNotTreatPercentAsFraction(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"0", false},
		{"0.81", false},
		{"81", false},
		{"99.9", false},
		{"1", true},
		{"1.0", true},
		{"100", true},
		{"NaN", false},
		{"-1", false},
		{"101", false},
	}
	for _, tc := range tests {
		header := http.Header{}
		header.Set("Anthropic-Ratelimit-Unified-7d-Utilization", tc.raw)
		if got := claudeResponseCooksWeeklyWindow(header); got != tc.want {
			t.Errorf("weekly raw utilization %q => cooked %t, want %t", tc.raw, got, tc.want)
		}
	}
}
