package proxy

import (
	"bufio"
	"encoding/json"
	"errors"
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

func readWebSocketUntilCloseOrTurn(t *testing.T, conn *websocket.Conn) (string, int) {
	t.Helper()
	var got []string
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, body, err := conn.ReadMessage()
		if err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				return strings.Join(got, "\n"), closeErr.Code
			}
			t.Fatalf("read after %q: %v", got, err)
		}
		got = append(got, string(body))
		if strings.Contains(string(body), "response.completed") || strings.Contains(string(body), "response.failed") {
			return strings.Join(got, "\n"), 0
		}
	}
}

// A non-autonomous client (Codex Desktop) used to get a 1012 reconnect on
// its first capacity failure; a self-contained turn is now served from the
// next region behind the open socket.
func TestCodexRegionsNonAutonomousTurnServedFromNextRegion(t *testing.T) {
	local := newRegionPool(t, "local", always(true))
	fra := newRegionPool(t, "fra", always(false))
	rig := newRegionRig(t, local, fra)
	rig.server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	conn := dialRegionWebSocket(t, proxy.URL, "desktop-1", false)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	turn, code := readWebSocketUntilCloseOrTurn(t, conn)
	if code != 0 || !strings.Contains(turn, "served-from-fra") || strings.Contains(turn, "response.failed") {
		t.Fatalf("turn=%q close=%d, want Frankfurt on the open socket", turn, code)
	}
	events := readCapacityEvents(t, rig.server.CodexCapacityEventsPath)
	if countEvents(events, codexCapacityOutcomeAbsorbed, "") != 1 || countEvents(events, codexCapacityOutcomeClientVisible, "") != 0 {
		t.Fatalf("events=%+v, want the turn absorbed", events)
	}
}

// A session pinned to one account (X-Subrouter-Account-ID, the Ultrafast
// pin) keeps that account but still rotates regions: the capacity failure is
// served from the next region on the open socket, not closed 1012.
func TestCodexRegionsForcedAccountTurnServedFromNextRegion(t *testing.T) {
	local := newRegionPool(t, "local", always(true))
	fra := newRegionPool(t, "fra", always(false))
	rig := newRegionRig(t, local, fra)
	rig.server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	header := http.Header{"X-Subrouter-Session": []string{"desktop-forced"}, "X-Subrouter-Account-Id": []string{"codex-account-0"}}
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxy.URL, "http")+"/responses", header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = response.Body.Close()
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	turn, code := readWebSocketUntilCloseOrTurn(t, conn)
	if code != 0 || !strings.Contains(turn, "served-from-fra") || strings.Contains(turn, "response.failed") {
		t.Fatalf("turn=%q close=%d, want Frankfurt on the open socket", turn, code)
	}
	events := readCapacityEvents(t, rig.server.CodexCapacityEventsPath)
	if countEvents(events, codexCapacityOutcomeAbsorbed, "") != 1 || countEvents(events, codexCapacityOutcomeClientVisible, "") != 0 {
		t.Fatalf("events=%+v, want the turn absorbed", events)
	}
	for _, event := range events {
		if event.Account != "" && event.Account != "codex-account-0" {
			t.Fatalf("event %+v left the forced account", event)
		}
	}
}

// A turn chained on previous_response_id cannot leave its connection, so a
// non-autonomous client's capacity failure is resent on that same socket
// instead of the 1012 reconnect it used to get.
func TestCodexRegionsNonAutonomousChainedTurnResendsOnSameSocket(t *testing.T) {
	var localCreates atomic.Int32
	local := newRegionPool(t, "local", func() bool { return localCreates.Add(1) == 1 })
	fra := newRegionPool(t, "fra", always(false))
	rig := newRegionRig(t, local, fra)
	rig.server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	conn := dialRegionWebSocket(t, proxy.URL, "desktop-chained", false)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"resp_prev","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	turn, code := readWebSocketUntilCloseOrTurn(t, conn)
	if code != 0 || !strings.Contains(turn, "served-from-local") || strings.Contains(turn, "response.failed") {
		t.Fatalf("turn=%q close=%d, want the resend served on the same socket", turn, code)
	}
	if local.creates.Load() != 2 || fra.creates.Load() != 0 {
		t.Fatalf("local=%d fra=%d creates, want the failure and one resend on local", local.creates.Load(), fra.creates.Load())
	}
}

// A chained turn whose region keeps refusing stops after the budget.
func TestCodexRegionsNonAutonomousChainedTurnIsBounded(t *testing.T) {
	local := newRegionPool(t, "local", always(true))
	fra := newRegionPool(t, "fra", always(false))
	rig := newRegionRig(t, local, fra)
	rig.server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	conn := dialRegionWebSocket(t, proxy.URL, "desktop-chained-bounded", false)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"resp_prev","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	turn, code := readWebSocketUntilCloseOrTurn(t, conn)
	if local.creates.Load() != 1+codexWebSocketRegionRetries || fra.creates.Load() != 0 {
		t.Fatalf("local=%d fra=%d (turn=%q close=%d), want 1+%d on local only", local.creates.Load(), fra.creates.Load(), turn, code, codexWebSocketRegionRetries)
	}
}

// When every region refuses, a non-autonomous turn stops after the bounded
// region retries instead of waiting without end.
func TestCodexRegionsNonAutonomousRetriesAreBounded(t *testing.T) {
	local := newRegionPool(t, "local", always(true))
	fra := newRegionPool(t, "fra", always(true))
	tyo := newRegionPool(t, "tyo", always(true))
	rig := newRegionRig(t, local, fra, tyo)
	rig.server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	conn := dialRegionWebSocket(t, proxy.URL, "desktop-bounded", false)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	turn, code := readWebSocketUntilCloseOrTurn(t, conn)
	creates := local.creates.Load() + fra.creates.Load() + tyo.creates.Load()
	if creates != 1+codexWebSocketRegionRetries {
		t.Fatalf("creates=%d (turn=%q close=%d), want the first attempt plus %d region retries", creates, turn, code, codexWebSocketRegionRetries)
	}
	if code == 0 && !strings.Contains(turn, "response.failed") {
		t.Fatalf("turn=%q, want the failure or a close after the budget", turn)
	}
}

// An upstream that refuses a turn with a close of its own is not a capacity
// failure: a non-autonomous client still sees that close, unretried.
func TestCodexRegionsNonAutonomousUpstreamCloseIsNotRetried(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var creates atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		creates.Add(1)
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "policy"))
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	fra := newRegionPool(t, "fra", always(false))
	rig := newRegionRig(t, &regionPool{name: "local", url: upstreamURL}, fra)
	rig.server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	conn := dialRegionWebSocket(t, proxy.URL, "desktop-policy", false)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	_, code := readWebSocketUntilCloseOrTurn(t, conn)
	if code != websocket.ClosePolicyViolation || creates.Load() != 1 || fra.creates.Load() != 0 {
		t.Fatalf("close=%d upstream creates=%d fra=%d, want the 1008 passed through with no retry", code, creates.Load(), fra.creates.Load())
	}
}

// The upstream may evict previous_response_id when it fails a chained turn,
// so the resend can come back previous_response_not_found. That must reach
// the client as the 1012 reconnect it had before, never as a failed turn.
func TestCodexRegionsChainedResendFailureBecomesReconnect(t *testing.T) {
	for name, resendFailure := range map[string]string{
		"evicted":       `{"type":"error","error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response not found."}}`,
		"token_expired": `{"type":"error","status":401,"error":{"type":"invalid_request_error","code":"token_expired","message":"Provided authentication token is expired."}}`,
	} {
		t.Run(name, func(t *testing.T) {
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			var creates atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					if creates.Add(1) == 1 {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCapacityFailure))
						continue
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(resendFailure))
				}
			}))
			defer upstream.Close()
			upstreamURL, _ := url.Parse(upstream.URL)
			fra := newRegionPool(t, "fra", always(false))
			rig := newRegionRig(t, &regionPool{name: "local", url: upstreamURL}, fra)
			rig.server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
			proxy := httptest.NewServer(rig.server.Handler())
			defer proxy.Close()

			conn := dialRegionWebSocket(t, proxy.URL, "desktop-resend-"+name, false)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"resp_prev","input":[]}`)); err != nil {
				t.Fatal(err)
			}
			turn, code := readWebSocketUntilCloseOrTurn(t, conn)
			if code != websocket.CloseServiceRestart || strings.Contains(turn, `"error"`) {
				t.Fatalf("turn=%q close=%d, want a 1012 and no error event", turn, code)
			}
			// One resend only: a failed resend never spends the rest of the
			// budget on the same socket.
			if creates.Load() != 2 {
				t.Fatalf("upstream creates=%d, want the failure and one resend", creates.Load())
			}
		})
	}
}

// A resent chained turn whose budget runs out on capacity failures ends in a
// reconnect, not a failed turn, even when no reroute layer takes it.
func TestCodexRegionsChainedResendBudgetEndsInReconnect(t *testing.T) {
	local := newRegionPool(t, "local", always(true))
	fra := newRegionPool(t, "fra", always(false))
	rig := newRegionRig(t, local, fra)
	rig.server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	conn := dialRegionWebSocket(t, proxy.URL, "desktop-chained-exhausted", false)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"resp_prev","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	turn, code := readWebSocketUntilCloseOrTurn(t, conn)
	if code != websocket.CloseServiceRestart || strings.Contains(turn, "response.failed") {
		t.Fatalf("turn=%q close=%d, want a 1012 with no failure event", turn, code)
	}
}

// historyPool is one region's backend that answers each response.create
// with one output item (none for a generate:false prewarm) and a completed
// response named after the region, and keeps every body it received. Like
// the real upstream, a previous_response_id only resolves on the connection
// that produced it.
type historyPool struct {
	*regionPool
	failWith string
	mu       sync.Mutex
	bodies   []string
}

func newHistoryPool(t *testing.T, name string, fail func() bool) *historyPool {
	t.Helper()
	pool := &historyPool{regionPool: &regionPool{name: name, fail: fail}, failWith: turnRetryCapacityFailure}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	pool.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		held := map[string]bool{}
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			n := pool.creates.Add(1)
			pool.mu.Lock()
			pool.bodies = append(pool.bodies, string(body))
			pool.mu.Unlock()
			var create struct {
				PreviousResponseID string `json:"previous_response_id"`
				Generate           *bool  `json:"generate"`
			}
			_ = json.Unmarshal(body, &create)
			if create.PreviousResponseID != "" && !held[create.PreviousResponseID] {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response not found."}}`))
				continue
			}
			if pool.fail() {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(pool.failWith))
				continue
			}
			id := fmt.Sprintf("%s_%d", name, n)
			held["resp_"+id] = true
			if create.Generate == nil || *create.Generate {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_item.done","item":{"type":"message","id":"msg_`+id+`","role":"assistant","content":[{"type":"output_text","text":"served-from-`+name+`"}]}}`))
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_`+id+`"}}`))
		}
	}))
	t.Cleanup(pool.server.Close)
	pool.url, _ = url.Parse(pool.server.URL)
	return pool
}

func (p *historyPool) lastBody() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.bodies) == 0 {
		return ""
	}
	return p.bodies[len(p.bodies)-1]
}

func sendRegionTurn(t *testing.T, conn *websocket.Conn, create string) (string, int) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(create)); err != nil {
		t.Fatal(err)
	}
	return readWebSocketUntilCloseOrTurn(t, conn)
}

// fullInputOf is the input items of a response.create, and whether it was
// sent self-contained.
func fullInputOf(t *testing.T, body string) ([]string, bool) {
	t.Helper()
	var sent struct {
		PreviousResponseID *string           `json:"previous_response_id"`
		Input              []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatal(err)
	}
	items := make([]string, len(sent.Input))
	for i, item := range sent.Input {
		items[i] = string(item)
	}
	return items, sent.PreviousResponseID == nil
}

const (
	userOne   = `{"type":"message","role":"user","content":"one"}`
	userTwo   = `{"type":"message","role":"user","content":"two"}`
	userThree = `{"type":"message","role":"user","content":"three"}`
)

func chainedCreate(previous, input string) string {
	return `{"type":"response.create","model":"gpt-6-astra","instructions":"be brief","previous_response_id":"` + previous + `","input":[` + input + `]}`
}

// A turn chained on the connection's last response used to be resent only
// on that connection, in the region that just refused it, and a second
// refusal became a 1012 reconnect. The relay saw the whole conversation, so
// it now sends the turn self-contained from the next region.
func TestCodexRegionsChainedTurnRebuiltForNextRegion(t *testing.T) {
	for _, autonomous := range []bool{false, true} {
		t.Run(fmt.Sprintf("autonomous=%v", autonomous), func(t *testing.T) {
			var localCreates atomic.Int32
			local := newHistoryPool(t, "local", func() bool { return localCreates.Add(1) > 1 })
			fra := newHistoryPool(t, "fra", always(false))
			rig := newRegionRig(t, local.regionPool, fra.regionPool)
			rig.server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
			proxy := httptest.NewServer(rig.server.Handler())
			defer proxy.Close()

			conn := dialRegionWebSocket(t, proxy.URL, fmt.Sprintf("desktop-rebuild-%v", autonomous), autonomous)
			if turn, code := sendRegionTurn(t, conn, `{"type":"response.create","model":"gpt-6-astra","instructions":"be brief","input":[`+userOne+`]}`); code != 0 || !strings.Contains(turn, "resp_local_1") {
				t.Fatalf("first turn=%q close=%d, want it served from local", turn, code)
			}
			turn, code := sendRegionTurn(t, conn, chainedCreate("resp_local_1", userTwo))
			if code != 0 || !strings.Contains(turn, "resp_fra_1") || strings.Contains(turn, "response.failed") {
				t.Fatalf("chained turn=%q close=%d, want it served from Frankfurt on the open socket", turn, code)
			}
			items, selfContained := fullInputOf(t, fra.lastBody())
			if !selfContained || !strings.Contains(fra.lastBody(), `"instructions":"be brief"`) || len(items) != 3 ||
				!strings.Contains(items[0], `"one"`) || !strings.Contains(items[1], "msg_local_1") || !strings.Contains(items[2], `"two"`) {
				t.Fatalf("Frankfurt got %s, want the full conversation without previous_response_id", fra.lastBody())
			}
			if local.creates.Load() != 2 {
				t.Fatalf("local creates=%d, want the first turn and the refused chained turn only", local.creates.Load())
			}
			// The next turn chains on the Frankfurt response and goes out
			// as is: the new connection holds it.
			if turn, code := sendRegionTurn(t, conn, chainedCreate("resp_fra_1", userThree)); code != 0 || !strings.Contains(turn, "resp_fra_2") {
				t.Fatalf("third turn=%q close=%d, want it served from Frankfurt", turn, code)
			}
			events := readCapacityEvents(t, rig.server.CodexCapacityEventsPath)
			if countEvents(events, codexCapacityOutcomeClientVisible, "") != 0 {
				t.Fatalf("events=%+v, want no client-visible failure", events)
			}
		})
	}
}

// A prewarm (generate:false) leaves its input and no output on the
// connection; the chained turn after it is rebuilt from that input.
func TestCodexRegionsChainedTurnAfterPrewarmRebuilt(t *testing.T) {
	var localCreates atomic.Int32
	local := newHistoryPool(t, "local", func() bool { return localCreates.Add(1) > 1 })
	fra := newHistoryPool(t, "fra", always(false))
	rig := newRegionRig(t, local.regionPool, fra.regionPool)
	rig.server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	conn := dialRegionWebSocket(t, proxy.URL, "desktop-prewarm", false)
	if turn, code := sendRegionTurn(t, conn, `{"type":"response.create","model":"gpt-6-astra","generate":false,"input":[`+userOne+`]}`); code != 0 || !strings.Contains(turn, "resp_local_1") {
		t.Fatalf("prewarm=%q close=%d", turn, code)
	}
	if turn, code := sendRegionTurn(t, conn, chainedCreate("resp_local_1", "")); code != 0 || !strings.Contains(turn, "served-from-fra") {
		t.Fatalf("chained turn=%q close=%d, want it served from Frankfurt", turn, code)
	}
	items, selfContained := fullInputOf(t, fra.lastBody())
	if !selfContained || len(items) != 1 || !strings.Contains(items[0], `"one"`) {
		t.Fatalf("Frankfurt got %s, want the prewarm input only", fra.lastBody())
	}
}

// A turn chained on a response the relay has no history for (not the last
// completed one) keeps the same-socket resend.
func TestCodexRegionsChainedTurnWithoutHistoryResendsOnSameSocket(t *testing.T) {
	var localCreates atomic.Int32
	local := newHistoryPool(t, "local", func() bool { return localCreates.Add(1) == 3 })
	fra := newHistoryPool(t, "fra", always(false))
	rig := newRegionRig(t, local.regionPool, fra.regionPool)
	rig.server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	conn := dialRegionWebSocket(t, proxy.URL, "desktop-no-history", false)
	for _, create := range []string{
		`{"type":"response.create","model":"gpt-6-astra","input":[` + userOne + `]}`,
		`{"type":"response.create","model":"gpt-6-astra","input":[` + userTwo + `]}`,
	} {
		if turn, code := sendRegionTurn(t, conn, create); code != 0 || !strings.Contains(turn, "response.completed") {
			t.Fatalf("turn=%q close=%d", turn, code)
		}
	}
	if turn, code := sendRegionTurn(t, conn, chainedCreate("resp_local_1", userThree)); code != 0 || !strings.Contains(turn, "served-from-local") {
		t.Fatalf("chained turn=%q close=%d, want the resend served on the same socket", turn, code)
	}
	if fra.creates.Load() != 0 || !strings.Contains(local.lastBody(), `"previous_response_id":"resp_local_1"`) {
		t.Fatalf("fra=%d local last=%s, want the chained body resent on local", fra.creates.Load(), local.lastBody())
	}
}

// A rebuilt turn the next region rejects for another reason ends in the
// 1012 reconnect a chained resend gets, never in an error event: Codex then
// sends its own full request.
func TestCodexRegionsRebuiltTurnRejectionBecomesReconnect(t *testing.T) {
	var localCreates atomic.Int32
	local := newHistoryPool(t, "local", func() bool { return localCreates.Add(1) > 1 })
	fra := newHistoryPool(t, "fra", always(true))
	fra.failWith = `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be verified."}}`
	rig := newRegionRig(t, local.regionPool, fra.regionPool)
	rig.server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	conn := dialRegionWebSocket(t, proxy.URL, "desktop-rebuild-rejected", false)
	if turn, code := sendRegionTurn(t, conn, `{"type":"response.create","model":"gpt-6-astra","input":[`+userOne+`]}`); code != 0 || !strings.Contains(turn, "resp_local_1") {
		t.Fatalf("first turn=%q close=%d", turn, code)
	}
	turn, code := sendRegionTurn(t, conn, chainedCreate("resp_local_1", userTwo))
	if code != websocket.CloseServiceRestart || strings.Contains(turn, `"error"`) {
		t.Fatalf("turn=%q close=%d, want a 1012 and no error event", turn, code)
	}
	if fra.creates.Load() != 1 {
		t.Fatalf("fra creates=%d, want the one rebuilt send", fra.creates.Load())
	}
}

// An autonomous turn whose account sheds in two regions used to switch
// account at once: a 1012 its client sees as a reconnect. It now tries the
// rest of the region budget on the same account first.
func TestCodexRegionsAutonomousTurnTriesRegionsBeforeAccountSwitch(t *testing.T) {
	local := newRegionPool(t, "local", always(true))
	fra := newRegionPool(t, "fra", always(true))
	ams := newRegionPool(t, "ams", always(false))
	rig := newRegionRig(t, local, fra, ams)
	proxies := make([]*url.URL, 0, 2)
	for _, region := range []*regionPool{fra, ams} {
		proxies = append(proxies, regionProxy(t, region.name, region.url, &atomic.Int32{}))
	}
	rig.server = codexEgressServer(t, local.url, proxies, 2)
	rig.server.CodexEgress.PinStorePath = ""
	rig.server.CodexCapacityEventsPath = filepath.Join(t.TempDir(), "codex-capacity-events.jsonl")
	rig.server.CodexOverloadFailover = &CodexOverloadFailoverConfig{Enabled: true}
	fastCapacityGaps(rig.server.CodexOverloadFailover, time.Millisecond)
	rig.server.codexWebSocketTurnRetryDelay = noTurnRetryDelay
	proxy := httptest.NewServer(rig.server.Handler())
	defer proxy.Close()

	conn := dialRegionWebSocket(t, proxy.URL, "auto-regions-first", true)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	turn, code := readWebSocketUntilCloseOrTurn(t, conn)
	if code != 0 || !strings.Contains(turn, "served-from-ams") {
		t.Fatalf("turn=%q close=%d, want Amsterdam on the open socket", turn, code)
	}
	events := readCapacityEvents(t, rig.server.CodexCapacityEventsPath)
	if countEvents(events, codexCapacityOutcomeClientVisible, "") != 0 {
		t.Fatalf("events=%+v, want no client-visible failure", events)
	}
}
