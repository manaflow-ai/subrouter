package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func overloadReservationServer(t *testing.T, provider accounts.Provider, count int) Server {
	t.Helper()
	upstream, _ := url.Parse("http://example.invalid")
	server := codexOverloadServer(t, upstream, count, true)
	var scores []selectacct.Score
	for i := range server.Accounts {
		server.Accounts[i].Provider = provider
		scores = append(scores, selectacct.Score{
			AccountID: server.Accounts[i].ID, Provider: provider,
			Headroom: 0.9, ShortHeadroom: 0.9,
		})
	}
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(scores))
	return server
}

func requireOverloadDebits(t *testing.T, server *Server, want int) {
	t.Helper()
	total := 0
	for _, count := range server.SchedulerRef.LiveDebits() {
		total += count
	}
	if total != want {
		t.Fatalf("live debits = %v, want total %d", server.SchedulerRef.LiveDebits(), want)
	}
}

func TestClaudeOverloadCandidateReservationLifecycle(t *testing.T) {
	for _, failure := range []string{"none", "refresh", "headroom", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			server := overloadReservationServer(t, accounts.ProviderClaude, 2)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "refresh" {
				server.RefreshAccountFn = func(context.Context, accounts.Account) (accounts.Account, error) {
					requireOverloadDebits(t, &server, 1)
					return accounts.Account{}, errors.New("temporary refresh failure")
				}
			}
			if failure == "headroom" {
				server.SchedulerRef.Set(selectacct.NewScheduler([]selectacct.Score{
					{AccountID: server.Accounts[1].ID, Provider: accounts.ProviderClaude, Headroom: 0.01, ShortHeadroom: 0.01},
				}))
			}
			transport := usageLimitRetryTransport{server: &server, provider: accounts.ProviderClaude, agent: "claude", session: "overload"}
			_, reservation, ok := transport.claudeOverloadRerouteCandidateWithReservation(ctx, map[string]struct{}{server.Accounts[0].ID: {}})
			defer reservation.Release()
			if failure == "refresh" || failure == "headroom" {
				if ok || reservation != nil {
					t.Fatalf("rejected candidate returned ok=%v, reservation=%v", ok, reservation)
				}
				requireOverloadDebits(t, &server, 0)
				return
			}
			if !ok || reservation == nil {
				t.Fatal("usable overload candidate has no reservation")
			}
			requireOverloadDebits(t, &server, 1)
			if failure == "cancel" {
				cancel()
				// Release is also the synchronous caller cleanup at abandonment.
				reservation.Release()
				requireOverloadDebits(t, &server, 0)
				return
			}
			reservation.Commit()
			reservation.Release()
			requireOverloadDebits(t, &server, 1)
		})
	}
}

func TestCodexOverloadPlansReserveBeforeRefresh(t *testing.T) {
	server := overloadReservationServer(t, accounts.ProviderCodex, 3)
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		requireOverloadDebits(t, &server, 1)
		return account, nil
	}
	transport := codexOverloadFailoverTransport{server: &server, agent: "codex", session: "overload", attempt: &upstreamAttempt{}}
	left, switched := 0, 0
	plan, ok := transport.planDefaultRetry(t.Context(), server.Accounts[0].ID, "capacity", map[string]struct{}{server.Accounts[0].ID: {}}, &left, &switched, 3, time.Now().Add(time.Minute))
	defer plan.reservation.Release()
	if !ok || plan.reservation == nil || plan.next == nil {
		t.Fatalf("switch plan = %+v, planned=%v", plan, ok)
	}
	plan.reservation.Release()
	requireOverloadDebits(t, &server, 0)
}

func TestCodexOverloadPersistSameAccountReleasesReservation(t *testing.T) {
	server := overloadReservationServer(t, accounts.ProviderCodex, 1)
	transport := codexOverloadFailoverTransport{server: &server, agent: "codex", session: "overload"}
	plan, ok := transport.planPersistRetry(t.Context(), server.Accounts[0].ID, map[string]struct{}{server.Accounts[0].ID: {}}, time.Now().Add(time.Minute), false)
	defer plan.reservation.Release()
	if !ok || plan.next != nil || plan.reservation != nil {
		t.Fatalf("same-account persist plan = %+v, planned=%v", plan, ok)
	}
	requireOverloadDebits(t, &server, 0)
}

func TestCodexOverloadReservationReleasedBeforeDispatch(t *testing.T) {
	for _, failure := range []string{"cancel", "replay"} {
		t.Run(failure, func(t *testing.T) {
			server := overloadReservationServer(t, accounts.ProviderCodex, 2)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.invalid/responses", strings.NewReader(`{"input":[]}`))
			calls, waits := 0, 0
			transport := codexOverloadFailoverTransport{
				server: &server, account: server.Accounts[0].ID, agent: "codex", session: "overload",
				base: roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("busy"))}, nil
				}),
				sleep: func(context.Context, time.Duration) bool {
					waits++
					if waits == 1 { // Same-account retry does not reserve.
						requireOverloadDebits(t, &server, 0)
						return true
					}
					requireOverloadDebits(t, &server, 1)
					if failure == "cancel" {
						cancel()
						return false
					}
					req.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("body unavailable") }
					return true
				},
			}
			response, err := transport.RoundTrip(req)
			if response != nil {
				response.Body.Close()
			}
			if err != nil || calls != 2 || waits != 2 {
				t.Fatalf("calls=%d waits=%d err=%v, want abandonment before third request", calls, waits, err)
			}
			requireOverloadDebits(t, &server, 0)
		})
	}
}

func TestCodexOverloadReservationCommitsOnceAtDispatch(t *testing.T) {
	server := overloadReservationServer(t, accounts.ProviderCodex, 2)
	initial := server.Accounts[0].ID
	if _, err := server.Sessions.Put("codex", "overload", initial, ""); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.invalid/responses", strings.NewReader(`{"input":[]}`))
	calls := 0
	transport := codexOverloadFailoverTransport{
		server: &server, account: initial, agent: "codex", session: "overload",
		sleep: func(context.Context, time.Duration) bool { return true },
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls <= 2 {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("busy"))}, nil
			}
			// Dispatch commits offered load, even though the reply has not
			// completed and the sticky assignment must still be unchanged.
			requireOverloadDebits(t, &server, 1)
			assignment, _ := server.Sessions.Get("codex", "overload")
			if assignment.AccountID != initial {
				t.Fatalf("sticky account moved before response completed: %s", assignment.AccountID)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output":[]}`))}, nil
		}),
	}
	response, err := transport.RoundTrip(req)
	if err != nil || response == nil {
		t.Fatalf("RoundTrip response=%v err=%v", response, err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	assignment, _ := server.Sessions.Get("codex", "overload")
	if assignment.AccountID == initial {
		t.Fatal("completed replacement response did not move sticky account")
	}
	requireOverloadDebits(t, &server, 1)
	if calls != 3 {
		t.Fatalf("upstream calls = %d, want 3", calls)
	}
}
