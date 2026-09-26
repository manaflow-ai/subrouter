package proxy

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
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

// A fetch that panics must still release its key. httputil.ReverseProxy
// panics with http.ErrAbortHandler when the upstream body breaks mid-copy, and
// the flight runs it under a request that keeps http.ServerContextKey, so this
// is a real path. Without the release every waiter and every later caller for
// the key blocks forever.
func TestSingleFlightReleasesKeyWhenFetchPanics(t *testing.T) {
	sf := newSingleFlight()
	entered := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan any, 1)
	go func() {
		defer func() { leaderDone <- recover() }()
		sf.do("catalog", func() flightResult {
			close(entered)
			<-release
			panic(http.ErrAbortHandler)
		})
	}()
	<-entered

	waiterDone := make(chan flightResult, 1)
	go func() {
		result, _ := sf.do("catalog", func() flightResult {
			return flightResult{statusCode: http.StatusOK, body: []byte("waiter-ran-its-own")}
		})
		waiterDone <- result
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)

	select {
	case <-leaderDone:
	case <-time.After(2 * time.Second):
		t.Fatal("leader never returned")
	}
	select {
	case result := <-waiterDone:
		// The waiter joined the leader's flight, so it shares that flight's
		// outcome: a failure, never an empty success.
		if result.statusCode != http.StatusBadGateway {
			t.Fatalf("waiter on a panicked flight got %+v, want 502", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent waiter blocked forever on a panicked flight")
	}

	followUp := make(chan flightResult, 1)
	go func() {
		result, _ := sf.do("catalog", func() flightResult {
			return flightResult{statusCode: http.StatusOK, body: []byte("fresh")}
		})
		followUp <- result
	}()
	select {
	case result := <-followUp:
		if string(result.body) != "fresh" {
			t.Fatalf("follow-up caller got %+v, want its own fresh fetch", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow-up caller blocked forever on a key a panicked flight never released")
	}
}

// End to end through a real http.Server: the upstream breaks the catalog body
// mid-copy, so ReverseProxy panics inside the flight. The client must get a
// 502 rather than a dropped connection, and the next request for the same
// catalog must be served instead of hanging on the orphaned flight.
func TestCatalogFlightSurvivesUpstreamBodyAbort(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"plugins":[`)
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = io.WriteString(w, `{"plugins":[]}`)
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(pluginTestServer(t, upstream))
	defer proxy.Close()
	proxy.Config.ErrorLog = log.New(io.Discard, "", 0)

	get := func() (int, error) {
		client := &http.Client{Timeout: 3 * time.Second}
		response, err := client.Get(proxy.URL + "/backend-api/ps/plugins/installed?scope=USER")
		if err != nil {
			return 0, err
		}
		defer response.Body.Close()
		_, _ = io.ReadAll(response.Body)
		return response.StatusCode, nil
	}

	status, err := get()
	if err != nil || status != http.StatusBadGateway {
		t.Errorf("broken upstream body: status=%d err=%v, want a 502 response", status, err)
	}
	status, err = get()
	if err != nil {
		t.Fatalf("follow-up request for the same catalog failed (orphaned flight?): %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("follow-up status=%d, want 200", status)
	}
}
