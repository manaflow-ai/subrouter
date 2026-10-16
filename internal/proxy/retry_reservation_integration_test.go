package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestConcurrentTerminalRefreshFallbackReservesBeforeRefresh(t *testing.T) {
	server := overloadReservationServer(t, accounts.ProviderCodex, 3)
	rejected := server.Accounts[0]
	const requests = 50
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	arrived := make(chan string, requests)
	release := make(chan struct{})
	server.RefreshAccountFn = func(ctx context.Context, account accounts.Account) (accounts.Account, error) {
		if account.ID == rejected.ID {
			return account, errors.New("invalid_grant")
		}
		arrived <- account.ID
		select {
		case <-release:
			return account, nil
		case <-ctx.Done():
			return account, ctx.Err()
		}
	}
	type result struct {
		account     accounts.Account
		reservation *selectacct.PlacementReservation
		err         error
	}
	results := make(chan result, requests)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", nil).WithContext(ctx)
			var reservation *selectacct.PlacementReservation
			account, _, err := server.refreshSelectedAccountWithReservation(ctx, accounts.ProviderCodex, "codex", fmt.Sprintf("refresh-%d", i), "", request, rejected, &reservation)
			results <- result{account, reservation, err}
		}()
	}
	counts := map[string]int{}
	for range requests {
		select {
		case id := <-arrived:
			counts[id]++
		case <-ctx.Done():
			close(release)
			wg.Wait()
			t.Fatal("replacement refreshes did not reach barrier")
		}
	}
	debits := server.SchedulerRef.LiveDebits()
	for _, account := range server.Accounts[1:] {
		if counts[account.ID] == 0 {
			t.Errorf("replacement refresh burst did not use %q: %v", account.ID, counts)
		}
		if got := debits[selectacct.ScoreKey(account.Provider, account.ID)]; got != counts[account.ID] {
			t.Errorf("pending load for %q = %d before refresh completes, want %d", account.ID, got, counts[account.ID])
		}
	}
	if got := debits[selectacct.ScoreKey(rejected.Provider, rejected.ID)]; got != 0 {
		t.Errorf("rejected account retains debit = %d", got)
	}
	close(release)
	wg.Wait()
	close(results)
	for result := range results {
		if result.err != nil {
			t.Errorf("refresh replacement: %v", result.err)
		}
		if result.reservation == nil {
			t.Errorf("replacement %q has no reservation", result.account.ID)
		}
		result.reservation.Release()
	}
	requireOverloadDebits(t, &server, 0)
}

func TestTerminalRefreshFallbackReleasesFailedPreparation(t *testing.T) {
	server := overloadReservationServer(t, accounts.ProviderCodex, 2)
	rejected := server.Accounts[0]
	_, reservation, err := pickAndReserveRoutingAccount(t.Context(), server.scheduler(), server.SchedulerRef, []accounts.Account{rejected})
	if err != nil {
		t.Fatal(err)
	}
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		if account.ID == rejected.ID {
			return account, errors.New("invalid_grant")
		}
		requireOverloadDebits(t, &server, 1)
		return account, errors.New("temporary refresh failure")
	}
	request := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", nil)
	_, _, err = server.refreshSelectedAccountWithReservation(t.Context(), accounts.ProviderCodex, "codex", "refresh", "", request, rejected, &reservation)
	if err == nil {
		t.Fatal("failed preparation unexpectedly succeeded")
	}
	if reservation != nil {
		t.Fatal("failed replacement returned a pending reservation")
	}
	requireOverloadDebits(t, &server, 0)
}

func TestModelIncompatibilityReservationBeforeRefresh(t *testing.T) {
	server := overloadReservationServer(t, accounts.ProviderCodex, 2)
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		requireOverloadDebits(t, &server, 1)
		return account, nil
	}
	_, reservation, err := server.rerouteModelIncompatibilityWithReservation(t.Context(), accounts.ProviderCodex, "codex", "compatibility", "", server.Accounts[0].ID, "test-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reservation == nil {
		t.Fatal("HTTP compatibility replacement has no reservation")
	}
	reservation.Release()
	requireOverloadDebits(t, &server, 0)
}

func TestReconnectModelIncompatibilityDoesNotDebitDispatch(t *testing.T) {
	server := overloadReservationServer(t, accounts.ProviderCodex, 2)
	initial := server.Accounts[0].ID
	if _, err := server.Sessions.Put("codex", "reconnect", initial, ""); err != nil {
		t.Fatal(err)
	}
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		requireOverloadDebits(t, &server, 1)
		return account, nil
	}
	if err := server.rerouteModelIncompatibilityForReconnect(t.Context(), accounts.ProviderCodex, "codex", "reconnect", "", initial, "test-model"); err != nil {
		t.Fatal(err)
	}
	assignment, _ := server.Sessions.Get("codex", "reconnect")
	if assignment.AccountID == initial {
		t.Fatal("reconnect did not move incompatible assignment")
	}
	requireOverloadDebits(t, &server, 0)
}

func TestClaudeOverloadDispatchReservationLifecycle(t *testing.T) {
	for _, replayFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(replayFailure), func(t *testing.T) {
			server := overloadReservationServer(t, accounts.ProviderClaude, 2)
			server.ClaudeOverloadReroute = true
			initial := server.Accounts[0]
			request := claudeOverloadRequest(initial.Token)
			server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
				requireOverloadDebits(t, &server, 1)
				if replayFailure {
					request.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("body unavailable") }
				}
				return account, nil
			}
			initialCalls, replacementCalls := 0, 0
			var waits []time.Duration
			transport := claudeOverloadTransport(&server, "overload-dispatch", initial.ID, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") == initial.AuthorizationHeader() {
					initialCalls++
					return claudeOverloaded529(nil), nil
				}
				replacementCalls++
				requireOverloadDebits(t, &server, 1)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
			}), &waits)
			transport.maxAttempts = 5
			response, err := transport.RoundTrip(request)
			if err != nil && !replayFailure {
				t.Fatal(err)
			}
			if response != nil {
				response.Body.Close()
			}
			if replayFailure {
				if replacementCalls != 0 {
					t.Fatalf("failed replay dispatched %d replacements", replacementCalls)
				}
				requireOverloadDebits(t, &server, 0)
			} else {
				if replacementCalls != 1 || initialCalls != 1+providerOverloadMaxRetries {
					t.Fatalf("initial=%d replacement=%d, want3 and1", initialCalls, replacementCalls)
				}
				requireOverloadDebits(t, &server, 1)
			}
		})
	}
}
