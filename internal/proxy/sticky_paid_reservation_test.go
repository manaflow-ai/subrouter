package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

type placementReservationObserver func(slog.Record)

func (o placementReservationObserver) Enabled(context.Context, slog.Level) bool { return true }
func (o placementReservationObserver) Handle(_ context.Context, r slog.Record) error {
	o(r)
	return nil
}
func (o placementReservationObserver) WithAttrs([]slog.Attr) slog.Handler { return o }
func (o placementReservationObserver) WithGroup(string) slog.Handler      { return o }

func TestStickyPaidFallbackTransfersReservationBeforePublishingMove(t *testing.T) {
	const old = "a-exhausted@example.com"
	const funded = "b-funded@example.com"
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: old, Provider: accounts.ProviderClaude},
		{AccountID: funded, Provider: accounts.ProviderClaude,
			ClaudeExtraUsageEnabled: true, ClaudeExtraUsageKnown: true, ClaudeExtraUsageRemaining: 10},
	}))
	sessions := newSessionStore(t)
	if _, err := sessions.Put("claude", "sticky-extra", old, ""); err != nil {
		t.Fatal(err)
	}
	observed := false
	server := Server{
		Accounts: []accounts.Account{
			{ID: old, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "old-token"},
			{ID: funded, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "funded-token"},
		},
		Sessions: sessions, SchedulerRef: ref, MaxBodyBytes: 1024,
		Logger: slog.New(placementReservationObserver(func(record slog.Record) {
			if record.Message != "rerouting cold sticky session from constrained account" {
				return
			}
			observed = true
			debits := ref.LiveDebits()
			if got := debits[selectacct.ScoreKey(accounts.ProviderClaude, old)]; got != 0 {
				t.Errorf("published sticky move retains rejected account debit = %d", got)
			}
			if got := debits[selectacct.ScoreKey(accounts.ProviderClaude, funded)]; got != 1 {
				t.Errorf("published sticky move funded account debit = %d, want 1", got)
			}
		})),
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	var reservation *selectacct.PlacementReservation
	var pending bool
	picked, _, _, err := server.accountForSessionProviderWithOptions(accounts.ProviderClaude, "claude", "sticky-extra", request, accountSelectionOptions{
		pendingSessionCommit: &pending, placementReservation: &reservation,
	})
	defer reservation.Release()
	if err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("sticky paid fallback never published its move")
	}
	if picked.ID != funded || !pending || reservation == nil {
		t.Fatalf("paid selection account=%q pending=%v reservation=%v", picked.ID, pending, reservation)
	}
	reservation.Release()
	if got := ref.LiveDebits(); len(got) != 0 {
		t.Fatalf("released paid selection leaves live debits: %v", got)
	}
}
