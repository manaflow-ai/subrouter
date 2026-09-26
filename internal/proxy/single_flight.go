package proxy

import (
	"net/http"
	"sync"
)

// Cache misses arrive in bursts: several codex sessions starting at once all
// ask for the same catalog before any of them has filled the cache. Each miss
// then walks the catalog's pagination upstream, so N clients cost N times the
// upstream requests for one identical answer. Measured with three concurrent
// cold sessions: 12 catalog walks, 168 upstream page fetches in seconds, which
// is the shape that gets a proxy bot-challenged.
//
// So identical concurrent misses share one upstream fetch. The first caller
// does the work; the rest wait and take its result.

type flightResult struct {
	statusCode int
	header     map[string][]string
	body       []byte
}

type flightCall struct {
	wg     sync.WaitGroup
	result flightResult
}

type singleFlight struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

func newSingleFlight() *singleFlight {
	return &singleFlight{calls: make(map[string]*flightCall)}
}

// do runs fetch for key, or waits for an in-flight fetch of the same key. The
// bool reports whether this caller waited on someone else's fetch rather than
// running its own.
func (s *singleFlight) do(key string, fetch func() flightResult) (flightResult, bool) {
	if s == nil {
		return fetch(), false
	}
	s.mu.Lock()
	if call, ok := s.calls[key]; ok {
		s.mu.Unlock()
		call.wg.Wait()
		return call.result, true
	}
	// Waiters read this if fetch panics before producing a result: they
	// shared a failed flight, so they get a failure, not an empty success.
	call := &flightCall{result: flightFailedResult()}
	call.wg.Add(1)
	s.calls[key] = call
	s.mu.Unlock()

	// Release the key even if fetch panics, or every waiter and every later
	// caller for it blocks forever. The panic still reaches this caller.
	defer func() {
		s.mu.Lock()
		delete(s.calls, key)
		s.mu.Unlock()
		call.wg.Done()
	}()
	call.result = fetch()
	return call.result, false
}

// flightFailedResult is the shared answer for a flight that could not
// complete.
func flightFailedResult() flightResult {
	return flightResult{
		statusCode: http.StatusBadGateway,
		header:     http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		body:       []byte("upstream fetch failed\n"),
	}
}
