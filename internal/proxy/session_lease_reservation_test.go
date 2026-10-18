package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Pause each selector after it has captured the same scheduler snapshot. This
// exposes pick-then-reserve races without timing-dependent goroutine scheduling
// or changing the scheduler's random generator. API keys have deterministic
// ordering, so live reservations are the only reason the second account wins.
type leasePlacementSnapshotBarrier struct {
	arrived chan struct{}
	release chan struct{}
}

func (b *leasePlacementSnapshotBarrier) Enabled(context.Context, slog.Level) bool { return true }
func (b *leasePlacementSnapshotBarrier) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "model quota pool matched" {
		b.arrived <- struct{}{}
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	}
	return nil
}
func (b *leasePlacementSnapshotBarrier) WithAttrs([]slog.Attr) slog.Handler { return b }
func (b *leasePlacementSnapshotBarrier) WithGroup(string) slog.Handler      { return b }

func TestFreshLeasePlacementReservesAtomicallyAcrossConcurrentSnapshots(t *testing.T) {
	const placements = 50
	const model = "gemini-2.5-pro"
	candidates := []accounts.Account{
		{ID: "a", Provider: accounts.ProviderOpenRouter, AuthMode: accounts.AuthModeAPIKey, Token: "key-a"},
		{ID: "b", Provider: accounts.ProviderOpenRouter, AuthMode: accounts.AuthModeAPIKey, Token: "key-b"},
	}
	scores := make([]selectacct.Score, 0, len(candidates))
	for _, candidate := range candidates {
		score := selectacct.Score{AccountID: candidate.ID, Provider: candidate.Provider, Headroom: 1, ShortHeadroom: 1}
		score.ModelScores = map[string]selectacct.Score{selectacct.ModelKey(model): score}
		scores = append(scores, score)
	}
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(scores))
	barrier := &leasePlacementSnapshotBarrier{arrived: make(chan struct{}, placements), release: make(chan struct{})}
	server := Server{
		Accounts: candidates, Sessions: newSessionStore(t), SchedulerRef: ref,
		Logger: slog.New(barrier), MaxBodyBytes: 1024,
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	type result struct {
		account     accounts.Account
		reservation *selectacct.PlacementReservation
		err         error
	}
	results := make(chan result, placements)
	var wg sync.WaitGroup
	for i := range placements {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/internal/v1/session-leases", nil).WithContext(ctx)
			req.Header.Set("X-Subrouter-Model", model)
			var reservation *selectacct.PlacementReservation
			account, _, _, err := server.accountForSessionProviderWithOptions(accounts.ProviderOpenRouter, "pi", fmt.Sprintf("lease-%d", i), req, accountSelectionOptions{
				ignoreForcedAccount: true, placementReservation: &reservation,
			})
			results <- result{account: account, reservation: reservation, err: err}
		}()
	}
	for range placements {
		select {
		case <-barrier.arrived:
		case <-ctx.Done():
			close(barrier.release)
			wg.Wait()
			t.Fatal("concurrent selectors did not reach the snapshot barrier")
		}
	}
	close(barrier.release)
	wg.Wait()
	close(results)
	counts := map[string]int{}
	var reservations []*selectacct.PlacementReservation
	for result := range results {
		if result.err != nil {
			t.Errorf("select lease account: %v", result.err)
			continue
		}
		if result.reservation == nil {
			t.Error("fresh lease has no pending placement reservation")
		}
		counts[result.account.ID]++
		reservations = append(reservations, result.reservation)
	}
	defer func() {
		for _, reservation := range reservations {
			reservation.Release()
		}
	}()
	if counts["a"] != placements/2 || counts["b"] != placements/2 {
		t.Fatalf("concurrent fresh leases used stale selection load: %v, want 25 on each account", counts)
	}
	for id, count := range counts {
		if got := ref.LiveDebits()[selectacct.ScoreKey(accounts.ProviderOpenRouter, id)]; got != count {
			t.Errorf("pending debits for %q = %d, want %d", id, got, count)
		}
	}
	for _, reservation := range reservations {
		reservation.Release()
	}
	if got := ref.LiveDebits(); len(got) != 0 {
		t.Fatalf("released lease placements left live debits: %v", got)
	}
}

func TestSessionLeaseMovesReservationAfterTerminalRefreshFailure(t *testing.T) {
	const rejected = "rejected@example.com"
	const healthy = "healthy@example.com"
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: rejected, Provider: accounts.ProviderCodex, Headroom: 1, ShortHeadroom: 1},
		{AccountID: healthy, Provider: accounts.ProviderCodex, Headroom: 0.1, ShortHeadroom: 0.1},
	}))
	handler := Server{
		Accounts: []accounts.Account{
			{ID: rejected, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "rejected-token"},
			{ID: healthy, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "healthy-token"},
		},
		Sessions: newSessionStore(t), SchedulerRef: ref, sessionLeases: newSessionLeaseStore(),
		AdminToken: "service-admin-token", MaxBodyBytes: 1024,
		RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			if account.ID == rejected {
				return account, &accounts.CodexStoredRefreshFailureError{Failure: accounts.CodexRefreshFailure{
					StatusCode: http.StatusUnauthorized, ProviderCode: "refresh_token_reused",
				}}
			}
			return account, nil
		},
	}.Handler()
	lease, _ := issueSessionLease(t, handler, "codex", "gpt-5.4")
	if lease.Assignment.AccountID != healthy {
		t.Fatalf("lease account = %q, want %q", lease.Assignment.AccountID, healthy)
	}
	debits := ref.LiveDebits()
	if got := debits[selectacct.ScoreKey(accounts.ProviderCodex, rejected)]; got != 0 {
		t.Errorf("rejected account debit = %d, want 0", got)
	}
	if got := debits[selectacct.ScoreKey(accounts.ProviderCodex, healthy)]; got != 1 {
		t.Errorf("serving account debit = %d, want 1", got)
	}
}

func TestFreshLeasePaidFallbackReservesOnlyFundedAccount(t *testing.T) {
	const exhausted = "a-exhausted@example.com"
	const funded = "b-funded@example.com"
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: exhausted, Provider: accounts.ProviderClaude},
		{AccountID: funded, Provider: accounts.ProviderClaude,
			ClaudeExtraUsageEnabled: true, ClaudeExtraUsageKnown: true, ClaudeExtraUsageRemaining: 10},
	}))
	handler := Server{
		Accounts: []accounts.Account{
			{ID: exhausted, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "exhausted-token"},
			{ID: funded, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "funded-token"},
		},
		Sessions: newSessionStore(t), SchedulerRef: ref, sessionLeases: newSessionLeaseStore(),
		AdminToken: "service-admin-token", MaxBodyBytes: 1024,
	}.Handler()
	lease, _ := issueSessionLease(t, handler, "claude", "claude-opus-4-1")
	if lease.Assignment.AccountID != funded {
		t.Fatalf("paid fallback lease account = %q, want %q", lease.Assignment.AccountID, funded)
	}
	debits := ref.LiveDebits()
	if got := debits[selectacct.ScoreKey(accounts.ProviderClaude, exhausted)]; got != 0 {
		t.Errorf("exhausted account debit = %d, want 0", got)
	}
	if got := debits[selectacct.ScoreKey(accounts.ProviderClaude, funded)]; got != 1 {
		t.Errorf("funded account debit = %d, want 1", got)
	}
}
