package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

type testRoundTripFunc func(*http.Request) (*http.Response, error)

func (f testRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestTailscaleServerURLPreservesURLShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		url  string
		node tailscaleNodeStatus
		want string
	}{
		{
			name: "magic dns keeps port path and query",
			url:  "http://retired.example:31415/t/tenant?mode=one",
			node: tailscaleNodeStatus{ID: "node-1", DNSName: "current.example."},
			want: "http://current.example:31415/t/tenant?mode=one",
		},
		{
			name: "query trailing slash is not normalized as path",
			url:  "http://retired.example:31415/t/tenant/?redirect=/",
			node: tailscaleNodeStatus{ID: "node-1", DNSName: "current.example."},
			want: "http://current.example:31415/t/tenant?redirect=/",
		},
		{
			name: "ipv4 fallback",
			url:  "https://retired.example/control",
			node: tailscaleNodeStatus{ID: "node-1", TailscaleIPs: []string{"100.64.0.9"}},
			want: "https://100.64.0.9/control",
		},
		{
			name: "ipv6 fallback is bracketed",
			url:  "http://retired.example:31415/v1",
			node: tailscaleNodeStatus{ID: "node-1", TailscaleIPs: []string{"fd7a:115c:a1e0::1"}},
			want: "http://[fd7a:115c:a1e0::1]:31415/v1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := tailscaleServerURL(test.url, test.node)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("URL = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTailscaleServerURLParseErrorRedactsStoredURLAndTenantKey(t *testing.T) {
	secret := "srt_super_secret"
	storedURL := "http://user:" + secret + "@host.invalid/%zz/t/" + secret
	_, err := tailscaleServerURLs(storedURL, tailscaleNodeStatus{ID: "node-1", DNSName: "current.example."})
	if err == nil {
		t.Fatal("malformed stored URL unexpectedly parsed")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "user:") || strings.Contains(err.Error(), "%zz") {
		t.Fatalf("parse error leaked stored URL or key: %v", err)
	}
	if !strings.Contains(err.Error(), "value redacted") {
		t.Fatalf("parse error = %v, want explicit redaction", err)
	}
}

func TestHealTailscaleServerParseErrorRedactsStoredURLAndTenantKey(t *testing.T) {
	secret := "srt_super_secret"
	server := srServerConfig{
		Name: "team", URL: "http://host.invalid/%zz/t/" + secret,
		TenantKey: secret, TailscaleNodeID: "node-1",
	}
	status, err := json.Marshal(tailscaleStatusDocument{Peer: map[string]tailscaleNodeStatus{
		"peer": {ID: "node-1", DNSName: "current.example."},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	_, err = healTailscaleServer(t.Context(), srServerStore{}, server, &http.Client{}, &warnings,
		func(context.Context) ([]byte, error) { return status, nil })
	if err == nil {
		t.Fatal("malformed stored URL unexpectedly healed")
	}
	combined := err.Error() + "\n" + warnings.String()
	if strings.Contains(combined, secret) || strings.Contains(combined, "%zz") {
		t.Fatalf("repair diagnostics leaked stored URL or key: %s", combined)
	}
}

func TestHealTailscaleServerRepairsExactHealthyNode(t *testing.T) {
	t.Parallel()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/health" {
			t.Errorf("health path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(healthy.Close)
	parsed, err := url.Parse(healthy.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	server := srServerConfig{
		Name:            "team",
		URL:             "http://retired.invalid:" + port + "/t/example",
		TailscaleNodeID: "node-stable",
	}
	if err := store.save(srServerFile{Default: server.Name, Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	status, err := json.Marshal(tailscaleStatusDocument{Peer: map[string]tailscaleNodeStatus{
		"peer-key": {ID: "node-stable", DNSName: "127.0.0.1.", Online: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	got, err := healTailscaleServer(
		context.Background(), store, server, healthy.Client(), &warnings,
		func(context.Context) ([]byte, error) { return status, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:" + port + "/t/example"
	if got.URL != want {
		t.Fatalf("healed URL = %q, want %q", got.URL, want)
	}
	stored, ok, err := store.find(server.Name)
	if err != nil || !ok {
		t.Fatalf("find stored server: ok=%v err=%v", ok, err)
	}
	if stored.URL != want || stored.TailscaleNodeID != server.TailscaleNodeID {
		t.Fatalf("stored server = %+v", stored)
	}
	if !strings.Contains(warnings.String(), "updated") || !strings.Contains(warnings.String(), "node-stable") {
		t.Fatalf("warning = %q", warnings.String())
	}
}

func TestHealTailscaleServerDoesNotGuessNode(t *testing.T) {
	t.Parallel()
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	server := srServerConfig{Name: "team", URL: "http://retired.invalid:31415", TailscaleNodeID: "wanted-node"}
	if err := store.save(srServerFile{Default: server.Name, Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	status, err := json.Marshal(tailscaleStatusDocument{Peer: map[string]tailscaleNodeStatus{
		"similar-name": {ID: "different-node", DNSName: "team.invalid.", Online: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	_, err = healTailscaleServer(
		context.Background(), store, server, &http.Client{}, &warnings,
		func(context.Context) ([]byte, error) { return status, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "node ID wanted-node not found") {
		t.Fatalf("repair error = %v", err)
	}
	stored, ok, findErr := store.find(server.Name)
	if findErr != nil || !ok {
		t.Fatalf("find stored server: ok=%v err=%v", ok, findErr)
	}
	if stored.URL != server.URL {
		t.Fatalf("unmatched node changed stored URL to %q", stored.URL)
	}
	if !strings.Contains(warnings.String(), "node ID wanted-node not found") {
		t.Fatalf("warning = %q", warnings.String())
	}
}

func TestHealTailscaleServerDoesNotTrustHealthyURLOwnedByAnotherNode(t *testing.T) {
	t.Parallel()
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	server := srServerConfig{Name: "team", URL: "http://reused.example:31415", TailscaleNodeID: "wanted-node"}
	if err := store.save(srServerFile{Default: server.Name, Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	status, err := json.Marshal(tailscaleStatusDocument{Peer: map[string]tailscaleNodeStatus{
		"peer": {ID: "wanted-node", DNSName: "current.example.", Online: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var probed []string
	client := &http.Client{Transport: testRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		probed = append(probed, request.URL.Hostname())
		mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}
	got, err := healTailscaleServer(
		context.Background(), store, server, client, io.Discard,
		func(context.Context) ([]byte, error) { return status, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "http://current.example:31415" {
		t.Fatalf("healed URL = %q", got.URL)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(probed, ",") != "current.example" {
		t.Fatalf("health probes = %v; reused host must not be contacted", probed)
	}
}

func TestHealTailscaleServerFallsBackFromMagicDNSToNodeIP(t *testing.T) {
	t.Parallel()
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	server := srServerConfig{Name: "team", URL: "http://retired.example:31415", TailscaleNodeID: "wanted-node"}
	if err := store.save(srServerFile{Default: server.Name, Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	status, err := json.Marshal(tailscaleStatusDocument{Peer: map[string]tailscaleNodeStatus{
		"peer": {
			ID: "wanted-node", DNSName: "unresolved.example.",
			TailscaleIPs: []string{"100.64.0.9", "fd7a:115c:a1e0::9"}, Online: true,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var probed []string
	client := &http.Client{Transport: testRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		probed = append(probed, request.URL.Hostname())
		if request.URL.Hostname() != "100.64.0.9" {
			return nil, &net.DNSError{Name: request.URL.Hostname(), Err: "unresolved"}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}
	got, err := healTailscaleServer(
		context.Background(), store, server, client, io.Discard,
		func(context.Context) ([]byte, error) { return status, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "http://100.64.0.9:31415" {
		t.Fatalf("healed URL = %q", got.URL)
	}
	if strings.Join(probed, ",") != "unresolved.example,100.64.0.9" {
		t.Fatalf("health probes = %v", probed)
	}
}

func TestHealTailscaleServerGivesLaterAdvertisedEndpointAFullProbeBudget(t *testing.T) {
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	server := srServerConfig{Name: "team", URL: "http://retired.example:31415", TailscaleNodeID: "wanted-node"}
	if err := store.save(srServerFile{Default: server.Name, Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	status, err := json.Marshal(tailscaleStatusDocument{Peer: map[string]tailscaleNodeStatus{
		"peer": {
			ID: "wanted-node", DNSName: "slow-dns.example.",
			TailscaleIPs: []string{"100.64.0.8", "100.64.0.9"}, Online: true,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var probed []string
	client := &http.Client{Transport: testRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		host := request.URL.Hostname()
		probed = append(probed, host)
		if host != "100.64.0.9" {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		time.Sleep(150 * time.Millisecond)
		if err := request.Context().Err(); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}
	got, err := healTailscaleServer(
		context.Background(), store, server, client, io.Discard,
		func(context.Context) ([]byte, error) {
			time.Sleep(1900 * time.Millisecond)
			return status, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "http://100.64.0.9:31415" {
		t.Fatalf("healed URL = %q", got.URL)
	}
	if strings.Join(probed, ",") != "slow-dns.example,100.64.0.8,100.64.0.9" {
		t.Fatalf("health probes = %v", probed)
	}
}

func TestTailscaleNodeByIDAcceptsExactPeerWithStaleOfflineFlag(t *testing.T) {
	t.Parallel()
	status, err := json.Marshal(tailscaleStatusDocument{Peer: map[string]tailscaleNodeStatus{
		"peer": {ID: "node-1", DNSName: "node.invalid.", TailscaleIPs: []string{"100.64.0.9"}, Online: false},
	}})
	if err != nil {
		t.Fatal(err)
	}
	node, ok, err := tailscaleNodeByID(status, "node-1")
	if err != nil || !ok || node.ID != "node-1" {
		t.Fatalf("node result = %+v ok=%v err=%v", node, ok, err)
	}
}

func TestHealTailscaleServerUsesHealthNotStaleOnlineFlag(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(healthy.Close)
	parsed, err := url.Parse(healthy.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	server := srServerConfig{Name: "team", URL: "http://retired.invalid:" + port, TailscaleNodeID: "node-1"}
	if err := store.save(srServerFile{Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	status, err := json.Marshal(tailscaleStatusDocument{Peer: map[string]tailscaleNodeStatus{
		"peer": {ID: "node-1", DNSName: "127.0.0.1.", Online: false},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := healTailscaleServer(
		context.Background(), store, server, healthy.Client(), io.Discard,
		func(context.Context) ([]byte, error) { return status, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://127.0.0.1:" + port; got.URL != want {
		t.Fatalf("healed URL = %q, want %q", got.URL, want)
	}
}

func TestHealTailscaleServerFailsWhenExactOfflinePeerHasNoHealthyEndpoint(t *testing.T) {
	t.Parallel()
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	server := srServerConfig{Name: "team", URL: "http://retired.invalid:31415", TailscaleNodeID: "node-1"}
	if err := store.save(srServerFile{Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	status, err := json.Marshal(tailscaleStatusDocument{Peer: map[string]tailscaleNodeStatus{
		"peer": {ID: "node-1", DNSName: "unreachable.invalid.", Online: false},
	}})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: testRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, &net.DNSError{Name: request.URL.Hostname(), Err: "unreachable"}
	})}
	_, err = healTailscaleServer(
		context.Background(), store, server, client, io.Discard,
		func(context.Context) ([]byte, error) { return status, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "no discovered endpoint") {
		t.Fatalf("repair error = %v", err)
	}
}

func TestCompareAndSwapServerURLPreservesConcurrentUpdate(t *testing.T) {
	t.Parallel()
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	server := srServerConfig{Name: "team", URL: "http://newer.invalid:31415", TailscaleNodeID: "node-1"}
	if err := store.save(srServerFile{Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	_, err := store.compareAndSwapServerURL("team", "node-1", "http://old.invalid:31415", "http://candidate.invalid:31415")
	if err == nil || !strings.Contains(err.Error(), "changed concurrently") {
		t.Fatalf("compare-and-swap error = %v", err)
	}
	stored, ok, findErr := store.find("team")
	if findErr != nil || !ok {
		t.Fatalf("find server: ok=%v err=%v", ok, findErr)
	}
	if stored.URL != server.URL {
		t.Fatalf("concurrent URL overwritten: %q", stored.URL)
	}
}

func TestFindTailscaleBinaryHonorsExecutableOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tailscale-test")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tailscaleBinaryEnv, path)
	got, err := findTailscaleBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("binary = %q, want %q", got, path)
	}
}

func TestDefaultTailscaleStatusLoaderTimesOut(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "tailscale-test")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tailscaleBinaryEnv, binary)
	started := time.Now()
	_, err := defaultTailscaleStatusLoader(context.Background())
	if err == nil {
		t.Fatal("status loader unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("status loader took %s, want bounded near %s", elapsed, tailscaleStatusTimeout)
	}
}

func TestDefaultTailscaleStatusLoaderForcesCLIMode(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "tailscale-test")
	script := "#!/bin/sh\n[ \"$TAILSCALE_BE_CLI\" = true ] || exit 42\nprintf '{\"Self\":{}}\\n'\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tailscaleBinaryEnv, binary)
	t.Setenv("TAILSCALE_BE_CLI", "0")
	if got, err := findTailscaleBinary(); err != nil || got != binary {
		t.Fatalf("binary = %q, %v; want %q", got, err, binary)
	}
	output, err := defaultTailscaleStatusLoader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(output)) != `{"Self":{}}` {
		t.Fatalf("status output = %q", output)
	}
}

func TestCodexBaseURLRepairsDefaultServerThroughTailscale(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(healthy.Close)
	parsed, err := url.Parse(healthy.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	status, err := json.Marshal(tailscaleStatusDocument{Peer: map[string]tailscaleNodeStatus{
		"peer": {ID: "node-stable", DNSName: "127.0.0.1.", Online: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "tailscale")
	script := "#!/bin/sh\nprintf '%s\\n' '" + strings.ReplaceAll(string(status), "'", "'\\''") + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tailscaleBinaryEnv, binary)
	t.Setenv("SUBROUTER_CODEX_BASE_URL", "")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	server := srServerConfig{
		Name:            "team",
		URL:             "http://retired.invalid:" + port,
		TailscaleNodeID: "node-stable",
	}
	if err := store.save(srServerFile{Default: server.Name, Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	got, err := codexBaseURLWithTailscaleHealing(store, &warnings)
	if err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:" + port + "/v1"
	if got != want {
		t.Fatalf("Codex base URL = %q, want %q", got, want)
	}
	if !strings.Contains(warnings.String(), "updated") {
		t.Fatalf("warnings = %q", warnings.String())
	}
}

func TestSelectedRemoteServerRepairsThroughTailscale(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(healthy.Close)
	parsed, err := url.Parse(healthy.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	status, err := json.Marshal(tailscaleStatusDocument{Peer: map[string]tailscaleNodeStatus{
		"peer": {ID: "node-stable", DNSName: "127.0.0.1.", Online: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "tailscale")
	script := "#!/bin/sh\nprintf '%s\\n' '" + strings.ReplaceAll(string(status), "'", "'\\''") + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tailscaleBinaryEnv, binary)
	t.Setenv("SUBROUTER_SERVER", "")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	root := t.TempDir()
	accountStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	store := defaultSRServerStore(accountStore)
	server := srServerConfig{
		Name:            "team",
		URL:             "http://retired.invalid:" + port,
		TailscaleNodeID: "node-stable",
	}
	if err := store.save(srServerFile{Default: server.Name, Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	runner := srRunner{store: accountStore, client: healthy.Client(), errOut: &warnings}
	got, ok, err := runner.selectedRemoteServer()
	if err != nil || !ok {
		t.Fatalf("selected server: ok=%v err=%v", ok, err)
	}
	want := "http://127.0.0.1:" + port
	if got.URL != want {
		t.Fatalf("selected URL = %q, want %q", got.URL, want)
	}
	if !strings.Contains(warnings.String(), "updated") {
		t.Fatalf("warnings = %q", warnings.String())
	}
}

func TestNamedRemoteServerRepairsThroughTailscale(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(healthy.Close)
	parsed, err := url.Parse(healthy.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	status, err := json.Marshal(tailscaleStatusDocument{Peer: map[string]tailscaleNodeStatus{
		"peer": {ID: "node-stable", DNSName: "127.0.0.1.", Online: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "tailscale")
	script := "#!/bin/sh\nprintf '%s\\n' '" + strings.ReplaceAll(string(status), "'", "'\\''") + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tailscaleBinaryEnv, binary)
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	server := srServerConfig{
		Name:            "team",
		URL:             "http://retired.invalid:" + port,
		TailscaleNodeID: "node-stable",
	}
	if err := store.save(srServerFile{Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{client: healthy.Client(), errOut: io.Discard}
	got, err := runner.namedRemoteServer(context.Background(), store, "team")
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://127.0.0.1:" + port; got.URL != want {
		t.Fatalf("named server URL = %q, want %q", got.URL, want)
	}
}

func TestServerAddPreservesTailscaleIdentityForSameURLAndClearsItForReplacement(t *testing.T) {
	t.Parallel()
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	runner := srRunner{out: io.Discard, errOut: io.Discard}
	if err := runner.serverAdd(store, []string{
		"team", "--url", "http://one.invalid:31415", "--tailscale-node-id", "node-one", "--no-codex-config",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.serverAdd(store, []string{
		"team", "--url", "http://one.invalid:31415/", "--no-codex-config",
	}); err != nil {
		t.Fatal(err)
	}
	server, ok, err := store.find("team")
	if err != nil || !ok {
		t.Fatalf("find server: ok=%v err=%v", ok, err)
	}
	if server.TailscaleNodeID != "node-one" {
		t.Fatalf("node ID = %q, want preserved node-one", server.TailscaleNodeID)
	}
	if err := runner.serverAdd(store, []string{
		"team", "--url", "http://two.invalid:31415", "--no-codex-config",
	}); err != nil {
		t.Fatal(err)
	}
	server, ok, err = store.find("team")
	if err != nil || !ok {
		t.Fatalf("find updated server: ok=%v err=%v", ok, err)
	}
	if server.TailscaleNodeID != "" {
		t.Fatalf("node ID = %q, want cleared after URL replacement", server.TailscaleNodeID)
	}
	if err := runner.serverAdd(store, []string{
		"team", "--url", "http://three.invalid:31415", "--tailscale-node-id", "node-three", "--no-codex-config",
	}); err != nil {
		t.Fatal(err)
	}
	server, ok, err = store.find("team")
	if err != nil || !ok {
		t.Fatalf("find explicitly rebound server: ok=%v err=%v", ok, err)
	}
	if server.TailscaleNodeID != "node-three" {
		t.Fatalf("node ID = %q, want node-three", server.TailscaleNodeID)
	}
}

func TestCodexDefaultTailscaleDiscoveryFailureFallsBackLocally(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(local.Close)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	t.Setenv("SUBROUTER_CODEX_BASE_URL", "")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	t.Setenv("SUBROUTER_DISABLE_FALLBACK", "")
	t.Setenv(tailscaleBinaryEnv, filepath.Join(t.TempDir(), "missing-tailscale"))

	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	server := srServerConfig{
		Name:            "team",
		URL:             "http://retired.invalid:31415",
		TailscaleNodeID: "node-stable",
	}
	if err := store.save(srServerFile{Default: server.Name, Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	got, err := codexBaseURLWithFallback(store, &warnings)
	if err != nil {
		t.Fatal(err)
	}
	if want := local.URL + "/v1"; got != want {
		t.Fatalf("Codex base URL = %q, want local fallback %q", got, want)
	}
	if !strings.Contains(warnings.String(), "cannot safely resolve") {
		t.Fatalf("warnings = %q", warnings.String())
	}
}

func TestCodexRegistryErrorDoesNotFallBackLocally(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(local.Close)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	t.Setenv("SUBROUTER_CODEX_BASE_URL", "")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	t.Setenv("SUBROUTER_DISABLE_FALLBACK", "")
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if err := os.WriteFile(store.Path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := codexBaseURLWithFallback(store, io.Discard)
	if err == nil || got != "" {
		t.Fatalf("registry error resolved to %q, %v; want original failure", got, err)
	}
}

func TestCodexPinnedTailscaleDiscoveryFailureDoesNotFallBackLocally(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(local.Close)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	t.Setenv("SUBROUTER_CODEX_BASE_URL", "")
	t.Setenv("SUBROUTER_CODEX_SERVER", "team")
	t.Setenv("SUBROUTER_DISABLE_FALLBACK", "")
	t.Setenv(tailscaleBinaryEnv, filepath.Join(t.TempDir(), "missing-tailscale"))

	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	server := srServerConfig{
		Name:            "team",
		URL:             "http://retired.invalid:31415",
		TailscaleNodeID: "node-stable",
	}
	if err := store.save(srServerFile{Default: server.Name, Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	got, err := codexBaseURLWithFallback(store, io.Discard)
	if err == nil || got != "" {
		t.Fatalf("pinned discovery = %q, %v; want fail closed", got, err)
	}
}

func TestTailscaleRepairDiagnosticsRedactURLUserinfo(t *testing.T) {
	if got := redactedServerURL("http://user:secret@example.invalid:31415/t/team?api_key=query-secret#fragment-secret"); strings.Contains(got, "secret") || strings.Contains(got, "api_key") || strings.Contains(got, "/team") || !strings.Contains(got, "xxxxx") || !strings.Contains(got, "/t/[redacted]") || strings.Contains(got, "%5Bredacted%5D") {
		t.Fatalf("redacted URL = %q", got)
	}
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	server := srServerConfig{Name: "team", URL: "http://user:secret@newer.invalid:31415", TailscaleNodeID: "node-1"}
	if err := store.save(srServerFile{Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	_, err := store.compareAndSwapServerURL(
		"team", "node-1",
		"http://user:old-secret@old.invalid:31415",
		"http://user:new-secret@candidate.invalid:31415",
	)
	if err == nil {
		t.Fatal("concurrent update unexpectedly succeeded")
	}
	for _, secret := range []string{"secret", "old-secret", "new-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("concurrent update error exposed %q: %v", secret, err)
		}
	}
}

func TestDefaultTailscaleStatusLoaderForksOncePerProcess(t *testing.T) {
	resetTailscaleStatusMemoForTest()
	t.Cleanup(resetTailscaleStatusMemoForTest)
	dir := t.TempDir()
	counter := filepath.Join(dir, "calls")
	binary := filepath.Join(dir, "tailscale-test")
	script := "#!/bin/sh\necho x >> " + counter + "\nprintf '{\"Self\":{\"ID\":\"node-1\"}}\\n'\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tailscaleBinaryEnv, binary)
	for i := 0; i < 3; i++ {
		output, err := defaultTailscaleStatusLoader(context.Background())
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !strings.Contains(string(output), "node-1") {
			t.Fatalf("call %d output = %q", i, output)
		}
	}
	calls, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(calls), "x"); got != 1 {
		t.Fatalf("tailscale forks = %d, want 1", got)
	}
}

func TestDefaultTailscaleStatusLoaderMemoizesTimeoutButNotCallerCancel(t *testing.T) {
	resetTailscaleStatusMemoForTest()
	t.Cleanup(resetTailscaleStatusMemoForTest)
	binary := filepath.Join(t.TempDir(), "tailscale-test")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tailscaleBinaryEnv, binary)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := defaultTailscaleStatusLoader(cancelled); err == nil {
		t.Fatal("cancelled probe unexpectedly succeeded")
	}
	tailscaleStatusMemo.mu.Lock()
	memoized := len(tailscaleStatusMemo.results)
	tailscaleStatusMemo.mu.Unlock()
	if memoized != 0 {
		t.Fatal("a caller-cancelled probe must not be memoized")
	}

	if _, err := defaultTailscaleStatusLoader(context.Background()); err == nil {
		t.Fatal("slow probe unexpectedly succeeded")
	}
	started := time.Now()
	if _, err := defaultTailscaleStatusLoader(context.Background()); err == nil {
		t.Fatal("memoized probe unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("second probe took %s; the timed-out result should be memoized", elapsed)
	}
}
