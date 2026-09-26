package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withSRClientName(t *testing.T, name string) {
	t.Helper()
	previous := srClientName
	srClientName = func() string { return name }
	t.Cleanup(func() { srClientName = previous })
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
	if got != `{"X-Subrouter-Agent"="codex","X-Subrouter-Client"="leos-mbp"}` {
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
