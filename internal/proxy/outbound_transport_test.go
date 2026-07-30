package proxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// barrierServer holds every request open until `want` of them are in flight,
// then releases them together. Without this, requests finish fast enough to be
// recycled mid-burst, so the number of connections opened depends on goroutine
// scheduling and any assertion about it flakes. Holding them simultaneously
// forces exactly one connection per concurrent request, deterministically.
type barrierServer struct {
	*httptest.Server
	mu       sync.Mutex
	conns    map[string]struct{}
	want     int
	inFlight int
	release  chan struct{}
}

func newBarrierServer(t *testing.T, want int) *barrierServer {
	t.Helper()
	b := &barrierServer{conns: make(map[string]struct{}), want: want, release: make(chan struct{})}
	b.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		b.inFlight++
		if b.inFlight == b.want {
			close(b.release)
		}
		gate := b.release
		b.mu.Unlock()

		select {
		case <-gate:
		case <-time.After(10 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	b.Server.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state != http.StateNew {
			return
		}
		b.mu.Lock()
		b.conns[c.RemoteAddr().String()] = struct{}{}
		b.mu.Unlock()
	}
	b.Server.Start()
	return b
}

// resetBarrier arms the server for another synchronized burst.
func (b *barrierServer) resetBarrier() {
	b.mu.Lock()
	b.inFlight = 0
	b.release = make(chan struct{})
	b.mu.Unlock()
}

func (b *barrierServer) distinctConns() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.conns)
}

// The proxy speaks HTTP/1.1 only (see NewOutboundTransport), so concurrency is
// bounded by connection count rather than stream multiplexing. If the idle pool
// is left at Go's default of 2, concurrent requests to one host burn a fresh
// connection each and strand it in TIME_WAIT, exhausting ephemeral ports.
func TestNewOutboundTransportPoolsConnectionsPerHost(t *testing.T) {
	transport := NewOutboundTransport()

	if transport.MaxIdleConnsPerHost <= http.DefaultMaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConnsPerHost = %d, want more than the default %d; HTTP/1.1 cannot multiplex so a small pool forces one dial per concurrent request",
			transport.MaxIdleConnsPerHost, http.DefaultMaxIdleConnsPerHost)
	}
	if transport.MaxIdleConns < transport.MaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConns = %d, want >= MaxIdleConnsPerHost %d, otherwise the global cap silently defeats the per-host pool",
			transport.MaxIdleConns, transport.MaxIdleConnsPerHost)
	}
	if transport.IdleConnTimeout <= 0 {
		t.Fatal("IdleConnTimeout must be positive so idle connections are eventually reclaimed")
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 must stay false; the pool sizing above assumes one request per connection")
	}
}

// Concurrent requests to one host must reuse connections rather than dial per
// request. This is the behavior whose absence exhausted the port range.
//
// Both bursts are synchronized by barrierServer so the connection counts are
// deterministic rather than a function of goroutine scheduling. The first burst
// necessarily opens one connection per concurrent request and leaves them all
// idle; the second needs exactly the same number and must therefore dial none.
func TestOutboundTransportReusesConnectionsUnderConcurrency(t *testing.T) {
	const concurrency = 8

	server := newBarrierServer(t, concurrency)
	defer server.Close()

	client := &http.Client{Transport: NewOutboundTransport(), Timeout: 30 * time.Second}

	burst := func(round int) {
		server.resetBarrier()
		var wg sync.WaitGroup
		for range concurrency {
			wg.Add(1)
			go func() {
				defer wg.Done()
				response, err := client.Get(server.URL)
				if err != nil {
					t.Errorf("round %d: %v", round, err)
					return
				}
				// Drain and close so the connection returns to the pool.
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}()
		}
		wg.Wait()
	}

	// All `concurrency` requests are held open together, so this opens exactly
	// one connection each and leaves them all idle in the pool afterwards.
	burst(1)
	afterWarmup := server.distinctConns()
	if afterWarmup != concurrency {
		t.Fatalf("warm-up opened %d connections, want exactly %d; the barrier should hold every request open simultaneously", afterWarmup, concurrency)
	}

	// The same synchronized burst again. Every connection it needs is already
	// idle in the pool, so a correctly sized pool dials nothing. With the
	// default pool of 2, only two survived and the rest must be re-dialed.
	burst(2)
	if dialed := server.distinctConns() - afterWarmup; dialed != 0 {
		t.Fatalf("second burst dialed %d new connections despite %d idle in the pool; connections are not being reused",
			dialed, afterWarmup)
	}
}

// Pooling a connection for longer than the upstream keeps it alive guarantees
// checking out a connection the peer already closed. The next write then fails
// with "use of closed network connection" and the client sees a 502. Go's
// default of 90s is 6x the measured upstream idle close, so this must be set
// explicitly rather than inherited from DefaultTransport.
func TestOutboundIdleTimeoutStaysUnderUpstreamIdleClose(t *testing.T) {
	transport := NewOutboundTransport()

	if transport.IdleConnTimeout >= upstreamIdleCloseAfter {
		t.Fatalf("IdleConnTimeout = %s, must be below the measured upstream idle close of %s; otherwise pooled connections are handed out already closed",
			transport.IdleConnTimeout, upstreamIdleCloseAfter)
	}
	if transport.IdleConnTimeout <= 0 {
		t.Fatalf("IdleConnTimeout = %s, want a positive value", transport.IdleConnTimeout)
	}
}
