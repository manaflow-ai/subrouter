package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/tenant"
)

const testTenantKey = "srt_0123456789abcdef0123456789abcdef"

func TestSRServerAddStoresTenantKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out}
	err := runner.run(context.Background(), []string{
		"server", "add", "hosted",
		"--url", "https://router.example",
		"--tenant-key", testTenantKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	server, ok, err := defaultSRServerStore(store).find("hosted")
	if err != nil || !ok {
		t.Fatalf("server missing: %v", err)
	}
	if server.TenantKey != testTenantKey {
		t.Fatalf("tenant key = %q", server.TenantKey)
	}

	// Updating other metadata without --tenant-key preserves the stored key.
	err = runner.run(context.Background(), []string{
		"server", "add", "hosted",
		"--url", "https://router.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	server, _, _ = defaultSRServerStore(store).find("hosted")
	if server.TenantKey != testTenantKey {
		t.Fatalf("tenant key not preserved: %q", server.TenantKey)
	}
}

func TestSRServerAddRejectsMalformedTenantKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer
	runner := srRunner{store: accounts.DefaultCodexStore(), out: &out, errOut: &out}
	err := runner.run(context.Background(), []string{
		"server", "add", "hosted",
		"--url", "http://100.64.0.1:31415",
		"--tenant-key", "srt_nothex",
	})
	if err == nil || !strings.Contains(err.Error(), "srt_") {
		t.Fatalf("err = %v", err)
	}
}

func TestTenantScopedBaseURLs(t *testing.T) {
	server := srServerConfig{Name: "hosted", URL: "https://router.example", TenantKey: testTenantKey}
	got, err := codexBaseURLForServer(server)
	if err != nil || got != "https://router.example/t/"+testTenantKey+"/v1" {
		t.Fatalf("codex base URL = %q", got)
	}
	got, err = serverControlBaseURL(server)
	if err != nil || got != "https://router.example/t/"+testTenantKey {
		t.Fatalf("control base URL = %q", got)
	}
	// A stored URL that already ends in /v1 keeps the tenant prefix at the root.
	server.URL = "https://router.example/v1"
	got, err = codexBaseURLForServer(server)
	if err != nil || got != "https://router.example/t/"+testTenantKey+"/v1" {
		t.Fatalf("codex base URL with /v1 suffix = %q", got)
	}
	got, err = serverControlBaseURL(server)
	if err != nil || got != "https://router.example/t/"+testTenantKey {
		t.Fatalf("control base URL with /v1 suffix = %q", got)
	}
	server.URL = "https://router.example/backend-api"
	got, err = serverControlBaseURL(server)
	if err != nil || got != "https://router.example/t/"+testTenantKey {
		t.Fatalf("control base URL with /backend-api suffix = %q", got)
	}
	// Without a tenant key nothing changes.
	legacy := srServerConfig{Name: "team", URL: "http://100.64.0.1:31415"}
	got, err = codexBaseURLForServer(legacy)
	if err != nil || got != "http://100.64.0.1:31415/v1" {
		t.Fatalf("legacy codex base URL = %q", got)
	}
	got, err = serverControlBaseURL(legacy)
	if err != nil || got != "http://100.64.0.1:31415" {
		t.Fatalf("legacy control base URL = %q", got)
	}
}

func TestTenantScopedLegacyURLsFailClosed(t *testing.T) {
	for _, server := range []srServerConfig{
		{URL: "http://192.168.1.10:31415", TenantKey: testTenantKey},
		{URL: "http://192.168.1.10:31415/t/" + testTenantKey},
	} {
		if got, err := codexBaseURLForServer(server); err == nil || got != "" {
			t.Fatalf("legacy Codex URL = %q, err = %v", got, err)
		}
		if got, err := serverControlBaseURL(server); err == nil || got != "" {
			t.Fatalf("legacy control URL = %q, err = %v", got, err)
		}
	}
}

func TestTenantScopedHTTPAllowsAndPinsSafeAddresses(t *testing.T) {
	for _, rawURL := range []string{
		"http://127.0.0.1:31415/t/" + testTenantKey,
		"http://[::1]:31415/t/" + testTenantKey,
	} {
		got, err := secureTenantProxyURL(t.Context(), rawURL, "")
		if err != nil || got != rawURL {
			t.Fatalf("secureTenantProxyURL(%q) = %q, %v", rawURL, got, err)
		}
	}
	loopbackLookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	got, err := secureTenantServerURLWithResolvers(
		t.Context(), "http://rebinding.example:31415/t/"+testTenantKey,
		srServerConfig{TenantKey: testTenantKey}, loopbackLookup, nil,
	)
	if err != nil || got != "http://127.0.0.1:31415/t/"+testTenantKey {
		t.Fatalf("loopback DNS URL was not pinned: %q, %v", got, err)
	}
	ipv6Lookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("::1")}}, nil
	}
	got, err = secureTenantServerURLWithResolvers(
		t.Context(), "http://localhost:31415/t/"+testTenantKey,
		srServerConfig{TenantKey: testTenantKey}, ipv6Lookup, nil,
	)
	if err != nil || got != "http://[::1]:31415/t/"+testTenantKey {
		t.Fatalf("IPv6-only localhost URL was not pinned: %q, %v", got, err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	dualLookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("::1")}, {IP: net.ParseIP("127.0.0.1")}}, nil
	}
	got, err = secureTenantServerURLWithResolvers(
		t.Context(), "http://localhost:"+port+"/t/"+testTenantKey,
		srServerConfig{TenantKey: testTenantKey}, dualLookup, nil,
	)
	if err != nil || got != "http://127.0.0.1:"+port+"/t/"+testTenantKey {
		t.Fatalf("dual-stack localhost did not select reachable IPv4: %q, %v", got, err)
	}

	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != "M3.Example.TS.Net." {
			t.Fatalf("lookup host = %q", host)
		}
		return []net.IPAddr{{IP: net.ParseIP("100.88.0.9")}}, nil
	}
	status := `{
		"Self":{"ID":"self","Online":true},
		"Peer":{"node-m3":{"ID":"node-m3","DNSName":"m3.example.ts.net.","TailscaleIPs":["100.88.0.9","fd7a:115c:a1e0::1"],"Online":true}}
	}`
	load := func(context.Context) ([]byte, error) { return []byte(status), nil }
	got, err = secureTenantServerURLWithResolvers(
		t.Context(),
		"http://M3.Example.TS.Net.:31415/t/"+testTenantKey,
		srServerConfig{TenantKey: testTenantKey},
		lookup,
		load,
	)
	if err != nil || got != "http://100.88.0.9:31415/t/"+testTenantKey {
		t.Fatalf("pinned MagicDNS URL = %q, %v", got, err)
	}
	for _, rawURL := range []string{
		"http://100.88.0.9:31415/t/" + testTenantKey,
		"http://[fd7a:115c:a1e0::1]:31415/t/" + testTenantKey,
	} {
		got, err := secureTenantServerURLWithResolvers(
			t.Context(), rawURL, srServerConfig{TenantKey: testTenantKey}, lookup, load,
		)
		if err != nil || got != rawURL {
			t.Fatalf("verified literal URL = %q, %v", got, err)
		}
	}
}

func TestTenantScopedHTTPRejectsHostileOrMixedDNS(t *testing.T) {
	tests := []struct {
		name      string
		addresses []net.IPAddr
	}{
		{name: "public Funnel address", addresses: []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}},
		{name: "mixed tailnet and public", addresses: []net.IPAddr{{IP: net.ParseIP("100.88.0.9")}, {IP: net.ParseIP("203.0.113.10")}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(context.Context, string) ([]net.IPAddr, error) { return tc.addresses, nil }
			_, err := secureTenantServerURLWithResolvers(
				t.Context(),
				"http://funnel.ts.net/t/"+testTenantKey,
				srServerConfig{TenantKey: testTenantKey},
				lookup,
				nil,
			)
			if err == nil {
				t.Fatal("hostile DNS result was accepted")
			}
		})
	}
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("100.88.0.9")}}, nil
	}
	load := func(context.Context) ([]byte, error) {
		return []byte(`{
			"Self":{"ID":"self","Online":true},
			"Peer":{"node-m3":{"ID":"node-m3","DNSName":"m3.example.ts.net.","TailscaleIPs":["100.88.0.9"],"Online":true}}
		}`), nil
	}
	if _, err := secureTenantServerURLWithResolvers(
		t.Context(), "http://evil.example/t/"+testTenantKey,
		srServerConfig{TenantKey: testTenantKey}, lookup, load,
	); err == nil {
		t.Fatal("a safe-range DNS answer without Tailscale name ownership was accepted")
	}
}

func TestTenantScopedHTTPUsesAuthenticatedTailscaleNode(t *testing.T) {
	server := srServerConfig{
		URL:             "http://M3.Example.TS.Net.:31415",
		TenantKey:       testTenantKey,
		TailscaleNodeID: "node-m3",
	}
	status := `{
		"Self":{"ID":"self","Online":true},
		"Peer":{"node-m3":{"ID":"node-m3","DNSName":"m3.example.ts.net.","TailscaleIPs":["100.88.0.9"],"Online":true}}
	}`
	load := func(context.Context) ([]byte, error) { return []byte(status), nil }
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		return nil, errors.New("DNS must not be used for a node-pinned request")
	}
	got, err := secureTenantServerURLWithResolvers(
		t.Context(),
		server.URL+"/t/"+testTenantKey,
		server,
		lookup,
		load,
	)
	if err != nil || got != "http://100.88.0.9:31415/t/"+testTenantKey {
		t.Fatalf("node-pinned URL = %q, %v", got, err)
	}
	wrongNodeURL := server
	wrongNodeURL.URL = "http://funnel.ts.net:31415"
	if _, err := secureTenantServerURLWithResolvers(
		t.Context(), wrongNodeURL.URL+"/t/"+testTenantKey, wrongNodeURL, lookup, load,
	); err == nil {
		t.Fatal("a ts.net suffix without node ownership was accepted")
	}

	offline := strings.Replace(status, `"Online":true`, `"Online":false`, 1)
	_, err = secureTenantServerURLWithResolvers(
		t.Context(), server.URL+"/t/"+testTenantKey, server, lookup,
		func(context.Context) ([]byte, error) { return []byte(offline), nil },
	)
	if err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("offline Tailscale error = %v", err)
	}
}

func TestTokenlessControlHTTPWithNodeIdentityIsPinned(t *testing.T) {
	server := srServerConfig{URL: "http://m3.example.ts.net.:31415", TailscaleNodeID: "node-m3"}
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		return nil, errors.New("node identity must avoid DNS")
	}
	load := func(context.Context) ([]byte, error) {
		return []byte(`{"Self":{"ID":"self","Online":true},"Peer":{"node-m3":{"ID":"node-m3","DNSName":"m3.example.ts.net.","TailscaleIPs":["100.88.0.9"],"Online":true}}}`), nil
	}
	got, err := serverControlBaseURLForResolvers(server, false, lookup, load)
	if err != nil || got != "http://100.88.0.9:31415" {
		t.Fatalf("tokenless node control URL = %q, %v", got, err)
	}

	legacy := srServerConfig{URL: "http://legacy.lan:31415"}
	got, err = serverControlBaseURLForResolvers(legacy, false, lookup, nil)
	if err != nil || got != legacy.URL {
		t.Fatalf("legacy tokenless LAN URL = %q, %v", got, err)
	}

	loopback := srServerConfig{URL: "http://127.0.0.1:31415", TailscaleNodeID: "stale-node"}
	got, err = serverControlBaseURLForResolvers(
		loopback, true, lookup,
		func(context.Context) ([]byte, error) {
			t.Fatal("loopback control must not consult a stale Tailscale identity")
			return nil, nil
		},
	)
	if err != nil || got != loopback.URL {
		t.Fatalf("loopback control URL = %q, %v", got, err)
	}
	got, err = secureTenantServerURLWithResolvers(
		t.Context(), loopback.URL+"/t/"+testTenantKey,
		srServerConfig{URL: loopback.URL, TenantKey: testTenantKey, TailscaleNodeID: "stale-node"},
		lookup,
		func(context.Context) ([]byte, error) {
			t.Fatal("loopback data-plane URL must not consult a stale Tailscale identity")
			return nil, nil
		},
	)
	if err != nil || got != loopback.URL+"/t/"+testTenantKey {
		t.Fatalf("loopback data-plane URL = %q, %v", got, err)
	}
}

func TestTenantTransportNilTailscaleLoaderFailsClosed(t *testing.T) {
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("100.88.0.9")}}, nil
	}
	if _, err := secureTenantServerURLWithResolvers(
		t.Context(), "http://m3.example.ts.net.:31415/t/"+testTenantKey,
		srServerConfig{TenantKey: testTenantKey}, lookup, nil,
	); err == nil || !strings.Contains(err.Error(), "verify Tailscale") {
		t.Fatalf("nil loader error = %v", err)
	}
}

func TestWriteClaudeProxyEnvTenantKeySetsAuthToken(t *testing.T) {
	dir := t.TempDir()
	if err := writeClaudeProxyEnv(dir, "https://host.example/t/"+testTenantKey, testTenantKey); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	env := settings["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "https://host.example/t/"+testTenantKey {
		t.Fatalf("base url = %v", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != testTenantKey {
		t.Fatalf("auth token = %v", env["ANTHROPIC_AUTH_TOKEN"])
	}
}

func TestSRTenantLocalCreateListRevoke(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".subrouter")
	t.Setenv("SUBROUTER_STATE_DIR", stateDir)
	store := accounts.CodexStore{Dir: filepath.Join(stateDir, "codex", "accounts")}

	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"tenant", "create", "acme"}); err != nil {
		t.Fatal(err)
	}
	created := out.String()
	if !strings.Contains(created, "Created tenant acme") || !strings.Contains(created, "srt_") {
		t.Fatalf("create output = %q", created)
	}

	registry := tenant.NewRegistry(stateDir)
	tenants, err := registry.List()
	if err != nil || len(tenants) != 1 {
		t.Fatalf("tenants = %+v, err %v", tenants, err)
	}

	out.Reset()
	if err := runner.run(context.Background(), []string{"tenant", "list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "acme") {
		t.Fatalf("list output = %q", out.String())
	}

	out.Reset()
	if err := runner.run(context.Background(), []string{"tenant", "key", "revoke", "acme", tenants[0].Keys[0].Prefix}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Revoked 1 key(s)") {
		t.Fatalf("revoke output = %q", out.String())
	}
}

func TestWriteClaudeProxyEnvClearsStaleTenantKey(t *testing.T) {
	dir := t.TempDir()
	// Profile previously pointed at a tenant-scoped server.
	if err := writeClaudeProxyEnv(dir, "https://host.example/t/"+testTenantKey, testTenantKey); err != nil {
		t.Fatal(err)
	}
	// Switching back to a non-tenant server must not keep sending the old key.
	if err := writeClaudeProxyEnv(dir, "http://host:31415", ""); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	env := settings["env"].(map[string]any)
	if env["ANTHROPIC_AUTH_TOKEN"] != "subrouter" {
		t.Fatalf("stale tenant key kept: %v", env["ANTHROPIC_AUTH_TOKEN"])
	}
}
