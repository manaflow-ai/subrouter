package proxy

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Concurrent callers for one key must produce exactly one upstream fetch, and
// all of them must receive its result.
func TestSingleFlightCollapsesConcurrentMisses(t *testing.T) {
	sf := newSingleFlight()
	var fetches int32
	release := make(chan struct{})

	var wg sync.WaitGroup
	results := make([]flightResult, 8)
	shared := make([]bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], shared[i] = sf.do("catalog", func() flightResult {
				atomic.AddInt32(&fetches, 1)
				<-release
				return flightResult{statusCode: 200, body: []byte("catalog-body")}
			})
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Fatalf("%d upstream fetches, want 1", got)
	}
	sharedCount := 0
	for i := range results {
		if string(results[i].body) != "catalog-body" || results[i].statusCode != 200 {
			t.Fatalf("caller %d got %+v", i, results[i])
		}
		if shared[i] {
			sharedCount++
		}
	}
	if sharedCount != 7 {
		t.Fatalf("%d callers waited on the shared fetch, want 7", sharedCount)
	}
}

// Different keys must not block each other.
func TestSingleFlightKeepsKeysIndependent(t *testing.T) {
	sf := newSingleFlight()
	var fetches int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sf.do(string(rune('a'+i)), func() flightResult {
				atomic.AddInt32(&fetches, 1)
				return flightResult{statusCode: 200}
			})
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&fetches); got != 4 {
		t.Fatalf("%d fetches for 4 distinct keys, want 4", got)
	}
}

// A finished flight must not be reused: the next caller fetches again.
func TestSingleFlightDoesNotCacheAcrossCalls(t *testing.T) {
	sf := newSingleFlight()
	var fetches int32
	for i := 0; i < 3; i++ {
		sf.do("k", func() flightResult {
			atomic.AddInt32(&fetches, 1)
			return flightResult{statusCode: 200}
		})
	}
	if got := atomic.LoadInt32(&fetches); got != 3 {
		t.Fatalf("%d fetches for 3 sequential calls, want 3", got)
	}
}
