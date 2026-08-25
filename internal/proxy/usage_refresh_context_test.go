package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// A usage-score refresh is triggered by whichever request found the scores
// stale, but must not die with that request: a client disconnect mid-refresh
// used to cancel every remaining per-account refresh ("context canceled" for
// the whole pool) while still stamping the TTL window, starving the scheduler
// of fresh scores for another full TTL per impatient client.
func TestUsageRefreshSurvivesTriggeringRequestCancellation(t *testing.T) {
	accountStore := accounts.CodexStore{Dir: t.TempDir()}
	stored := proxyStoredOAuthAccount("acct@example.com", "acct", time.Now().Add(time.Hour))
	if err := accountStore.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	loaded, err := accountStore.List()
	if err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(accountStore, loaded, nil)
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	schedulerRef.SetUpdatedAt(time.Time{})

	var scoreCtxErr error
	var scoreCtxHasDeadline bool
	server := Server{
		AccountRef:    ref,
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: time.Nanosecond,
		ScoreAccounts: func(ctx context.Context, _ []accounts.Account) ([]selectacct.Score, int) {
			scoreCtxErr = ctx.Err()
			_, scoreCtxHasDeadline = ctx.Deadline()
			return []selectacct.Score{{
				AccountID: "acct@example.com", Headroom: 0.5, ShortHeadroom: 0.5,
			}}, 1
		},
	}

	requestCtx, cancel := context.WithCancel(context.Background())
	cancel() // the triggering client is already gone
	server.refreshUsageScoresIfStale(requestCtx)

	if scoreCtxErr != nil {
		t.Fatalf("score refresh ran on the canceled request context: %v", scoreCtxErr)
	}
	if !scoreCtxHasDeadline {
		t.Fatal("detached score refresh context has no deadline; a wedged upstream would hold account selection open forever")
	}
	got := schedulerRef.Get().ScoreFor(accounts.ProviderCodex, "acct@example.com")
	if got.Headroom != 0.5 {
		t.Fatalf("refresh result was not published: headroom = %v, want 0.5", got.Headroom)
	}
}
