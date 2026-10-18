package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// regionProxy is one regional egress in front of that region's own pool.
// Plain proxied HTTP requests are forwarded to the pool with the region
// stamped in codexEgressHeader; CONNECT (how a websocket goes through a
// proxy) is tunnelled straight to the pool, whatever host it names.
func regionProxy(t *testing.T, name string, pool *url.URL, calls *atomic.Int32) *url.URL {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method == http.MethodConnect {
			upstream, err := net.Dial("tcp", pool.Host)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				_ = upstream.Close()
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			client, buffered, err := hijacker.Hijack()
			if err != nil {
				_ = upstream.Close()
				return
			}
			go func() {
				defer upstream.Close()
				defer client.Close()
				if buffered.Reader.Buffered() > 0 {
					peek, _ := buffered.Reader.Peek(buffered.Reader.Buffered())
					_, _ = upstream.Write(peek)
				}
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
				go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
				<-done
			}()
			return
		}
		target := *r.URL
		target.Scheme, target.Host = pool.Scheme, pool.Host
		forward, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		forward.Header = r.Header.Clone()
		forward.Header.Set(codexEgressHeader, name)
		response, err := http.DefaultTransport.RoundTrip(forward)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(server.Close)
	parsed, _ := url.Parse(server.URL)
	return parsed
}

// regionPool is one region's Codex backend: it serves HTTP turns and
// websocket turns, failing or completing each as fail says.
type regionPool struct {
	name    string
	fail    func() bool
	http    atomic.Int32
	creates atomic.Int32
	server  *httptest.Server
	url     *url.URL
	sawTest atomic.Bool
}

func newRegionPool(t *testing.T, name string, fail func() bool) *regionPool {
	t.Helper()
	pool := &regionPool{name: name, fail: fail}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	pool.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(codexFaultHeader) != "" {
			pool.sawTest.Store(true)
		}
		if websocket.IsWebSocketUpgrade(r) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				_, _, err := conn.ReadMessage()
				if err != nil {
					return
				}
				pool.creates.Add(1)
				if pool.fail() {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCapacityFailure))
					continue
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"served-from-`+name+`"}`))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCompleted))
			}
		}
		pool.http.Add(1)
		if pool.fail() {
			codexEgressWriteOverloaded(w)
			return
		}
		codexEgressWriteCompleted(w, name)
	}))
	t.Cleanup(pool.server.Close)
	pool.url, _ = url.Parse(pool.server.URL)
	return pool
}

func always(v bool) func() bool { return func() bool { return v } }

type regionRig struct {
	local   *regionPool
	regions []*regionPool
	calls   []*atomic.Int32
	server  Server
}

func newRegionRig(t *testing.T, local *regionPool, regions ...*regionPool) *regionRig {
	t.Helper()
	rig := &regionRig{local: local, regions: regions}
	proxies := make([]*url.URL, 0, len(regions))
	for _, region := range regions {
		calls := &atomic.Int32{}
		rig.calls = append(rig.calls, calls)
		proxies = append(proxies, regionProxy(t, region.name, region.url, calls))
	}
	rig.server = codexEgressServer(t, local.url, proxies, 1)
	rig.server.CodexEgress.PinStorePath = ""
	rig.server.CodexCapacityEventsPath = filepath.Join(t.TempDir(), "codex-capacity-events.jsonl")
	return rig
}

func readCapacityEvents(t *testing.T, path string) []CodexCapacityEvent {
	t.Helper()
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []CodexCapacityEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event CodexCapacityEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("event line %q: %v", scanner.Text(), err)
		}
		events = append(events, event)
	}
	return events
}

func countEvents(events []CodexCapacityEvent, outcome, region string) int {
	n := 0
	for _, event := range events {
		if event.Outcome == outcome && (region == "" || event.Region == region) {
			n++
		}
	}
	return n
}

// The same-account capacity ladder replays out of another region: the
// local pool refuses, the very next attempt leaves through an egress, and
// the request is absorbed without the egress fallback layer ever running.
func TestCodexRegionsLadderRotatesRegion(t *testing.T) {
	local := newRegionPool(t, "local", always(true))
	fra := newRegionPool(t, "fra", always(false))
	rig := newRegionRig(t, local, fra)
	rig.server.CodexOverloadFailover = &CodexOverloadFailoverConfig{}
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "ladder-1")
	if status != http.StatusOK || !strings.Contains(body, "served-from-fra") {
		t.Fatalf("status=%d body=%s, want the Frankfurt stream", status, body)
	}
	if local.http.Load() != 1 || fra.http.Load() != 1 {
		t.Fatalf("local=%d fra=%d, want one attempt each", local.http.Load(), fra.http.Load())
	}
	events := readCapacityEvents(t, rig.server.CodexCapacityEventsPath)
	if countEvents(events, codexCapacityOutcomeAttempt, "local") != 1 || countEvents(events, codexCapacityOutcomeAbsorbed, "") != 1 ||
		countEvents(events, codexCapacityOutcomeClientVisible, "") != 0 {
		t.Fatalf("events=%+v, want one local attempt failure and one absorbed request", events)
	}
}

// While local is cooling for the model, a brand-new session does not start
// there.
func TestCodexRegionsNewSessionSkipsCoolingLocal(t *testing.T) {
	local := newRegionPool(t, "local", always(true))
	fra := newRegionPool(t, "fra", always(false))
	tyo := newRegionPool(t, "tyo", always(false))
	rig := newRegionRig(t, local, fra, tyo)
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	for index := range 6 {
		status, body := codexEgressPost(t, proxy.URL, fmt.Sprintf("cool-%d", index))
		if status != http.StatusOK || !strings.Contains(body, "served-from-") {
			t.Fatalf("session %d: status=%d body=%s", index, status, body)
		}
	}
	if local.http.Load() != 1 {
		t.Fatalf("local attempts=%d, want only the first", local.http.Load())
	}
}

// Every region refusing reaches the client, and that is recorded once as
// client_visible next to the per-region attempt failures; the admin
// endpoint reports both.
func TestCodexRegionsRecordsClientVisibleFailure(t *testing.T) {
	local := newRegionPool(t, "local", always(true))
	fra := newRegionPool(t, "fra", always(true))
	rig := newRegionRig(t, local, fra)
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	if status, _ := codexEgressPost(t, proxy.URL, "visible-1"); status == http.StatusOK {
		t.Fatalf("status=%d, want the capacity failure", status)
	}
	events := readCapacityEvents(t, rig.server.CodexCapacityEventsPath)
	if countEvents(events, codexCapacityOutcomeClientVisible, "") != 1 {
		t.Fatalf("events=%+v, want exactly one client_visible", events)
	}
	if countEvents(events, codexCapacityOutcomeAttempt, "local") < 1 ||
		countEvents(events, codexCapacityOutcomeAttempt, rig.server.CodexEgress.Proxies[0].Host) < 1 {
		t.Fatalf("events=%+v, want attempt failures in both regions", events)
	}
	response, err := http.Get(proxy.URL + "/_subrouter/codex-capacity")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var report CodexCapacityReport
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.Events == nil || len(report.Events.ClientVisible) != 1 || len(report.Regions) < 2 {
		t.Fatalf("report events=%+v regions=%+v, want the client-visible event and both regions", report.Events, report.Regions)
	}
}

// The test fault is honored only when enabled, and only from loopback; it
// never reaches upstream either way.
func TestCodexRegionsFaultInjection(t *testing.T) {
	local := newRegionPool(t, "local", always(false))
	fra := newRegionPool(t, "fra", always(false))
	var lastRegion string
	post := func(t *testing.T, proxyURL string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, proxyURL+"/responses",
			strings.NewReader(`{"model":"gpt-6-astra","session_id":"fault-1","input":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(codexFaultHeader, "capacity=local")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		lastRegion = response.Header.Get(codexRegionHeader)
		return response.StatusCode, string(body)
	}

	off := newRegionRig(t, local, fra)
	proxyOff := httptest.NewServer(off.server.Handler())
	defer proxyOff.Close()
	if status, body := post(t, proxyOff.URL); status != http.StatusOK || !strings.Contains(body, "served-from-local") {
		t.Fatalf("fault off: status=%d body=%s, want local to serve", status, body)
	}

	on := newRegionRig(t, local, fra)
	on.server.CodexFaultInjection = true
	proxyOn := httptest.NewServer(on.server.Handler())
	defer proxyOn.Close()
	localBefore := local.http.Load()
	if status, body := post(t, proxyOn.URL); status != http.StatusOK || !strings.Contains(body, "served-from-fra") {
		t.Fatalf("fault on: status=%d body=%s, want Frankfurt to serve", status, body)
	}
	if local.http.Load() != localBefore {
		t.Fatalf("local was called under a local fault")
	}
	if lastRegion != on.server.CodexEgress.Proxies[0].Host {
		t.Fatalf("%s=%q, want the Frankfurt proxy", codexRegionHeader, lastRegion)
	}
	if local.sawTest.Load() || fra.sawTest.Load() {
		t.Fatalf("the test fault header reached upstream")
	}
}

// Load simulation through the real handler: the local pool sheds 90% of
// requests and each of four regions sheds 50%, independently. With region
// rotation nearly every request is served; the local pool alone would fail
// about nine in ten.
func TestCodexRegionsSimulatedSheddingRarelyReachesClient(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var mu sync.Mutex
	shed := func(p float64) func() bool {
		return func() bool { mu.Lock(); defer mu.Unlock(); return rng.Float64() < p }
	}
	local := newRegionPool(t, "local", shed(0.9))
	regions := []*regionPool{
		newRegionPool(t, "fra", shed(0.5)), newRegionPool(t, "tyo", shed(0.5)),
		newRegionPool(t, "iad", shed(0.5)), newRegionPool(t, "sin", shed(0.5)),
	}
	rig := newRegionRig(t, local, regions...)
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	const requests = 200
	failed := 0
	for index := range requests {
		if status, _ := codexEgressPost(t, proxy.URL, fmt.Sprintf("sim-%d", index)); status != http.StatusOK {
			failed++
		}
	}
	rate := float64(failed) / requests
	t.Logf("client-visible capacity failures: %d of %d (%.1f%%)", failed, requests, rate*100)
	if rate > 0.03 {
		t.Fatalf("client-visible rate %.1f%%, want under 3%%", rate*100)
	}
	events := readCapacityEvents(t, rig.server.CodexCapacityEventsPath)
	if got := countEvents(events, codexCapacityOutcomeClientVisible, ""); got != failed {
		t.Fatalf("client_visible events=%d, want %d (one per failed request)", got, failed)
	}
}

func dialRegionWebSocket(t *testing.T, proxyURL, session string, autonomous bool) *websocket.Conn {
	t.Helper()
	header := http.Header{"X-Subrouter-Session": []string{session}}
	if autonomous {
		header.Set(AgentRetryPolicyHeader, "autonomous")
	}
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxyURL, "http")+"/responses", header)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	_ = response.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readRegionTurn(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	var got []string
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, body, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("client read after %q: %v", got, err)
		}
		got = append(got, string(body))
		if strings.Contains(string(body), "response.failed") {
			t.Fatalf("client received failure event %q", body)
		}
		if strings.Contains(string(body), "response.completed") {
			return strings.Join(got, "\n")
		}
	}
}

// A session the HTTP path pinned to an egress used to be refused with 426;
// its websocket now dials through that egress.
func TestCodexRegionsPinnedSessionWebSocketDialsThroughEgress(t *testing.T) {
	local := newRegionPool(t, "local", always(true))
	fra := newRegionPool(t, "fra", always(false))
	rig := newRegionRig(t, local, fra)
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	if status, body := codexEgressPost(t, proxy.URL, "pinned-ws"); status != http.StatusOK || !strings.Contains(body, "served-from-fra") {
		t.Fatalf("http turn status=%d body=%s, want Frankfurt", status, body)
	}
	conn := dialRegionWebSocket(t, proxy.URL, "pinned-ws", false)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	if turn := readRegionTurn(t, conn); !strings.Contains(turn, "served-from-fra") {
		t.Fatalf("turn=%s, want it served from Frankfurt", turn)
	}
	if local.creates.Load() != 0 {
		t.Fatalf("local websocket creates=%d, want none", local.creates.Load())
	}
}

// An autonomous self-contained turn that hits capacity redials out of the
// next region behind the open client socket; the client sees only success.
func TestCodexRegionsAutonomousTurnRedialsInNextRegion(t *testing.T) {
	local := newRegionPool(t, "local", always(true))
	fra := newRegionPool(t, "fra", always(false))
	rig := newRegionRig(t, local, fra)
	rig.server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	conn := dialRegionWebSocket(t, proxy.URL, "auto-ws", true)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	if turn := readRegionTurn(t, conn); !strings.Contains(turn, "served-from-fra") {
		t.Fatalf("turn=%s, want it served from Frankfurt", turn)
	}
	if local.creates.Load() != 1 || fra.creates.Load() != 1 {
		t.Fatalf("local=%d fra=%d creates, want one each", local.creates.Load(), fra.creates.Load())
	}
	events := readCapacityEvents(t, rig.server.CodexCapacityEventsPath)
	if countEvents(events, codexCapacityOutcomeAttempt, "local") != 1 || countEvents(events, codexCapacityOutcomeAbsorbed, "") != 1 {
		t.Fatalf("events=%+v, want one local failure and one absorbed turn", events)
	}
}

// A session that left local only because local was cooling is not pinned:
// once local recovers, its next turn is served at home again.
func TestCodexRegionsSessionReturnsHomeAfterCooldown(t *testing.T) {
	var localDown atomic.Bool
	localDown.Store(true)
	local := newRegionPool(t, "local", localDown.Load)
	fra := newRegionPool(t, "fra", always(false))
	rig := newRegionRig(t, local, fra)
	var mu sync.Mutex
	now := time.Now()
	rig.server.codexRegions = newCodexRegionHealth(rig.server.CodexEgress)
	rig.server.codexRegions.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	// Another session's failure puts local in cooldown.
	_, _ = codexEgressPost(t, proxy.URL, "other-session")
	if status, body := codexEgressPost(t, proxy.URL, "home-1"); status != http.StatusOK || !strings.Contains(body, "served-from-fra") {
		t.Fatalf("while cooling: status=%d body=%s, want Frankfurt", status, body)
	}
	localDown.Store(false)
	mu.Lock()
	now = now.Add(codexRegionCooldown + time.Second)
	mu.Unlock()
	if status, body := codexEgressPost(t, proxy.URL, "home-1"); status != http.StatusOK || !strings.Contains(body, "served-from-local") {
		t.Fatalf("after cooldown: status=%d body=%s, want the local uplink", status, body)
	}
}

// An autonomous turn's redial skips an egress whose proxy is down instead
// of retrying it in a loop, and lands on the next region.
func TestCodexRegionsAutonomousRedialSkipsDeadEgress(t *testing.T) {
	local := newRegionPool(t, "local", always(true))
	tyo := newRegionPool(t, "tyo", always(false))
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL, _ := url.Parse(dead.URL)
	dead.Close()
	var tyoCalls atomic.Int32
	server := codexEgressServer(t, local.url, []*url.URL{deadURL, regionProxy(t, "tyo", tyo.url, &tyoCalls)}, 1)
	server.CodexCapacityEventsPath = filepath.Join(t.TempDir(), "events.jsonl")
	server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	for index := range 4 {
		conn := dialRegionWebSocket(t, proxy.URL, fmt.Sprintf("dead-egress-%d", index), true)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","input":[]}`)); err != nil {
			t.Fatal(err)
		}
		if turn := readRegionTurn(t, conn); !strings.Contains(turn, "served-from-tyo") {
			t.Fatalf("session %d turn=%s, want Tokyo", index, turn)
		}
	}
}

// A test fault that blocks every region fails the request without sending
// anything upstream.
func TestCodexRegionsFaultBlockingEveryRegionSendsNothing(t *testing.T) {
	local := newRegionPool(t, "local", always(false))
	fra := newRegionPool(t, "fra", always(false))
	rig := newRegionRig(t, local, fra)
	rig.server.CodexFaultInjection = true
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/responses",
		strings.NewReader(`{"model":"gpt-6-astra","session_id":"fault-all","input":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(codexFaultHeader, "capacity=local,"+rig.server.CodexEgress.Proxies[0].Host)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode == http.StatusOK {
		t.Fatalf("status=%d, want a failure", response.StatusCode)
	}
	if local.http.Load() != 0 || fra.http.Load() != 0 {
		t.Fatalf("local=%d fra=%d upstream calls, want none", local.http.Load(), fra.http.Load())
	}
}

// /backend-api/codex/responses (what chatgpt_base_url clients send) gets the
// same retry stack, region rotation included, as /v1/responses.
func TestCodexRegionsBackendAPIPathUsesRegions(t *testing.T) {
	local := newRegionPool(t, "local", always(false))
	fra := newRegionPool(t, "fra", always(false))
	rig := newRegionRig(t, local, fra)
	rig.server.CodexFaultInjection = true
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()
	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/backend-api/codex/responses",
		strings.NewReader(`{"model":"gpt-6-astra","input":[],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Session", "backend-api-1")
	req.Header.Set(codexFaultHeader, "capacity=local")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	t.Logf("status=%d region=%q local=%d fra=%d body=%.120s", response.StatusCode, response.Header.Get(codexRegionHeader), local.http.Load(), fra.http.Load(), body)
	if local.http.Load() != 0 || fra.http.Load() != 1 || response.Header.Get(codexRegionHeader) == "" {
		t.Fatalf("want Frankfurt to serve the backend-api path under a local fault")
	}
}
