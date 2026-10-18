package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func tenantLeaseReservationDebits(server *Server) int {
	total := 0
	for _, count := range server.SchedulerRef.LiveDebits() {
		total += count
	}
	return total
}

func TestTenantCredentialLeaseConcurrentBurstReservesBeforeRefresh(t *testing.T) {
	for _, provider := range []accounts.Provider{accounts.ProviderCodex, accounts.ProviderClaude} {
		t.Run(string(provider), func(t *testing.T) {
			const requests, accountCount = 50, 7
			var available []accounts.Account
			var scores []selectacct.Score
			for i := 0; i < accountCount; i++ {
				account := tenantLeaseTestAccount(fmt.Sprintf("account-%d", i), provider)
				available = append(available, account)
				// Constrained but not exhausted accounts exercise deterministic
				// argmax routing. With a stale snapshot and no reservations all
				// 50 requests would choose account-0 before any refresh finishes.
				scores = append(scores, selectacct.Score{
					AccountID: account.ID, Provider: provider,
					Headroom: 0.30, ShortHeadroom: 0.30,
				})
			}
			server := tenantLeaseTestServer(available...)
			server.SchedulerRef.Set(selectacct.NewScheduler(scores))
			store := newTenantCredentialLeaseStore()
			started := make(chan string, requests)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			server.RefreshAccountFn = func(ctx context.Context, account accounts.Account) (accounts.Account, error) {
				started <- account.ID
				select {
				case <-release:
					return account, nil
				case <-ctx.Done():
					return accounts.Account{}, ctx.Err()
				}
			}
			responses := make(chan *httptest.ResponseRecorder, requests)
			var wg sync.WaitGroup
			for i := 0; i < requests; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/_subrouter/leases", strings.NewReader(fmt.Sprintf(
						`{"provider":%q,"sessionId":"burst-%d"}`, provider, i,
					)))
					response := httptest.NewRecorder()
					store.handleIssue(server, tenant.Tenant{ID: "team"}, response, request)
					responses <- response
				}(i)
			}
			counts := make(map[string]int)
			deadline := time.NewTimer(10 * time.Second)
			defer deadline.Stop()
			for i := 0; i < requests; i++ {
				select {
				case id := <-started:
					counts[id]++
				case <-deadline.C:
					t.Fatalf("only %d refreshes started", i)
				}
			}
			if got := tenantLeaseReservationDebits(server); got != requests {
				t.Fatalf("pending live debits = %d, want %d", got, requests)
			}
			for _, account := range available {
				if got := counts[account.ID]; got < 7 || got > 8 {
					t.Errorf("%s got %d/%d concurrent placements, want 7 or 8: %v", account.ID, got, requests, counts)
				}
			}
			store.mu.Lock()
			beforePublication := len(store.leases)
			store.mu.Unlock()
			if beforePublication != 0 {
				t.Fatalf("published %d leases before refresh completed", beforePublication)
			}
			unblock()
			wg.Wait()
			close(responses)
			for response := range responses {
				if response.Code != http.StatusOK {
					t.Errorf("issue status = %d: %s", response.Code, response.Body.String())
				}
			}
			store.mu.Lock()
			published := len(store.leases)
			store.mu.Unlock()
			if published != requests || tenantLeaseReservationDebits(server) != requests {
				t.Fatalf("published=%d live debits=%d, want %d of each with no duplicate debit", published, tenantLeaseReservationDebits(server), requests)
			}
		})
	}
}

func TestTenantCredentialLeaseReservationFailureCleanup(t *testing.T) {
	for _, failure := range []string{"refresh", "empty_token", "cancel", "session_write", "publication"} {
		t.Run(failure, func(t *testing.T) {
			account := tenantLeaseTestAccount("account", accounts.ProviderCodex)
			server := tenantLeaseTestServer(account)
			store := newTenantCredentialLeaseStore()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server.RefreshAccountFn = func(_ context.Context, selected accounts.Account) (accounts.Account, error) {
				if got := tenantLeaseReservationDebits(server); got != 1 {
					t.Fatalf("pending debit during refresh = %d, want 1", got)
				}
				switch failure {
				case "refresh":
					return accounts.Account{}, errors.New("temporary refresh failure")
				case "empty_token":
					selected.Token = ""
				case "cancel":
					cancel()
					return accounts.Account{}, ctx.Err()
				case "publication":
					// A trusted hold arriving after selection must still win
					// the publication guard without converting the reservation.
					server.SchedulerRef.MarkAccountUnavailableUntil(account.Provider, account.ID, time.Now().Add(time.Minute))
				}
				return selected, nil
			}
			if failure == "session_write" {
				dir := filepath.Join(t.TempDir(), "sessions")
				sessions, err := session.NewStore(filepath.Join(dir, "sessions.json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(dir, "sessions.json.lock")); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
				server.Sessions = sessions
			}
			response := httptest.NewRecorder()
			store.handleIssue(server, tenant.Tenant{ID: "team"}, response,
				httptest.NewRequestWithContext(ctx, http.MethodPost, "/_subrouter/leases", strings.NewReader(`{"provider":"codex","sessionId":"failure"}`)),
			)
			wantStatus := http.StatusServiceUnavailable
			if failure == "session_write" {
				wantStatus = http.StatusInternalServerError
			}
			if response.Code != wantStatus {
				t.Fatalf("status=%d, want %d", response.Code, wantStatus)
			}
			if len(store.leases) != 0 || tenantLeaseReservationDebits(server) != 0 {
				t.Fatalf("failed issue published %d leases or leaked debits: %v", len(store.leases), server.SchedulerRef.LiveDebits())
			}
			if failure == "cancel" && server.scheduler().Exhausted(account.Provider, account.ID) {
				t.Fatal("request cancellation poisoned the account's quota state")
			}
		})
	}
}
