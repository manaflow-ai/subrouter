package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestOlderUsageRefreshCannotOverwriteNewerAccountReload(t *testing.T) {
	accountStore := accounts.CodexStore{Dir: t.TempDir()}
	initialStored := proxyStoredOAuthAccount("old@example.com", "old", time.Now().Add(time.Hour))
	addedStored := proxyStoredOAuthAccount("new@example.com", "new", time.Now().Add(time.Hour))
	if err := accountStore.SaveStored(initialStored); err != nil {
		t.Fatal(err)
	}
	initial, err := accountStore.List()
	if err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(accountStore, initial, nil)
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: "old@example.com", Headroom: 0.9, ShortHeadroom: 0.9,
	}}))
	schedulerRef.SetUpdatedAt(time.Time{})
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	refreshDone := make(chan struct{})
	server := Server{
		AccountRef:    ref,
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: time.Nanosecond,
		ScoreAccounts: func(_ context.Context, available []accounts.Account) ([]selectacct.Score, int) {
			if len(available) == 1 {
				close(refreshStarted)
				<-releaseRefresh
				return []selectacct.Score{{
					AccountID: "old@example.com", Headroom: 0.1, ShortHeadroom: 0.1,
				}}, 1
			}
			return []selectacct.Score{
				{AccountID: "old@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
				{AccountID: "new@example.com", Headroom: 0.42, ShortHeadroom: 0.42},
			}, 2
		},
	}
	go func() {
		server.refreshUsageScoresIfStale(context.Background())
		close(refreshDone)
	}()
	select {
	case <-refreshStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("usage refresh did not start")
	}

	if err := accountStore.SaveStored(addedStored); err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.reloadAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(releaseRefresh)
	select {
	case <-refreshDone:
	case <-time.After(5 * time.Second):
		t.Fatal("usage refresh did not finish")
	}

	got := schedulerRef.Get().ScoreFor(accounts.ProviderCodex, "new@example.com")
	if got.Headroom != 0.42 {
		t.Fatalf("older refresh overwrote reloaded scheduler: new-account headroom = %v, want 0.42", got.Headroom)
	}
}
