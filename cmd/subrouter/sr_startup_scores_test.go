package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestStartupScoresOverlapUsesOneSweepAcrossDifferentLocalGenerations(t *testing.T) {
	store := newSRUsageScoreStore(t.TempDir())
	accountsOnDisk := []accounts.Account{
		{ID: "first@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "secret-first"},
		{ID: "second@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "secret-second"},
	}
	refs := []*selectacct.SchedulerRef{
		selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
	}
	refs[0].AdvanceAccountGeneration(3)
	refs[1].AdvanceAccountGeneration(91)

	started := make(chan struct{})
	release := make(chan struct{})
	var fetches atomic.Int32
	var callsMu sync.Mutex
	accountCalls := map[string]int{}
	fetch := func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
		fetches.Add(1)
		callsMu.Lock()
		for _, candidate := range candidates {
			accountCalls[candidate.ID]++
		}
		callsMu.Unlock()
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return []selectacct.Score{
			{AccountID: "first@example.com", Provider: accounts.ProviderCodex, Headroom: .8, ShortHeadroom: .8, Fresh: true},
			{AccountID: "second@example.com", Provider: accounts.ProviderCodex, Headroom: .7, ShortHeadroom: .7, Fresh: true},
		}, 2
	}

	errs := make(chan error, 2)
	go func() {
		errs <- ensureStartupScores(context.Background(), startupScoreConfig{
			Interval: time.Minute,
			AccountsSnapshot: func() ([]accounts.Account, uint64) {
				return append([]accounts.Account(nil), accountsOnDisk...), 3
			},
			SchedulerRef: refs[0], FetchScores: fetch, Store: store, PollInterval: time.Millisecond,
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("winning startup sweep did not begin")
	}
	go func() {
		errs <- ensureStartupScores(context.Background(), startupScoreConfig{
			Interval: time.Minute,
			AccountsSnapshot: func() ([]accounts.Account, uint64) {
				return append([]accounts.Account(nil), accountsOnDisk...), 91
			},
			SchedulerRef: refs[1], FetchScores: fetch, Store: store, PollInterval: time.Millisecond,
		})
	}()
	// The loser reaches the coordinator before publication and must wait for
	// the winner's durable snapshot instead of starting another sweep.
	time.Sleep(10 * time.Millisecond)
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("usage sweeps = %d, want exactly 1", got)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	for _, account := range accountsOnDisk {
		if accountCalls[account.ID] != 1 {
			t.Errorf("usage calls for %s = %d, want 1", account.ID, accountCalls[account.ID])
		}
	}
	for i, ref := range refs {
		if got := ref.Get().ScoreFor(accounts.ProviderCodex, "first@example.com").Headroom; got != .8 {
			t.Errorf("worker %d published headroom = %v, want .8", i, got)
		}
	}
	body, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "secret-first") || strings.Contains(string(body), "secret-second") {
		t.Fatal("persisted score snapshot leaked a credential")
	}
}

func TestStartupScoresRejectsStaleCredentialGeneration(t *testing.T) {
	store := newSRUsageScoreStore(t.TempDir())
	now := time.Unix(1_800_000_000, 0).UTC()
	store.now = func() time.Time { return now }
	old := []accounts.Account{{ID: "account@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "old-secret"}}
	if err := store.write(srUsageScoreSnapshot{
		SchemaVersion: scoreSnapshotSchemaVersion,
		GenerationKey: codexScoreGenerationKey(1, old),
		FetchedAt:     now,
		Scores:        []selectacct.Score{{AccountID: "account@example.com", Provider: accounts.ProviderCodex, Headroom: .1}},
	}); err != nil {
		t.Fatal(err)
	}
	current := []accounts.Account{{ID: "account@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "new-secret"}}
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	ref.AdvanceAccountGeneration(1)
	var fetches int
	err := ensureStartupScores(context.Background(), startupScoreConfig{
		Interval:         time.Minute,
		AccountsSnapshot: func() ([]accounts.Account, uint64) { return current, 1 },
		SchedulerRef:     ref,
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			fetches++
			return []selectacct.Score{{AccountID: "account@example.com", Provider: accounts.ProviderCodex, Headroom: .9, Fresh: true}}, 1
		},
		Store: store, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Fatalf("fresh generation fetches = %d, want 1", fetches)
	}
	if got := ref.Get().ScoreFor(accounts.ProviderCodex, "account@example.com").Headroom; got != .9 {
		t.Fatalf("published stale score headroom = %v, want .9", got)
	}
}

func TestStartupScoresFailedSweepRunsOneBatchAndReturnsError(t *testing.T) {
	store := newSRUsageScoreStore(t.TempDir())
	all := []accounts.Account{{ID: "account@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "secret"}}
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	ref.AdvanceAccountGeneration(1)
	var fetches atomic.Int32
	err := ensureStartupScores(context.Background(), startupScoreConfig{
		Interval:         time.Minute,
		AccountsSnapshot: func() ([]accounts.Account, uint64) { return all, 1 },
		SchedulerRef:     ref,
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			fetches.Add(1)
			return nil, 0
		},
		Store: store, PollInterval: time.Millisecond,
	})
	if !errors.Is(err, errStartupScoreSweepFailed) {
		t.Fatalf("startup failure = %v, want shared sweep failure", err)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("failed sweeps = %d, want exactly 1", got)
	}
	release, claimed, lockErr := store.tryLock()
	if lockErr != nil || !claimed {
		t.Fatalf("failed winner did not release kernel lock: claimed=%v err=%v", claimed, lockErr)
	}
	release()
}

func TestStartupScoresRetriesFailedSweepUntilProviderRecovers(t *testing.T) {
	store := newSRUsageScoreStore(t.TempDir())
	all := []accounts.Account{{ID: "account@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "secret"}}
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	ref.AdvanceAccountGeneration(1)
	var fetches atomic.Int32
	err := ensureStartupScores(context.Background(), startupScoreConfig{
		Interval:         time.Minute,
		AccountsSnapshot: func() ([]accounts.Account, uint64) { return all, 1 },
		SchedulerRef:     ref,
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			if fetches.Add(1) == 1 {
				return nil, 0
			}
			return []selectacct.Score{{
				AccountID: "account@example.com", Provider: accounts.ProviderCodex,
				Headroom: .9, ShortHeadroom: .9, Fresh: true,
			}}, 1
		},
		Store:            store,
		PollInterval:     time.Millisecond,
		RetryFailedSweep: true,
		RetryInterval:    time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("startup sweeps = %d, want one retry after the failed sweep", got)
	}
	if got := ref.Get().ScoreFor(accounts.ProviderCodex, "account@example.com").Headroom; got != .9 {
		t.Fatalf("recovered score headroom = %v, want .9", got)
	}
	snapshot, ok, err := store.read(codexScoreGenerationKey(1, all), time.Minute)
	if err != nil || !ok || snapshot.Failed {
		t.Fatalf("recovered snapshot = %+v, present=%v, err=%v", snapshot, ok, err)
	}
}

func TestStartupScoreKernelLockPreventsStaleOwnerDeletingReplacement(t *testing.T) {
	store := newSRUsageScoreStore(t.TempDir())
	oldRelease, claimed, err := store.tryLock()
	if err != nil || !claimed {
		t.Fatalf("old owner claim = %v, %v", claimed, err)
	}
	stale := time.Now().Add(-10 * lockStaleAfter)
	if err := os.Chtimes(store.lockPath, stale, stale); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.tryLock(); err != nil || claimed {
		t.Fatalf("stale live owner was replaced: claimed=%v err=%v", claimed, err)
	}

	oldRelease()
	newRelease, claimed, err := store.tryLock()
	if err != nil || !claimed {
		t.Fatalf("replacement owner claim = %v, %v", claimed, err)
	}
	// A delayed duplicate release from the old owner must affect only its
	// already-closed kernel handle, never the replacement owner's lock.
	oldRelease()
	if _, claimed, err := store.tryLock(); err != nil || claimed {
		t.Fatalf("third sweep admitted after old release: claimed=%v err=%v", claimed, err)
	}

	newRelease()
	thirdRelease, claimed, err := store.tryLock()
	if err != nil || !claimed {
		t.Fatalf("third owner was not admitted after current release: claimed=%v err=%v", claimed, err)
	}
	thirdRelease()
}

func TestStartupScoresFailedSweepDoesNotPoisonChangedCredentials(t *testing.T) {
	store := newSRUsageScoreStore(t.TempDir())
	all := []accounts.Account{{ID: "account@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "old-secret"}}
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	ref.AdvanceAccountGeneration(1)
	var fetches int
	err := ensureStartupScores(context.Background(), startupScoreConfig{
		Interval:         time.Minute,
		AccountsSnapshot: func() ([]accounts.Account, uint64) { return append([]accounts.Account(nil), all...), 1 },
		SchedulerRef:     ref,
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			fetches++
			if fetches == 1 {
				all[0].Token = "new-secret"
				return nil, 0
			}
			return []selectacct.Score{{AccountID: "account@example.com", Provider: accounts.ProviderCodex, Headroom: .9, Fresh: true}}, 1
		},
		Store: store, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fetches != 2 {
		t.Fatalf("credential generations fetched = %d, want one attempt per distinct credential generation", fetches)
	}
}

func TestStartupScoresOverlappingWorkersShareFailedSweep(t *testing.T) {
	store := newSRUsageScoreStore(t.TempDir())
	all := []accounts.Account{{ID: "account@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "secret"}}
	refs := []*selectacct.SchedulerRef{
		selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
	}
	refs[0].AdvanceAccountGeneration(1)
	refs[1].AdvanceAccountGeneration(8)
	started := make(chan struct{})
	release := make(chan struct{})
	var fetches atomic.Int32
	fetch := func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
		fetches.Add(1)
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return nil, 0
	}
	errs := make(chan error, 2)
	for i, generation := range []uint64{1, 8} {
		i, generation := i, generation
		go func() {
			errs <- ensureStartupScores(context.Background(), startupScoreConfig{
				Interval: time.Minute,
				AccountsSnapshot: func() ([]accounts.Account, uint64) {
					return all, generation
				},
				SchedulerRef: refs[i], FetchScores: fetch, Store: store, PollInterval: time.Millisecond,
			})
		}()
		if i == 0 {
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("failed winner did not begin its sweep")
			}
		}
	}
	time.Sleep(10 * time.Millisecond)
	close(release)
	for range 2 {
		if err := <-errs; !errors.Is(err, errStartupScoreSweepFailed) {
			t.Fatalf("overlap failure = %v, want shared sweep failure", err)
		}
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("overlapping failed sweeps = %d, want exactly 1", got)
	}
}

func TestStartupScoreReadinessPreservesDisabledSemantics(t *testing.T) {
	for _, readiness := range []*startupScoreReadiness{
		nil,
		{required: false}, // interval=0 or usage fetching disabled
	} {
		if err := readiness.check(); err != nil {
			t.Fatalf("disabled startup score gate = %v", err)
		}
	}
	required := &startupScoreReadiness{required: true}
	if err := required.check(); err == nil {
		t.Fatal("required score gate was ready before snapshot publication")
	}
	required.ready.Store(true)
	if err := required.check(); err != nil {
		t.Fatalf("published score gate = %v", err)
	}
}

func TestStartupScoreReadinessOnlyAppliesToCodexOAuthAccounts(t *testing.T) {
	for _, test := range []struct {
		name     string
		accounts []accounts.Account
		want     bool
	}{
		{name: "empty", accounts: nil, want: false},
		{name: "claude oauth", accounts: []accounts.Account{{Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}}, want: false},
		{name: "api key codex", accounts: []accounts.Account{{Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeAPIKey}}, want: false},
		{name: "other oauth", accounts: []accounts.Account{{Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth}}, want: false},
		{name: "codex oauth", accounts: []accounts.Account{{Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := requiresStartupScoreReadiness(test.accounts); got != test.want {
				t.Fatalf("requiresStartupScoreReadiness() = %v, want %v", got, test.want)
			}
		})
	}
}
