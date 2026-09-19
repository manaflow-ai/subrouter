package proxy

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

type concurrencyTrackingTransport struct {
	mu      sync.Mutex
	current int
	maximum int
	calls   int
}

type contextBlockingTransport struct {
	mu      sync.Mutex
	current int
	maximum int
	calls   int
}

func (t *contextBlockingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.current++
	t.calls++
	if t.current > t.maximum {
		t.maximum = t.current
	}
	t.mu.Unlock()
	<-req.Context().Done()
	t.mu.Lock()
	t.current--
	t.mu.Unlock()
	return nil, req.Context().Err()
}

func (t *contextBlockingTransport) counts() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls, t.maximum
}

func (t *concurrencyTrackingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.current++
	t.calls++
	if t.current > t.maximum {
		t.maximum = t.current
	}
	t.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	t.mu.Lock()
	t.current--
	t.mu.Unlock()
	return usageOKResponse(), nil
}

func (t *concurrencyTrackingTransport) counts() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls, t.maximum
}

func accountFetchConcurrencyFixture(t *testing.T) (*AccountRef, []accounts.Account, *concurrencyTrackingTransport) {
	t.Helper()
	store := accounts.CodexStore{Dir: t.TempDir()}
	available := make([]accounts.Account, 0, 8)
	for i := 0; i < 8; i++ {
		id := "account-" + string(rune('a'+i)) + "@example.com"
		token := proxyTestCodexJWT(id, id, time.Now().Add(time.Hour))
		stored := accounts.StoredCodexAccount{
			Email: id, Provider: accounts.ProviderCodex,
			OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
			Auth: accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
				AccessToken: token, RefreshToken: "refresh-" + id, IDToken: token,
			}},
		}
		if err := store.SaveStored(stored); err != nil {
			t.Fatal(err)
		}
		account, ok := stored.Account(stored.SourcePath(store))
		if !ok {
			t.Fatal("stored test account is unusable")
		}
		available = append(available, account)
	}
	transport := &concurrencyTrackingTransport{}
	ref := NewAccountRef(store, available, &http.Client{Transport: transport})
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	return ref, available, transport
}

func TestUsageStatusesLiveBoundsUpstreamConcurrency(t *testing.T) {
	ref, _, transport := accountFetchConcurrencyFixture(t)
	statuses := ref.usageStatusesLive(context.Background())
	if len(statuses) != 8 {
		t.Fatalf("statuses = %d, want 8", len(statuses))
	}
	calls, maximum := transport.counts()
	if calls != 8 || maximum != accountFetchConcurrency {
		t.Fatalf("usage fetch calls/max = %d/%d, want 8/%d", calls, maximum, accountFetchConcurrency)
	}
}

func TestScoreAccountsBoundsUpstreamConcurrency(t *testing.T) {
	ref, available, transport := accountFetchConcurrencyFixture(t)
	server := Server{
		AccountRef: ref,
		Scheduler:  selectacct.NewScheduler(nil),
	}
	_, scored := server.scoreAccounts(context.Background(), available)
	if scored != 8 {
		t.Fatalf("scored = %d, want 8", scored)
	}
	calls, maximum := transport.counts()
	if calls != 8 || maximum != accountFetchConcurrency {
		t.Fatalf("score fetch calls/max = %d/%d, want 8/%d", calls, maximum, accountFetchConcurrency)
	}
}

func TestAccountFetchSweepsUseOneDeadlineAcrossAllBatches(t *testing.T) {
	for _, testCase := range []struct {
		name string
		run  func(context.Context, *AccountRef, []accounts.Account)
	}{
		{
			name: "usage status",
			run: func(ctx context.Context, ref *AccountRef, _ []accounts.Account) {
				ref.usageStatusesLive(ctx)
			},
		},
		{
			name: "score accounts",
			run: func(ctx context.Context, ref *AccountRef, available []accounts.Account) {
				Server{AccountRef: ref, Scheduler: selectacct.NewScheduler(nil)}.scoreAccounts(ctx, available)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ref, available, _ := accountFetchConcurrencyFixture(t)
			transport := &contextBlockingTransport{}
			ref.client = &http.Client{Transport: transport}
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
			defer cancel()
			started := time.Now()
			testCase.run(ctx, ref, available)
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("sweep exceeded its shared deadline: %v", elapsed)
			}
			calls, maximum := transport.counts()
			if calls != accountFetchConcurrency || maximum != accountFetchConcurrency {
				t.Fatalf("blocked sweep calls/max = %d/%d, want %d/%d", calls, maximum, accountFetchConcurrency, accountFetchConcurrency)
			}
		})
	}
}

func TestSharedScoreDeadlinePreservesSeedsForQueuedAccounts(t *testing.T) {
	ref, available, _ := accountFetchConcurrencyFixture(t)
	transport := &contextBlockingTransport{}
	ref.client = &http.Client{Transport: transport}
	seeded := make([]selectacct.Score, 0, len(available))
	for _, account := range available {
		seeded = append(seeded, selectacct.Score{
			AccountID: account.ID,
			Provider:  account.Provider,
			Headroom:  0.75,
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	scores, scored := (Server{
		AccountRef: ref,
		Scheduler:  selectacct.NewScheduler(seeded),
	}).scoreAccounts(ctx, available)
	if scored != 0 {
		t.Fatalf("freshly scored accounts = %d, want 0 from blocked upstream", scored)
	}
	if len(scores) != len(available) {
		t.Fatalf("scores = %d, want %d", len(scores), len(available))
	}
	for _, score := range scores {
		if score.Headroom != 0.75 {
			t.Fatalf("account %s lost seeded headroom: %v", score.AccountID, score.Headroom)
		}
	}
	if calls, maximum := transport.counts(); calls != accountFetchConcurrency || maximum != accountFetchConcurrency {
		t.Fatalf("blocked sweep calls/max = %d/%d, want %d/%d", calls, maximum, accountFetchConcurrency, accountFetchConcurrency)
	}
}

func TestAccountFetchSweepsDoNotStartWorkAfterCallerDeadline(t *testing.T) {
	for _, testCase := range []struct {
		name string
		run  func(context.Context, *AccountRef, []accounts.Account)
	}{
		{
			name: "usage status",
			run: func(ctx context.Context, ref *AccountRef, _ []accounts.Account) {
				ref.usageStatusesLive(ctx)
			},
		},
		{
			name: "score accounts",
			run: func(ctx context.Context, ref *AccountRef, available []accounts.Account) {
				Server{AccountRef: ref, Scheduler: selectacct.NewScheduler(nil)}.scoreAccounts(ctx, available)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			for attempt := 0; attempt < 20; attempt++ {
				ref, available, transport := accountFetchConcurrencyFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				testCase.run(ctx, ref, available)
				calls, _ := transport.counts()
				if calls != 0 {
					t.Fatalf("attempt %d started %d upstream calls after caller deadline", attempt, calls)
				}
			}
		})
	}
}

func TestAccountFetchConcurrencyScalesWithPool(t *testing.T) {
	for _, test := range []struct{ n, want int }{
		{0, accountFetchConcurrency}, {8, accountFetchConcurrency}, {16, accountFetchConcurrency},
		{20, 5}, {105, 27}, {115, 29}, {1000, maxAccountFetchConcurrency},
	} {
		if got := accountFetchConcurrencyFor(test.n); got != test.want {
			t.Fatalf("accountFetchConcurrencyFor(%d) = %d, want %d", test.n, got, test.want)
		}
	}
}

func TestRotatedIndexesCoverPoolFromMovingStart(t *testing.T) {
	if got := rotatedIndexes(0, 3); len(got) != 0 {
		t.Fatalf("empty pool = %v", got)
	}
	want := []int{2, 3, 4, 0, 1}
	got := rotatedIndexes(5, 7)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rotatedIndexes(5, 7) = %v, want %v", got, want)
		}
	}
	ref := &AccountRef{}
	first, second, third := ref.nextSweepStart(27), ref.nextSweepStart(27), ref.nextSweepStart(0)
	if first != 0 || second != 27 || third != 54 {
		t.Fatalf("sweep starts = %d, %d, %d; want 0, 27, 54", first, second, third)
	}
}

func TestUsageStatusesLiveRotatesStarvedTailAcrossSweeps(t *testing.T) {
	ref, _, _ := accountFetchConcurrencyFixture(t)
	blocking := &contextBlockingTransport{}
	ref.client = &http.Client{Transport: blocking}
	// Every fetch blocks until the sweep deadline, so exactly one batch of
	// accounts acquires a slot per sweep. The set that acquires must move.
	acquired := func() map[string]bool {
		out := map[string]bool{}
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		for _, status := range ref.usageStatusesLive(ctx) {
			if status.Provider == accounts.ProviderCodex && status.Error != "context deadline exceeded" {
				out[status.ID] = true
			}
		}
		return out
	}
	first := acquired()
	second := acquired()
	if len(first) != accountFetchConcurrency || len(second) != accountFetchConcurrency {
		t.Fatalf("acquired per sweep = %d, %d; want %d each", len(first), len(second), accountFetchConcurrency)
	}
	// Spawn order rotates by the batch width, but semaphore admission among
	// goroutines spawned in the same instant is only approximately FIFO, so
	// allow a little overlap while requiring the served set to move.
	same := 0
	for id := range first {
		if second[id] {
			same++
		}
	}
	if same > len(first)/2 {
		t.Fatalf("second sweep re-served %d of %d accounts from the first batch: first=%v second=%v", same, len(first), first, second)
	}
}
