package tailnet

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseSameUserProofFromLsof(t *testing.T) {
	out := []byte("p7041\nn/dev/null\nn/Users/me/Library/Group Containers/W5364U7YZB.group.io.tailscale.ipn.macos/sameuserproof-57162-34645d814301b7f189fd\nnTCP 127.0.0.1:57162 (LISTEN)\n")
	endpoint, ok := parseSameUserProof(out)
	if !ok {
		t.Fatal("expected a LocalAPI endpoint")
	}
	if endpoint.network != "tcp" || endpoint.address != "127.0.0.1:57162" || endpoint.token != "34645d814301b7f189fd" {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	if _, ok := parseSameUserProof([]byte("p1\nn/dev/null\n")); ok {
		t.Fatal("no credential file must not yield an endpoint")
	}
}

// localAPIServer serves whois like tailscaled: 200 for a known peer, 404 for
// any other address, and 401 without the token.
func localAPIServer(t *testing.T, token string) localAPIEndpoint {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, got, _ := r.BasicAuth(); got != token || r.Header.Get("Sec-Tailscale") != "localapi" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/localapi/v0/whois" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("addr") == "100.82.214.112:1" {
			_, _ = w.Write([]byte(userWhois))
			return
		}
		http.Error(w, "no match for IP:port", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	return localAPIEndpoint{network: "tcp", address: strings.TrimPrefix(server.URL, "http://"), token: token}
}

func resolverWithLocalAPI(endpoint localAPIEndpoint, cli commandRunner) *Resolver {
	resolver := newTestResolver(cli)
	resolver.localAPI = &localAPIWhois{runner: cli, endpoint: &endpoint}
	return resolver
}

func TestLookupPrefersLocalAPIOverCLI(t *testing.T) {
	cli := &fakeRunner{err: errors.New("cli must not be reached")}
	identity, ok := resolverWithLocalAPI(localAPIServer(t, "tok"), cli).Lookup(context.Background(), "100.82.214.112:52344")
	if !ok || identity.LoginName != "lawrence@manaflow.ai" {
		t.Fatalf("identity = %+v ok = %v", identity, ok)
	}
	if cli.calls != 0 {
		t.Fatalf("CLI called %d times; LocalAPI answered", cli.calls)
	}
}

func TestLocalAPINotFoundIsDefinitiveAndNotLogged(t *testing.T) {
	var logs bytes.Buffer
	cli := &fakeRunner{err: errors.New("cli must not be reached")}
	resolver := resolverWithLocalAPI(localAPIServer(t, "tok"), cli)
	resolver.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	if _, ok := resolver.Lookup(context.Background(), "100.99.99.99:1"); ok {
		t.Fatal("unknown address resolved")
	}
	if cli.calls != 0 || logs.Len() != 0 {
		t.Fatalf("not-a-peer must not fall back or warn: calls=%d logs=%q", cli.calls, logs.String())
	}
}

// The failure that took the proxy down: LocalAPI unusable and the macOS app CLI
// unable to run from launchd. Both reasons reach the log, once.
func TestLookupFailureIsLoggedWithBothReasonsAndRateLimited(t *testing.T) {
	var logs bytes.Buffer
	dead := localAPIEndpoint{network: "tcp", address: closedAddress(t)}
	cli := &fakeRunner{output: "The Tailscale GUI failed to start", err: errors.New("exit status 1")}
	resolver := newTestResolver(cli)
	resolver.localAPI = &localAPIWhois{runner: &fakeRunner{}, endpoint: &dead}
	resolver.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	resolver.FailureTTL = time.Nanosecond

	for range 3 {
		if _, ok := resolver.Lookup(context.Background(), "100.82.214.112:1"); ok {
			t.Fatal("a failed lookup authorized a peer")
		}
		time.Sleep(time.Millisecond)
	}
	text := logs.String()
	if strings.Count(text, "tailnet identity lookup failed") != 1 {
		t.Fatalf("want exactly one warning, got:\n%s", text)
	}
	for _, want := range []string{"LocalAPI", "The Tailscale GUI failed to start"} {
		if !strings.Contains(text, want) {
			t.Fatalf("warning lacks %q:\n%s", want, text)
		}
	}
	if cli.calls != 3 {
		t.Fatalf("CLI calls = %d; a failure must be retried, not cached", cli.calls)
	}
}

func TestFailedLookupIsCachedBriefly(t *testing.T) {
	cli := &fakeRunner{err: errors.New("exit status 1")}
	resolver := newTestResolver(cli)
	now := time.Unix(1_700_000_000, 0)
	resolver.now = func() time.Time { return now }
	resolver.FailureTTL = 3 * time.Second
	resolver.Logger = slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	resolver.Lookup(context.Background(), "100.82.214.112:1")
	resolver.Lookup(context.Background(), "100.82.214.112:1")
	if cli.calls != 1 {
		t.Fatalf("calls = %d within the failure TTL, want 1", cli.calls)
	}
	cli.mu.Lock()
	cli.err = nil
	cli.output = userWhois
	cli.mu.Unlock()
	now = now.Add(4 * time.Second)
	if _, ok := resolver.Lookup(context.Background(), "100.82.214.112:1"); !ok {
		t.Fatal("resolver did not recover after the failure TTL")
	}
}

func TestLocalAPIRediscoversAfterTailscaleRestart(t *testing.T) {
	live := localAPIServer(t, "abc123")
	port := live.address[strings.LastIndex(live.address, ":")+1:]
	lsof := &fakeRunner{output: "n/x/sameuserproof-" + port + "-abc123\n"}
	stale := localAPIEndpoint{network: "tcp", address: closedAddress(t), token: "def456"}
	api := &localAPIWhois{runner: lsof, endpoint: &stale}
	if _, err := api.whois(context.Background(), "100.82.214.112"); err != nil {
		if strings.Contains(err.Error(), "unsupported on") {
			t.Skip("LocalAPI discovery is darwin/linux only")
		}
		t.Fatalf("whois after restart: %v", err)
	}
}

func closedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}
