package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestValidateServerAccountImportURLRestrictsPlaintextToTailnetOrLoopback(t *testing.T) {
	for _, tc := range []struct {
		url     string
		wantErr bool
	}{
		{url: "http://127.0.0.1:31415/_subrouter/account-import"},
		{url: "http://100.64.0.1:31415/_subrouter/account-import"},
		{url: "http://100.127.255.254:31415/_subrouter/account-import"},
		{url: "http://[fd7a:115c:a1e0::1]:31415/_subrouter/account-import"},
		{url: "https://subrouter.example.com/_subrouter/account-import"},
		{url: "http://100.128.0.1:31415/_subrouter/account-import", wantErr: true},
		{url: "http://192.168.1.10:31415/_subrouter/account-import", wantErr: true},
		{url: "https://credential@subrouter.example.com/_subrouter/account-import", wantErr: true},
		{url: "file:///tmp/account-import", wantErr: true},
	} {
		t.Run(tc.url, func(t *testing.T) {
			err := validateServerAccountImportURL(t.Context(), tc.url)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestServerAccountImportNeverFollowsRedirects(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	runner := srRunner{client: source.Client()}
	err := runner.ensureServerAccountImportAvailable(t.Context(), srServerConfig{
		Name:       "team",
		URL:        source.URL,
		AdminToken: "secret",
	})
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("error = %v, want rejected redirect", err)
	}
	if redirected.Load() != 0 {
		t.Fatalf("credential request followed redirect %d time(s)", redirected.Load())
	}
}

func TestServerAccountImportFailureDoesNotEchoResponseOrCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		http.Error(w, "provider rejected sk-access-secret", http.StatusBadRequest)
	}))
	defer server.Close()
	runner := srRunner{client: server.Client()}
	account := accounts.StoredCodexAccount{
		Email: "apikey:test",
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-access-secret",
		},
	}

	err := runner.uploadServerAccount(t.Context(), srServerConfig{
		Name:       "team",
		URL:        server.URL,
		AdminToken: "secret",
	}, account)
	if err == nil {
		t.Fatal("expected account import failure")
	}
	if strings.Contains(err.Error(), "access-secret") || strings.Contains(err.Error(), "provider rejected") {
		t.Fatalf("account import error leaked a credential-bearing response: %v", err)
	}
}

func TestServerAccountImportTransportFailureRedactsTenantKey(t *testing.T) {
	const tenantKey = "srt_secret-tenant-key"
	runner := srRunner{client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial %s failed", req.URL.String())
	})}}

	err := runner.ensureServerAccountImportAvailable(t.Context(), srServerConfig{
		Name:      "team",
		URL:       "https://subrouter.example.com",
		TenantKey: tenantKey,
	})
	if err == nil {
		t.Fatal("expected account-import transport failure")
	}
	if strings.Contains(err.Error(), tenantKey) {
		t.Fatalf("transport error leaked tenant key: %v", err)
	}
}

func TestPlainHTTPAccountImportBypassesConfiguredProxy(t *testing.T) {
	var proxyRequests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyRequests.Add(1)
		http.Error(w, "proxy must not receive credentials", http.StatusBadGateway)
	}))
	defer proxy.Close()
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer target.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	runner := srRunner{client: &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}}
	account := accounts.StoredCodexAccount{
		Email: "apikey:test",
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-test",
		},
	}

	if err := runner.uploadServerAccount(t.Context(), srServerConfig{
		Name:       "team",
		URL:        target.URL,
		AdminToken: "secret",
	}, account); err != nil {
		t.Fatal(err)
	}
	if proxyRequests.Load() != 0 {
		t.Fatalf("plaintext credential import used a configured HTTP proxy %d time(s)", proxyRequests.Load())
	}
	if targetRequests.Load() != 2 {
		t.Fatalf("target requests = %d, want preflight and POST", targetRequests.Load())
	}
}

func TestUnsafeHTTPServerFailsBeforeCodexOAuth(t *testing.T) {
	fake := &recordingSRCommandRunner{loginAuth: testCodexAuth("founders@manaflow.ai", "fresh")}
	var output bytes.Buffer
	runner := srRunner{store: accounts.CodexStore{Dir: t.TempDir()}, out: &output, errOut: &output, cmd: fake}

	err := runner.serverLoginOne(context.Background(), srServerConfig{
		Name:       "unsafe",
		URL:        "http://192.168.1.10:31415",
		AdminToken: "secret",
	}, false, "")
	if err == nil || !strings.Contains(err.Error(), "restricted to Tailscale or loopback") {
		t.Fatalf("error = %v, want unsafe plaintext rejection", err)
	}
	if fake.hasCommandPrefix("codex", "login") {
		t.Fatalf("Codex OAuth started for unsafe upload destination: %#v", fake.commands)
	}
}
