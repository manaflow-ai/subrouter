package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

func withSRClientName(t *testing.T, name string) {
	t.Helper()
	previous := srClientName
	srClientName = func() string { return name }
	t.Cleanup(func() { srClientName = previous })
}

func withSRClientVersion(t *testing.T, version string) {
	t.Helper()
	previous := srClientVersion
	srClientVersion = func() string { return version }
	t.Cleanup(func() { srClientVersion = previous })
}

// Every path sr uses to point a tool at the subrouter server carries the sr
// build version, so the server can count which versions teammates run.
func TestCodexSubrouterHeadersIncludeClientVersion(t *testing.T) {
	withSRClientVersion(t, "v0.1.174")
	got := codexSubrouterHeaders("", "", "")
	if got != `{"X-Subrouter-Agent"="codex","X-Subrouter-Retry-Policy"="autonomous","X-Subrouter-Client-Version"="v0.1.174"}` {
		t.Fatalf("headers = %s", got)
	}
}

func TestClaudeLaunchSettingsIncludeClientVersion(t *testing.T) {
	withSRClientName(t, "leos-mbp")
	withSRClientVersion(t, "v0.1.174")
	body, err := proxyClaudeLaunchSettings("http://127.0.0.1:1/v1", "token", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `X-Subrouter-Client: leos-mbp\nX-Subrouter-Client-Version: v0.1.174`) {
		t.Fatalf("settings = %s", body)
	}
}

// The persisted settings.json outlives the sr build that wrote it, so it must
// not carry a version that would go stale after an update.
func TestPersistedClaudeProxyEnvOmitsClientVersion(t *testing.T) {
	withSRClientVersion(t, "v0.1.174")
	dir := t.TempDir()
	if err := writeClaudeProxyEnvCanonical(dir, "http://127.0.0.1:31415", ""); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), clientVersionHeader) {
		t.Fatalf("persisted settings carry a client version: %s", body)
	}
}

func TestDefaultSRClientVersionIsNormalized(t *testing.T) {
	got := defaultSRClientVersion()
	if got == "" || proxy.NormalizeClientVersion(got) != got {
		t.Fatalf("defaultSRClientVersion() = %q, want a non-empty normalized version", got)
	}
}

func TestNativeProxyRelayReportsItsOwnClientVersion(t *testing.T) {
	withSRClientVersion(t, "v0.1.174")
	seen := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		seen <- strings.Join(request.Header.Values(clientVersionHeader), ",")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	attachPrivateLocalTestListener(t, upstream)

	relay, err := startNativeProxyRelay(upstream.URL, kimiNativeProxy, "session", "router-token", "")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	for _, sent := range []string{"", "v9.9.9-spoofed"} {
		request, err := http.NewRequest(http.MethodPost, relay.URL()+"/kimi/v1/messages", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if sent != "" {
			request.Header.Set(clientVersionHeader, sent)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if got := <-seen; got != "v0.1.174" {
			t.Fatalf("sent %q: upstream client version = %q, want v0.1.174", sent, got)
		}
	}
}

func TestShortClientHostName(t *testing.T) {
	for host, want := range map[string]string{
		"Leos-MacBook-Pro.local": "Leos-MacBook-Pro",
		"build-01.example.com":   "build-01",
		"plain":                  "plain",
		"has space.local":        "",
		"":                       "",
		strings.Repeat("h", 80):  "",
		"trailing-dot.example.":  "trailing-dot",
	} {
		if got := shortClientHostName(host); got != want {
			t.Errorf("shortClientHostName(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestDefaultSRClientNameHonorsValidOverride(t *testing.T) {
	t.Setenv(clientNameEnv, "ci-runner-7")
	if got := defaultSRClientName(); got != "ci-runner-7" {
		t.Fatalf("override = %q, want ci-runner-7", got)
	}
	t.Setenv(clientNameEnv, "not valid!")
	if got := defaultSRClientName(); got == "not valid!" {
		t.Fatal("an invalid override must not be sent")
	}
}

func TestCodexSubrouterHeadersIncludeClientName(t *testing.T) {
	withSRClientName(t, "leos-mbp")
	got := codexSubrouterHeaders("", "", "")
	if got != `{"X-Subrouter-Agent"="codex","X-Subrouter-Retry-Policy"="autonomous","X-Subrouter-Client"="leos-mbp"}` {
		t.Fatalf("headers = %s", got)
	}
}

func TestClaudeLaunchSettingsIncludeClientName(t *testing.T) {
	withSRClientName(t, "leos-mbp")
	body, err := proxyClaudeLaunchSettings("http://127.0.0.1:1/v1", "token", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `X-Subrouter-Agent: claude\nX-Subrouter-Client: leos-mbp`) {
		t.Fatalf("settings = %s", body)
	}
}

func TestNativeProxyRelayForwardsClientName(t *testing.T) {
	withSRClientName(t, "relay-host")
	seen := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		seen <- strings.Join(request.Header.Values(clientNameHeader), ",")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	attachPrivateLocalTestListener(t, upstream)

	relay, err := startNativeProxyRelay(upstream.URL, kimiNativeProxy, "session", "router-token", "")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	for _, test := range []struct {
		sent string
		want string
	}{
		{sent: "tool-supplied", want: "tool-supplied"},
		{sent: "", want: "relay-host"},
		{sent: "bad value!", want: "relay-host"},
	} {
		request, err := http.NewRequest(http.MethodPost, relay.URL()+"/kimi/v1/messages", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if test.sent != "" {
			request.Header.Set(clientNameHeader, test.sent)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if got := <-seen; got != test.want {
			t.Fatalf("sent %q: upstream client = %q, want %q", test.sent, got, test.want)
		}
	}
}
