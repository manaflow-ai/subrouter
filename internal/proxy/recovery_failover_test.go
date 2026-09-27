package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
	"github.com/manaflow-ai/subrouter/wake"
)

func TestQuotaAlarmRequiresTerminalPoolFailure(t *testing.T) {
	for _, alternateSucceeds := range []bool{true, false} {
		name := "all exhausted"
		if alternateSucceeds {
			name = "alternate succeeds"
		}
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if alternateSucceeds && r.Header.Get("Authorization") == "Bearer second-token" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: response.completed\ndata: {}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`)
			}))
			defer upstream.Close()
			sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sessions.Put("codex", "wake-test", "first", ""); err != nil {
				t.Fatal(err)
			}
			tracker := NewRecoveryTracker()
			handler := Server{
				Upstream: mustParseURL(t, upstream.URL),
				Accounts: []accounts.Account{
					{ID: "first", AuthMode: accounts.AuthModeOAuth, Token: "first-token"},
					{ID: "second", AuthMode: accounts.AuthModeOAuth, Token: "second-token"},
				},
				Sessions: sessions, Recovery: tracker, MaxBodyBytes: 1 << 20,
				SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
					{AccountID: "first", Headroom: .8, ShortHeadroom: .8},
					{AccountID: "second", Headroom: .8, ShortHeadroom: .8},
				})),
			}.Handler()
			proxyServer := httptest.NewServer(handler)
			defer proxyServer.Close()
			req, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"hello"}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Subrouter-Session", "wake-test")
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			states := tracker.List(time.Now())
			quotaFailures := 0
			for _, state := range states {
				if state.Kind == wake.KindCodexQuota {
					quotaFailures++
				}
			}
			if alternateSucceeds && (resp.StatusCode != http.StatusOK || quotaFailures != 0) {
				t.Fatalf("successful failover: status=%d quota failures=%d", resp.StatusCode, quotaFailures)
			}
			if !alternateSucceeds && (resp.StatusCode != http.StatusTooManyRequests || quotaFailures != 1) {
				t.Fatalf("exhausted pool: status=%d quota failures=%d", resp.StatusCode, quotaFailures)
			}
		})
	}
}

func TestRecoverySuccessRequiresCompletedStream(t *testing.T) {
	for _, test := range []struct {
		name, stream string
		wantSuccess  bool
	}{
		{"failed", "event: response.failed\ndata: {}\n\n", false},
		{"completed", "event: response.completed\ndata: {}\n\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := NewRecoveryTracker()
			response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(test.stream))}
			wrapRecoverySuccess(response, tracker, "codex", "stream")
			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			states := tracker.List(time.Now())
			gotSuccess := len(states) == 1 && !states[0].LastSuccessAt.IsZero()
			if gotSuccess != test.wantSuccess {
				t.Fatalf("recorded success=%t, want %t", gotSuccess, test.wantSuccess)
			}
		})
	}
}

func TestTerminalQuotaOutcomeIgnoresSuccessfulOuterFallback(t *testing.T) {
	tracker := NewRecoveryTracker()
	quota := &http.Response{StatusCode: http.StatusTooManyRequests}
	fallback := &http.Response{StatusCode: http.StatusOK}
	outcome := terminalQuotaOutcome{response: quota, kind: wake.KindCodexQuota, resetAt: time.Now().Add(time.Hour)}
	outcome.recordIfDelivered(fallback, tracker, "codex", "fallback")
	if got := tracker.List(time.Now()); len(got) != 0 {
		t.Fatalf("successful fallback left quota alarm evidence: %+v", got)
	}
	outcome.recordIfDelivered(quota, tracker, "codex", "exhausted")
	if got := tracker.List(time.Now()); len(got) != 1 || got[0].Kind != wake.KindCodexQuota {
		t.Fatalf("terminal quota evidence missing: %+v", got)
	}
}
