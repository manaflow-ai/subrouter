package proxy

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/selectacct"
)

type slowCountingUsageTransport struct {
	calls   atomic.Int64
	latency time.Duration
}

func (s *slowCountingUsageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	select {
	case <-time.After(s.latency):
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return claudeUsageOK(), nil
}

// TestUsageCallBudgetSimulatedMinute replays one minute of traffic against a
// pool of Claude accounts with a slow (200ms) usage endpoint: two score-TTL
// ticks (t=0s and t=30s), each with a score sweep overlapping three dashboard
// status polls. It reports the upstream usage calls made and asserts the
// budget of one shared fetch per account while the usage-window cache holds.
func TestUsageCallBudgetSimulatedMinute(t *testing.T) {
	const pool = 5
	transport := &slowCountingUsageTransport{latency: 200 * time.Millisecond}
	ref, accts := multiProfileAccountRef(t, transport, pool)
	server := Server{AccountRef: ref, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))}

	for tick := 0; tick < 2; tick++ {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			server.scoreAccounts(context.Background(), accts)
		}()
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ref.UsageStatuses(context.Background())
			}()
		}
		wg.Wait()
		// 30s later the status cache (30s TTL) has expired; the usage-window
		// cache (2m TTL) has not.
		ref.InvalidateUsageStatusCache()
	}
	got := transport.calls.Load()
	t.Logf("simulated minute: pool=%d upstream usage calls=%d (usage+Fable probe per fetch)", pool, got)
	if want := int64(2 * pool); got > want {
		t.Fatalf("upstream usage calls = %d, want <= %d", got, want)
	}
}
