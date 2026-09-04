package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

func TestNativeProxyRelayComposesProviderPathAndScrubsClientCredentials(t *testing.T) {
	t.Helper()
	seen := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		seen <- request.Clone(request.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	attachPrivateLocalTestListener(t, upstream)

	relay, err := startNativeProxyRelay(upstream.URL+"/t/srt_test", kimiNativeProxy, "sr-native-test-session", "local-proxy-token", "")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	request, err := http.NewRequest(http.MethodPost, relay.URL()+"/kimi/v1/messages?stream=1", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer local-vendor-secret")
	request.Header.Set("X-Api-Key", "local-api-secret")
	request.Header.Set("Cookie", "vendor_session=local-secret")
	request.Header.Set("OpenAI-Organization", "direct-org-secret")
	request.Header.Set("OpenAI-Project", "direct-project-secret")
	request.Header.Set("X-Subrouter-Account-ID", "untrusted-pin")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}

	got := <-seen
	if got.URL.Path != "/t/srt_test/kimi/v1/messages" || got.URL.RawQuery != "stream=1" {
		t.Fatalf("upstream URL = %s, want tenant + provider path", got.URL.String())
	}
	if authorization := got.Header.Get("Authorization"); authorization != "Bearer local-proxy-token" {
		t.Fatalf("Authorization = %q, want local proxy token", authorization)
	}
	if key := got.Header.Get("X-Api-Key"); key != "" {
		t.Fatalf("X-Api-Key leaked through relay: %q", key)
	}
	if got.Header.Get("Cookie") != "" || got.Header.Get("X-Subrouter-Account-ID") != "" ||
		got.Header.Get("OpenAI-Organization") != "" || got.Header.Get("OpenAI-Project") != "" {
		t.Fatalf("client credential/routing metadata leaked: cookie=%q account=%q org=%q project=%q",
			got.Header.Get("Cookie"), got.Header.Get("X-Subrouter-Account-ID"),
			got.Header.Get("OpenAI-Organization"), got.Header.Get("OpenAI-Project"))
	}
	if got.Header.Get("X-Subrouter-Agent") != "kimi" || got.Header.Get("X-Subrouter-Session") != "sr-native-test-session" {
		t.Fatalf("routing headers = agent %q session %q", got.Header.Get("X-Subrouter-Agent"), got.Header.Get("X-Subrouter-Session"))
	}
	if got.Host != strings.TrimPrefix(upstream.URL, "http://") {
		t.Fatalf("upstream Host = %q, want target host", got.Host)
	}

	guard, err := nativeProxyLoopbackGuardURL(relay.URL())
	if err != nil {
		t.Fatal(err)
	}
	guardURL, err := url.Parse(guard)
	if err != nil {
		t.Fatal(err)
	}
	proxyClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(guardURL)}}
	proxiedRequest, err := http.NewRequest(http.MethodPost, relay.URL()+"/kimi/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	proxiedResponse, err := proxyClient.Do(proxiedRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = proxiedResponse.Body.Close()
	if proxiedResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("same-origin absolute-form relay status = %d, want 404", proxiedResponse.StatusCode)
	}
	select {
	case leaked := <-seen:
		t.Fatalf("same-origin absolute-form relay reached upstream: %s", leaked.URL)
	default:
	}

	relayURL, err := url.Parse(relay.URL())
	if err != nil {
		t.Fatal(err)
	}
	externalTarget := "http://outside.example.test" + relayURL.Path + "/kimi/v1/messages"
	externalResponse, err := proxyClient.Get(externalTarget)
	if err != nil {
		t.Fatal(err)
	}
	_ = externalResponse.Body.Close()
	if externalResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-origin absolute-form relay status = %d, want 404", externalResponse.StatusCode)
	}
	select {
	case leaked := <-seen:
		t.Fatalf("cross-origin absolute-form relay reached upstream: %s", leaked.URL)
	default:
	}
	connectRequest, err := http.NewRequest(http.MethodConnect, relay.URL()+"/kimi/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	connectResponse, err := http.DefaultClient.Do(connectRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = connectResponse.Body.Close()
	if connectResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("CONNECT relay status = %d, want 404", connectResponse.StatusCode)
	}
	select {
	case leaked := <-seen:
		t.Fatalf("CONNECT relay reached upstream: %s", leaked.URL)
	default:
	}

	for _, forbidden := range []string{
		"http://" + relay.listener.Addr().String() + "/kimi/v1/messages",
		relay.URL() + "/_subrouter/accounts",
		relay.URL() + "/qwen-token/v1/chat/completions",
	} {
		response, err := http.Get(forbidden)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("forbidden relay path %q returned %d", forbidden, response.StatusCode)
		}
		select {
		case leaked := <-seen:
			t.Fatalf("forbidden relay path reached upstream: %s", leaked.URL)
		default:
		}
	}
}

func TestAntigravityNativeProxyRelayComposesCloudCodePath(t *testing.T) {
	seen := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		seen <- request.Clone(request.Context())
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	attachPrivateLocalTestListener(t, upstream)
	relay, err := startNativeProxyRelay(upstream.URL, antigravityNativeProxy, "agy-session", "router-token", "")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	request, err := http.NewRequest(http.MethodPost, relay.URL()+"/antigravity/v1internal:generateContent", strings.NewReader(`{"model":"gemini-3.1-pro"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer local-agy-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	got := <-seen
	if got.URL.Path != "/antigravity/v1internal:generateContent" {
		t.Fatalf("upstream path = %q", got.URL.Path)
	}
	if got.Header.Get("Authorization") != "Bearer router-token" || got.Header.Get("X-Subrouter-Agent") != "antigravity" || got.Header.Get("X-Subrouter-Session") != "agy-session" {
		t.Fatalf("routing headers = auth %q agent %q session %q", got.Header.Get("Authorization"), got.Header.Get("X-Subrouter-Agent"), got.Header.Get("X-Subrouter-Session"))
	}
}

func TestNativeProxyRelayInjectsOnlyValidatedPinnedAccount(t *testing.T) {
	seen := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		seen <- request.Clone(request.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	relay, err := startNativeProxyRelay(upstream.URL, qwenNativeProxy, "pinned-session", "router-token", "qwen-token:work")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	request, err := http.NewRequest(http.MethodPost, relay.URL()+"/qwen-token/v1/chat/completions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Subrouter-Account-ID", "untrusted-child-account")
	request.Header.Set("Connection", "Authorization, X-Subrouter-Agent, X-Subrouter-Session, X-Subrouter-Account-ID")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	got := <-seen
	if accountID := got.Header.Get("X-Subrouter-Account-ID"); accountID != "qwen-token:work" {
		t.Fatalf("pinned account header = %q", accountID)
	}
	if authorization := got.Header.Get("Authorization"); authorization != "Bearer router-token" {
		t.Fatalf("router authorization = %q", authorization)
	}
	if got.Header.Get("X-Subrouter-Agent") != "qwen-token" || got.Header.Get("X-Subrouter-Session") != "pinned-session" {
		t.Fatalf("routing headers were removed by client hop metadata: agent=%q session=%q", got.Header.Get("X-Subrouter-Agent"), got.Header.Get("X-Subrouter-Session"))
	}
	if got.Header.Get("Connection") != "" {
		t.Fatalf("client hop metadata leaked upstream: %q", got.Header.Get("Connection"))
	}
	if got.Header.Get("X-Subrouter-Account") != "" {
		t.Fatalf("untrusted account alias leaked: %q", got.Header.Get("X-Subrouter-Account"))
	}
	if _, err := startNativeProxyRelay(upstream.URL, qwenNativeProxy, "pinned-session", "router-token", "bad\r\nX-Injected: yes"); err == nil {
		t.Fatal("header-injecting pinned account was accepted")
	}
}

func TestLocalStoreAttestedProxyRelayKeepsDurableTokenOutOfChildAndReattests(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	initializeLocalDataTestStore(t, store)
	var attestations atomic.Int32
	var routed atomic.Int32
	const durableToken = "durable-local-proxy-token-must-not-reach-child"
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == proxy.StoreHandshakePath {
			writeLocalStoreAuthorityHealth(t, response, request, store, "enabled")
			return
		}
		if request.URL.Path == "/_subrouter/health" {
			challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader)
			proof, err := accounts.StoreAuthorityProof(store.Dir, challenge)
			if err != nil {
				t.Errorf("store proof: %v", err)
				http.Error(response, "proof", http.StatusInternalServerError)
				return
			}
			id, err := accounts.StoreAuthorityID(store.Dir)
			if err != nil {
				t.Errorf("store id: %v", err)
				http.Error(response, "id", http.StatusInternalServerError)
				return
			}
			attestations.Add(1)
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(map[string]string{
				"account_store_id": id, "account_store_proof": proof,
			})
			return
		}
		if request.URL.Path != "/v1/messages" {
			http.NotFound(response, request)
			return
		}
		if got := request.Header.Get("Authorization"); got != "Bearer "+durableToken {
			t.Errorf("upstream authorization = %q", got)
		}
		if got := request.Header.Get("X-Subrouter-Preferred-Account-ID"); got != "claude-preferred" {
			t.Errorf("preferred account = %q", got)
		}
		if got := request.Header.Get("X-Subrouter-Session"); got != "" {
			t.Errorf("synthetic session = %q, want vendor session identity to remain authoritative", got)
		}
		if got := request.Header.Get("X-Claude-Session-ID"); got != "existing-claude-session" {
			t.Errorf("vendor session = %q, want existing-claude-session", got)
		}
		routed.Add(1)
		response.Header().Set("Connection", "close")
		response.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	attachPrivateLocalTestListener(t, upstream)

	relay, err := startLocalStoreAttestedProxyRelay(
		upstream.URL, "v1", "claude", "", durableToken, "", "claude-preferred", store,
	)
	if err != nil {
		t.Fatal(err)
	}
	childURL := relay.URL() + "/v1/messages"
	childCredential := relay.Credential()
	if childCredential == "" || childCredential == durableToken || strings.Contains(childURL, durableToken) {
		t.Fatalf("child capability exposed durable token: url=%q credential=%q", childURL, childCredential)
	}
	for range 2 {
		request, err := http.NewRequest(http.MethodPost, childURL, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+childCredential)
		request.Header.Set("X-Claude-Session-ID", "existing-claude-session")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("relay status = %d", response.StatusCode)
		}
	}
	if got := routed.Load(); got != 2 {
		t.Fatalf("routed requests = %d, want 2", got)
	}
	if got := attestations.Load(); got != 0 {
		t.Fatalf("private data channel unexpectedly requested %d public proof(s)", got)
	}
	relay.Close()
	if _, err := http.Post(childURL, "application/json", strings.NewReader(`{}`)); err == nil {
		t.Fatal("closed relay still accepted a child request")
	}
}

func TestParseNativeProxyLaunchArgsOwnsOnlyLeadingAccountOption(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		selector   string
		picker     bool
		vendorArgs []string
		wantErr    string
	}{
		{name: "pooled", args: []string{"--model", "x"}, vendorArgs: []string{"--model", "x"}},
		{name: "pin separate", args: []string{"--account", "work", "--model", "x"}, selector: "work", vendorArgs: []string{"--model", "x"}},
		{name: "pin equals with delimiter", args: []string{"--account=work", "--", "--account", "vendor"}, selector: "work", vendorArgs: []string{"--account", "vendor"}},
		{name: "picker", args: []string{"--account"}, picker: true},
		{name: "picker with delimiter", args: []string{"--account", "--", "prompt"}, picker: true, vendorArgs: []string{"prompt"}},
		{name: "delimiter makes account vendor owned", args: []string{"--", "--account", "vendor"}, vendorArgs: []string{"--account", "vendor"}},
		{name: "first vendor arg ends parsing", args: []string{"--model", "x", "--account", "vendor"}, vendorArgs: []string{"--model", "x", "--account", "vendor"}},
		{name: "empty equals", args: []string{"--account="}, wantErr: "non-empty"},
		{name: "missing selector", args: []string{"--account", "--model"}, wantErr: "requires an account selector"},
		{name: "duplicate", args: []string{"--account", "work", "--account=other"}, wantErr: "only once"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, vendorArgs, err := parseNativeProxyLaunchArgs(test.args)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if options.accountSelector != test.selector || options.pickPinnedAccount != test.picker || !reflect.DeepEqual(vendorArgs, test.vendorArgs) {
				t.Fatalf("parsed = %+v, %q; want selector=%q picker=%t args=%q", options, vendorArgs, test.selector, test.picker, test.vendorArgs)
			}
		})
	}
}

func TestNativeProxyAccountSelectionIsProviderScopedAndFailsClosed(t *testing.T) {
	inventory := []remoteServerAccount{
		{ID: "kimi-subscription:work", Label: "Work", Email: "member@example.test", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth},
		{ID: "kimi:metered", Label: "Metered", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeAPIKey},
		{ID: "kimi-subscription:collision", Label: "kimi:metered", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth},
		{ID: "kimi-code", Label: "Direct CLI", Source: "kimi-code credentials file", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth},
		{ID: "qwen-token:large", Label: "Large", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey},
		{ID: "qwen-token:larger", Label: "Larger", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey},
		{ID: "antigravity-subscription:work", Label: "AGY Work", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth},
	}
	for selector, want := range map[string]string{
		"WORK":                "kimi-subscription:work",
		"member@example.test": "kimi-subscription:work",
		"metered":             "kimi:metered",
	} {
		got, err := resolveNativeProxyAccountSelector(kimiNativeProxy, inventory, selector)
		if err != nil || got != want {
			t.Fatalf("resolve Kimi %q = %q, %v; want %q", selector, got, err, want)
		}
	}
	if got, err := resolveNativeProxyAccountSelector(qwenNativeProxy, inventory, "large"); err != nil || got != "qwen-token:large" {
		t.Fatalf("resolve Qwen prefix-stripped ID = %q, %v", got, err)
	}
	if got, err := resolveNativeProxyAccountSelector(kimiNativeProxy, inventory, "kimi:metered"); err != nil || got != "kimi:metered" {
		t.Fatalf("canonical ID did not outrank colliding label: %q, %v", got, err)
	}
	if got, err := resolveNativeProxyAccountSelector(antigravityNativeProxy, inventory, "AGY Work"); err != nil || got != "antigravity-subscription:work" {
		t.Fatalf("resolve Antigravity label = %q, %v", got, err)
	}
	for _, test := range []struct {
		spec     nativeProxySpec
		selector string
		wantErr  string
	}{
		{spec: kimiNativeProxy, selector: "Direct CLI", wantErr: "not an eligible routed Kimi account"},
		{spec: kimiNativeProxy, selector: "Large", wantErr: "not an eligible routed Kimi account"},
		{spec: qwenNativeProxy, selector: "larg", wantErr: "ambiguous"},
		{spec: qwenNativeProxy, selector: "missing", wantErr: "was not found"},
		{spec: qwenNativeProxy, selector: "bad\rselector", wantErr: "control character"},
	} {
		if _, err := resolveNativeProxyAccountSelector(test.spec, inventory, test.selector); err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Fatalf("resolve %s %q error = %v, want %q", test.spec.display, test.selector, err, test.wantErr)
		}
	}
	injected := append([]remoteServerAccount(nil), inventory...)
	injected[4].ID = "qwen-token:good\r\nX-Injected: yes"
	if _, err := resolveNativeProxyAccountSelector(qwenNativeProxy, injected, "Large"); err == nil || !strings.Contains(err.Error(), "invalid server routing ID") {
		t.Fatalf("header-injecting account ID error = %v", err)
	}
}

func TestTeamNativeProxyLaunchUsesBrokerForPooledAndPinnedQwen(t *testing.T) {
	localStore := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "local-state", "codex", "accounts")}
	initializeLocalDataTestStore(t, localStore)
	var accountRequests atomic.Int32
	var pinnedRequests atomic.Int32
	var credentialSinkRequests atomic.Int32
	credentialSink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		credentialSinkRequests.Add(1)
		http.Error(w, "unexpected proxy use", http.StatusBadGateway)
	}))
	defer credentialSink.Close()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, credentialSink.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case proxy.StoreHandshakePath:
			writeLocalStoreAuthorityHealth(t, w, request, localStore, "enabled")
		case "/_subrouter/health":
			authorityID, err := accounts.StoreAuthorityID(localStore.Dir)
			if err != nil {
				t.Fatal(err)
			}
			proof := ""
			if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
				proof, err = accounts.StoreAuthorityProof(localStore.Dir, challenge)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, _ = fmt.Fprintf(w, `{"ok":true,"account_store_id":%q,"account_store_proof":%q}`, authorityID, proof)
		case "/":
			if request.Method != http.MethodHead {
				t.Errorf("data-plane preflight method = %s", request.Method)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/_subrouter/accounts":
			accountRequests.Add(1)
			_, _ = io.WriteString(w, `[]`)
		case "/qwen-token/v1/chat/completions":
			if got := request.Header.Get("X-Subrouter-Account-ID"); got != "qwen-token:work" {
				t.Errorf("pinned request account = %q", got)
			}
			if got := request.Header.Get("Authorization"); got != "Bearer test-local-proxy-token" {
				t.Errorf("pinned request authorization = %q", got)
			}
			pinnedRequests.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server)
	var toolRequests atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/tool-check" {
			http.NotFound(w, request)
			return
		}
		toolRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer toolServer.Close()
	var teamAccountRequests atomic.Int32
	brokerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/subrouter/accounts" {
			http.NotFound(w, request)
			return
		}
		teamAccountRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"teamId":"test-team","accounts":[{"id":"qwen-token:work","kind":"qwen-token","label":"Work"}]}`)
	}))
	defer brokerServer.Close()

	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	if err := broker.SaveConfig(cloudPath, broker.Config{
		CredentialSource: broker.CredentialSourceTeam,
		BaseURL:          brokerServer.URL,
		AccessToken:      "test-access",
		RefreshToken:     "test-refresh",
		TeamID:           "test-team",
		LocalProxyToken:  "test-local-proxy-token",
		HostedURL:        brokerServer.URL,
		TenantKey:        testTenantKey,
	}); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	// Qwen 0.22.3's production cli-entry replaces its real argv whenever this
	// inherited variable is non-empty. Model that boundary in an actual child:
	// the escape marker runs only if Subrouter failed to scrub the variable.
	if err := os.WriteFile(filepath.Join(binDir, "qwen"), []byte("#!/bin/sh\nif [ \"${SRTEST_QWEN_HELPER:-}\" = 1 ]; then\n  if [ -n \"${QWEN_CODE_RELAUNCH_ARGS:-}\" ]; then\n    exec \"$SRTEST_BINARY\" -test.run=^TestTeamNativeProxyQwenChild$ -- relaunch-escape\n  fi\n  exec \"$SRTEST_BINARY\" -test.run=^TestTeamNativeProxyQwenChild$ -- \"$@\"\nfi\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SRTEST_BINARY", os.Args[0])
	t.Setenv("SRTEST_TOOL_URL", toolServer.URL+"/tool-check")
	t.Setenv("QWEN_CODE_RELAUNCH_ARGS", `["--model","direct-relaunch-model","--openai-api-key","direct-relaunch-secret"]`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner := srRunner{store: localStore, client: server.Client(), in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.launchNativeProxy(t.Context(), qwenNativeProxy, nil, nativeProxyLaunchOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := accountRequests.Load(); got != 0 {
		t.Fatalf("pooled team launch made %d local account inventory request(s), want broker selection at request time", got)
	}
	for _, spec := range []nativeProxySpec{kimiNativeProxy, antigravityNativeProxy} {
		err := runner.launchNativeProxy(t.Context(), spec, nil, nativeProxyLaunchOptions{})
		if err == nil || !strings.Contains(err.Error(), "team credential storage cannot lease") {
			t.Fatalf("pooled team %s launch error = %v, want unsupported broker provider", spec.display, err)
		}
	}
	if got := accountRequests.Load(); got != 0 {
		t.Fatalf("unsupported team launch made %d account inventory request(s), want pre-exec rejection", got)
	}

	t.Setenv("SRTEST_QWEN_HELPER", "1")
	if err := runner.launchNativeProxy(t.Context(), qwenNativeProxy, nil, nativeProxyLaunchOptions{accountSelector: "work"}); err != nil {
		t.Fatalf("named pinned team launch: %v", err)
	}
	runner.in = strings.NewReader("1\n")
	if err := runner.launchNativeProxy(t.Context(), qwenNativeProxy, nil, nativeProxyLaunchOptions{pickPinnedAccount: true}); err != nil {
		t.Fatalf("picker pinned team launch: %v", err)
	}
	if got := teamAccountRequests.Load(); got != 2 {
		t.Fatalf("pinned team launches made %d broker inventory request(s), want 2", got)
	}
	if got := accountRequests.Load(); got != 0 {
		t.Fatalf("team launches made %d local account inventory request(s), want broker authority only", got)
	}
	if got := pinnedRequests.Load(); got != 2 {
		t.Fatalf("pinned team launches routed %d forced request(s), want 2", got)
	}
	if got := toolRequests.Load(); got != 2 {
		t.Fatalf("pinned team launches made %d direct tool request(s), want 2", got)
	}
	if got := credentialSinkRequests.Load(); got != 0 {
		t.Fatalf("Qwen child sent %d request(s) to the inherited credential sink", got)
	}
}

func TestTeamNativeProxyQwenChild(t *testing.T) {
	if os.Getenv("SRTEST_QWEN_HELPER") != "1" {
		return
	}
	if relaunch := os.Getenv("QWEN_CODE_RELAUNCH_ARGS"); relaunch != "" {
		t.Fatalf("QWEN_CODE_RELAUNCH_ARGS reached the Qwen child: %q", relaunch)
	}
	childArgs := "\x00" + strings.Join(os.Args, "\x00") + "\x00"
	for _, want := range []string{"\x00--bare\x00", "\x00--auth-type\x00openai\x00", "\x00--model\x00" + defaultQwenProxyModel + "\x00", "\x00--openai-api-key\x00subrouter\x00"} {
		if !strings.Contains(childArgs, want) {
			t.Fatalf("Qwen child argv %q does not contain forced routing sequence %q", os.Args, want)
		}
	}
	for _, escaped := range []string{"relaunch-escape", "direct-relaunch-model", "direct-relaunch-secret"} {
		if strings.Contains(childArgs, escaped) {
			t.Fatalf("Qwen child argv was replaced by inherited relaunch data: %q", os.Args)
		}
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, strings.TrimRight(os.Getenv("OPENAI_BASE_URL"), "/")+"/chat/completions", strings.NewReader(`{"model":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("routed Qwen request status = %d", response.StatusCode)
	}
	toolResponse, err := http.Get(os.Getenv("SRTEST_TOOL_URL"))
	if err != nil {
		t.Fatalf("direct Qwen tool request: %v", err)
	}
	defer toolResponse.Body.Close()
	if toolResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("direct Qwen tool request status = %d", toolResponse.StatusCode)
	}
}

func TestRemoteTenantNativeProxyAuthenticatesPreflightAndRelayWithoutExposingKeyToChild(t *testing.T) {
	var preflights atomic.Int32
	var routedRequests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+testTenantKey {
			http.Error(response, "tenant authorization required", http.StatusUnauthorized)
			return
		}
		switch {
		case request.Method == http.MethodHead && request.URL.Path == "/t/"+testTenantKey+"/":
			preflights.Add(1)
			response.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodPost && request.URL.Path == "/t/"+testTenantKey+"/qwen-token/v1/chat/completions":
			if preflights.Load() != 1 {
				http.Error(response, "routed request arrived before authenticated preflight", http.StatusPreconditionFailed)
				return
			}
			if request.Header.Get("X-Subrouter-Agent") != "qwen-token" || request.Header.Get("X-Subrouter-Session") == "" {
				http.Error(response, "missing routed session identity", http.StatusBadRequest)
				return
			}
			routedRequests.Add(1)
			response.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(response, request)
		}
	}))
	defer remote.Close()

	var credentialSinkRequests atomic.Int32
	credentialSink := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		credentialSinkRequests.Add(1)
		http.Error(response, "unexpected proxy use", http.StatusBadGateway)
	}))
	defer credentialSink.Close()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, credentialSink.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "tenant",
		Servers: []srServerConfig{{Name: "tenant", URL: remote.URL, TenantKey: testTenantKey}},
	}); err != nil {
		t.Fatal(err)
	}
	// A named remote is authoritative before any loopback serving-store
	// metadata is inspected. This intentionally malformed, public pointer would
	// fail closed if the remote launch accidentally consulted local authority.
	if err := os.WriteFile(localServingStoreBindingPath(store), []byte(`{"schema":`), 0o644); err != nil {
		t.Fatal(err)
	}
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := broker.SaveConfig(cloudPath, broker.Config{CredentialSource: broker.CredentialSourceLegacy}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("QWEN_HOME", filepath.Join(t.TempDir(), "qwen-home"))

	binDir := t.TempDir()
	qwenPath := filepath.Join(binDir, "qwen")
	qwenHelper := "#!/bin/sh\nexec \"$SRTEST_BINARY\" -test.run=^TestRemoteTenantNativeProxyQwenChild$ -- \"$@\"\n"
	if err := os.WriteFile(qwenPath, []byte(qwenHelper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SRTEST_BINARY", os.Args[0])
	t.Setenv("SRTEST_REMOTE_TENANT_QWEN_HELPER", "1")

	runner := srRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.launchNativeProxy(t.Context(), qwenNativeProxy, nil, nativeProxyLaunchOptions{}); err != nil {
		t.Fatal(err)
	}
	if preflights.Load() != 1 || routedRequests.Load() != 1 {
		t.Fatalf("remote tenant requests: preflight=%d routed=%d, want one each", preflights.Load(), routedRequests.Load())
	}
	if credentialSinkRequests.Load() != 0 {
		t.Fatalf("ambient proxy received %d request(s)", credentialSinkRequests.Load())
	}
}

func TestRemoteTenantNativeProxyQwenChild(t *testing.T) {
	if os.Getenv("SRTEST_REMOTE_TENANT_QWEN_HELPER") != "1" {
		return
	}
	for _, arg := range os.Args {
		if strings.Contains(arg, testTenantKey) {
			t.Fatal("remote tenant key reached child argv")
		}
	}
	for _, entry := range os.Environ() {
		if strings.Contains(entry, testTenantKey) {
			t.Fatal("remote tenant key reached child environment")
		}
	}
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if !strings.HasPrefix(baseURL, "http://127.0.0.1:") || strings.Contains(baseURL, testTenantKey) {
		t.Fatal("Qwen child did not receive an opaque loopback relay URL")
	}
	if os.Getenv("OPENAI_API_KEY") != "subrouter" {
		t.Fatal("Qwen child did not receive the non-secret routing sentinel")
	}
	request, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/chat/completions",
		strings.NewReader(`{"model":"test","messages":[]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("routed Qwen request status = %d", response.StatusCode)
	}
}

func TestNativeProxyPinnedPickerIsSortedAndBlankCancels(t *testing.T) {
	inventory := []remoteServerAccount{
		{ID: "qwen-token:z", Label: "Shared", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey},
		{ID: "qwen-token:a", Label: "Shared", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey},
	}
	var out bytes.Buffer
	runner := srRunner{in: strings.NewReader("2\n"), out: &out}
	got, chosen, err := runner.pickNativeProxyAccount(qwenNativeProxy, inventory)
	if err != nil || !chosen || got != "qwen-token:z" {
		t.Fatalf("picked = %q chosen=%t err=%v", got, chosen, err)
	}
	if text := out.String(); !strings.Contains(text, "PINNED process") || !strings.Contains(text, "No account failover") ||
		!strings.Contains(text, "Shared (qwen-token:a; apikey)") || !strings.Contains(text, "Shared (qwen-token:z; apikey)") ||
		strings.Index(text, "qwen-token:a") > strings.Index(text, "qwen-token:z") {
		t.Fatalf("picker output = %q", text)
	}
	runner.in = strings.NewReader("\n")
	if got, chosen, err := runner.pickNativeProxyAccount(qwenNativeProxy, inventory); err != nil || chosen || got != "" {
		t.Fatalf("blank picker = %q chosen=%t err=%v", got, chosen, err)
	}
}

func TestNativeProxyDispatchSeparatesManagementFromDefaultLaunch(t *testing.T) {
	for _, args := range [][]string{nil, {"--model", "x"}, {"proxy"}, {"--account", "work"}} {
		if isKimiManagementCommand(args) || isQwenManagementCommand(args) || isAntigravityManagementCommand(args) {
			t.Fatalf("launch args %q classified as management", args)
		}
	}
	for _, args := range [][]string{{"login"}, {"help"}, {"--help"}} {
		if !isKimiManagementCommand(args) || !isQwenManagementCommand(args) {
			t.Fatalf("management args %q classified as launch", args)
		}
	}
	if !isKimiManagementCommand([]string{"list"}) || isQwenManagementCommand([]string{"list"}) {
		t.Fatal("provider-specific management verbs were not preserved")
	}
	for _, verb := range []string{"add", "list", "remove"} {
		if !isAntigravityManagementCommand([]string{verb}) {
			t.Fatalf("Antigravity management verb %q classified as launch", verb)
		}
	}
	for _, provider := range []string{"kimi", "qwen"} {
		var out bytes.Buffer
		runner := srRunner{out: &out}
		if err := runner.run(t.Context(), []string{provider, "--help"}); err != nil {
			t.Fatalf("sr %s --help: %v", provider, err)
		}
		if !strings.Contains(out.String(), "Plain '") && !strings.Contains(out.String(), "plain ") {
			t.Fatalf("sr %s --help did not describe the direct bypass: %q", provider, out.String())
		}
	}
}

func TestNativeProxyRelayTransportNeverUsesAmbientProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://credential-sink.invalid")
	t.Setenv("HTTPS_PROXY", "http://credential-sink.invalid")
	transport, err := nativeProxyRelayTransport("https://router.example.test/t/opaque")
	if err != nil {
		t.Fatal(err)
	}
	if transport.Proxy != nil {
		t.Fatal("native relay transport retained an ambient proxy function")
	}
}

func TestNativeProxyUsesConfiguredLocalDaemonTokenWithoutExposingItToChild(t *testing.T) {
	root := "http://127.0.0.1:43213"
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", root)
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"local","localProxyToken":"local-daemon-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := nativeProxyServerToken(srServerConfig{URL: root + "/tenantless"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if token != "local-daemon-secret" {
		t.Fatalf("local daemon token selected = %q", token)
	}
	if remoteToken, err := nativeProxyServerToken(srServerConfig{URL: "https://router.example.test"}, true); err != nil || remoteToken != "subrouter" {
		t.Fatalf("remote placeholder = %q err=%v", remoteToken, err)
	}
	if tenantToken, err := nativeProxyServerToken(srServerConfig{URL: "https://router.example.test", TenantKey: "srt_selected_tenant"}, true); err != nil || tenantToken != "srt_selected_tenant" {
		t.Fatalf("remote tenant token = %q err=%v", tenantToken, err)
	}
	if selectedLoopbackToken, err := nativeProxyServerToken(srServerConfig{URL: root + "/selected-server"}, false); err != nil || selectedLoopbackToken != "local-daemon-secret" {
		t.Fatalf("selected loopback token = %q err=%v", selectedLoopbackToken, err)
	}
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", "http://localhost:43213")
	if pinnedLoopbackToken, err := nativeProxyServerToken(srServerConfig{URL: "http://127.0.0.1:43213"}, false); err != nil || pinnedLoopbackToken != "local-daemon-secret" {
		t.Fatalf("pinned loopback token = %q err=%v", pinnedLoopbackToken, err)
	}
	if otherLoopbackToken, err := nativeProxyServerToken(srServerConfig{URL: "http://127.0.0.2:43213"}, true); err != nil || otherLoopbackToken != "subrouter" {
		t.Fatalf("other loopback token = %q err=%v, want remote placeholder", otherLoopbackToken, err)
	}
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", "https://router.example.test")
	if sameLocalProxyEndpoint("https://router.example.test/t/opaque", "https://router.example.test") {
		t.Fatal("matching non-loopback endpoints were treated as the local daemon")
	}
	if remoteOverrideToken, err := nativeProxyServerToken(srServerConfig{URL: "https://router.example.test/t/opaque"}, true); err != nil || remoteOverrideToken != "subrouter" {
		t.Fatalf("non-loopback local override token = %q err=%v", remoteOverrideToken, err)
	}
	env, cleanup, err := nativeProxyEnvironment(kimiNativeProxy, "http://127.0.0.1:43214/capability", os.Environ(), nil)
	cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(env, "\n"), "local-daemon-secret") {
		t.Fatal("local daemon token leaked into the vendor child environment")
	}
}

func TestNativeProxyServerIgnoresStaleRemoteDefaultForLocalStorage(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			http.NotFound(w, request)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer local.Close()
	attachPrivateLocalTestListener(t, local)
	var staleRequests atomic.Int32
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		staleRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer stale.Close()

	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL)
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"local"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	serverStore := defaultSRServerStore(store)
	if err := serverStore.update(func(file *srServerFile) error {
		file.Default = "stale"
		file.Servers = []srServerConfig{{Name: "stale", URL: stale.URL}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	server, remote, err := (srRunner{store: store, errOut: io.Discard}).nativeProxyServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if remote || !sameEndpoint(server.URL, local.URL) {
		t.Fatalf("native proxy server = %+v remote=%t, want active local storage", server, remote)
	}
	if staleRequests.Load() != 0 {
		t.Fatalf("stale remote received %d request(s)", staleRequests.Load())
	}
}

func TestNativeProxyServerHonorsExplicitLocalOverLegacyStorage(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			http.NotFound(w, request)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer local.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL)
	t.Setenv("SUBROUTER_SERVER", "local")
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	server, remote, err := (srRunner{store: accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}, errOut: io.Discard}).nativeProxyServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if remote || !sameEndpoint(server.URL, local.URL) {
		t.Fatalf("native proxy server = %+v remote=%t, want explicit local", server, remote)
	}
}

func TestNativeProxyServerUsesReadyLocalServingAuthorityForUnselectedLegacy(t *testing.T) {
	var localRequests atomic.Int32
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		localRequests.Add(1)
		if request.URL.Path != "/_subrouter/health" {
			http.NotFound(w, request)
			return
		}
		authorityID, err := accounts.StoreAuthorityID(store.Dir)
		if err != nil {
			t.Fatal(err)
		}
		proof := ""
		if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
			proof, err = accounts.StoreAuthorityProof(store.Dir, challenge)
			if err != nil {
				t.Fatal(err)
			}
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"account_store_id":%q,"account_store_proof":%q}`, authorityID, proof)
	}))
	defer local.Close()
	attachPrivateLocalTestListener(t, local, store)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL)
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	server, remote, err := (srRunner{store: store, errOut: io.Discard, client: local.Client()}).nativeProxyServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if remote || !sameEndpoint(server.URL, local.URL) {
		t.Fatalf("native proxy server = %+v remote=%t, want ready local serving authority", server, remote)
	}
	if localRequests.Load() == 0 {
		t.Fatal("unselected legacy authority did not health-check the local daemon")
	}
}

func TestNativeProxyRejectsUnattestedLocalAuthorityBeforeInventoryOrToken(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	otherStore := accounts.CodexStore{Dir: filepath.Join(home, "rogue-state", "codex", "accounts")}
	otherAuthority, err := accounts.StoreAuthorityID(otherStore.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var nonHealthRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			nonHealthRequests.Add(1)
			http.Error(w, "credential sink", http.StatusUnauthorized)
			return
		}
		proof := ""
		if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
			var proofErr error
			proof, proofErr = accounts.StoreAuthorityProof(otherStore.Dir, challenge)
			if proofErr != nil {
				t.Fatal(proofErr)
			}
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"account_import":"enabled","account_store_id":%q,"account_store_proof":%q}`, otherAuthority, proof)
	}))
	defer server.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	cloudPath := filepath.Join(home, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := broker.SaveConfig(cloudPath, broker.Config{
		CredentialSource: broker.CredentialSourceLocal,
		LocalProxyToken:  "must-not-be-sent",
	}); err != nil {
		t.Fatal(err)
	}

	runner := srRunner{store: store, client: server.Client(), in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	err = runner.launchNativeProxy(t.Context(), qwenNativeProxy, nil, nativeProxyLaunchOptions{})
	if err == nil || (!strings.Contains(err.Error(), "published private local data socket") && !strings.Contains(err.Error(), "data-plane preflight failed")) {
		t.Fatalf("unattested local launcher error = %v", err)
	}
	if nonHealthRequests.Load() != 0 {
		t.Fatalf("unattested local listener received %d inventory or credential-bearing request(s)", nonHealthRequests.Load())
	}
}

func TestNativeProxyServerUsesHostedAuthorityWithoutLegacyRegistry(t *testing.T) {
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := broker.SaveConfig(cloudPath, broker.Config{
		CredentialSource: broker.CredentialSourceHosted,
		AccessToken:      "hosted-access",
		RefreshToken:     "hosted-refresh",
		TeamID:           "hosted-team",
		HostedURL:        "https://hosted.example.test",
		TenantKey:        testTenantKey,
	}); err != nil {
		t.Fatal(err)
	}
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	if err := os.MkdirAll(store.StoreDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultSRServerStore(store).Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", "http://127.0.0.1:1")

	server, remote, err := (srRunner{store: store, errOut: io.Discard}).nativeProxyServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !remote || server.Name != "cmux" || server.URL != "https://hosted.example.test" || server.TenantKey != testTenantKey {
		t.Fatalf("native proxy server = %+v remote=%t, want hosted cloud authority", server, remote)
	}
}

func TestNativeProxyEnvironmentsReplaceRoutingCredentialsWithoutExposingScope(t *testing.T) {
	kimiSourceHome := filepath.Join(t.TempDir(), "kimi-home")
	kimiSourceHome = prepareKimiTestSessionHome(t, kimiSourceHome)
	original := []string{
		"PATH=/usr/bin", "KEEP_ME=yes", "KIMI_CODE_HOME=" + kimiSourceHome, "QWEN_HOME=/custom/qwen-home",
		`QWEN_CODE_RELAUNCH_ARGS=["--model","direct-relaunch-model","--openai-api-key","direct-relaunch-secret"]`,
		"QWEN_SANDBOX=1",
		"OPENAI_API_KEY=real-openai-secret", "OPENAI_BASE_URL=https://vendor.invalid/v1",
		"OPENAI_ORG_ID=direct-org-secret", "OPENAI_PROJECT_ID=direct-project-secret",
		"BAILIAN_CODING_PLAN_API_KEY=real-coding-plan-secret",
		"BAILIAN_TOKEN_PLAN_API_KEY=real-bailian-secret", "KIMI_MODEL_API_KEY=real-kimi-secret",
		"KIMI_MODEL_MAX_CONTEXT_SIZE=999999", "KIMI_MODEL_CAPABILITIES=direct-tools",
		"KIMI_SECONDARY_MODEL=direct/secondary", "KIMI_CODE_OAUTH_HOST=https://oauth.invalid",
		"KIMI_CODE_EXPERIMENTAL_SECONDARY_MODEL=0",
		"KIMI_CODE_LEGACY_FLAG=1",
		"KIMI_CODE_CUSTOM_HEADERS=X-Direct-Gateway-Secret: custom-header-secret",
		"SUBROUTER_ADMIN_TOKEN=durable-admin-secret",
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE=/private/import-token",
		"SUBROUTER_FUTURE_KEY=future-subrouter-secret",
		"SUBROUTER_CLOUD_CONFIG=/private/cloud-config",
		"SUBROUTER_STATE_DIR=/private/state",
		"HTTP_PROXY=http://credential-sink.invalid", "https_proxy=http://credential-sink.invalid",
		"ALL_PROXY=socks5://credential-sink.invalid", "NO_PROXY=vendor.invalid",
	}
	qwenRelay := "http://127.0.0.1:43210/private-relay-capability"
	qwenProviderURL := qwenRelay + "/qwen-token/v1"
	qwenEnv, qwenCleanup, err := nativeProxyEnvironment(qwenNativeProxy, qwenRelay, original, []string{"--model", "qwen-test-model"})
	if err != nil {
		t.Fatal(err)
	}
	defer qwenCleanup()
	joined := strings.Join(qwenEnv, "\n")
	for _, secret := range []string{"real-openai-secret", "real-coding-plan-secret", "real-bailian-secret", "real-kimi-secret", "custom-header-secret", "direct-org-secret", "direct-project-secret", "direct-relaunch-model", "direct-relaunch-secret", "durable-admin-secret", "/private/import-token", "future-subrouter-secret", "/private/cloud-config", "/private/state", "vendor.invalid", "credential-sink.invalid"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("Qwen child environment leaked %q:\n%s", secret, joined)
		}
	}
	if got, exists := testEnvEntry(qwenEnv, "QWEN_CODE_RELAUNCH_ARGS"); exists {
		t.Fatalf("Qwen child QWEN_CODE_RELAUNCH_ARGS = %q, want removed", got)
	}
	if got, exists := testEnvEntry(qwenEnv, "QWEN_SANDBOX"); exists {
		t.Fatalf("Qwen child QWEN_SANDBOX = %q, want removed", got)
	}
	if got := testEnvValue(qwenEnv, "OPENAI_BASE_URL"); got != qwenProviderURL {
		t.Fatalf("OPENAI_BASE_URL = %q", got)
	}
	if got := testEnvValue(qwenEnv, "QWEN_HOME"); got != "/custom/qwen-home" {
		t.Fatalf("QWEN_HOME changed from its normal value: %q", got)
	}
	for key, want := range map[string]string{"QWEN_CODE_SIMPLE": "1", "QWEN_DISABLED_SLASH_COMMANDS": "auth,model"} {
		if got := testEnvValue(qwenEnv, key); got != want {
			t.Fatalf("Qwen child %s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"BAILIAN_CODING_PLAN_API_KEY", "BAILIAN_TOKEN_PLAN_API_KEY", "DASHSCOPE_API_KEY"} {
		if got := testEnvValue(qwenEnv, key); got != "subrouter" {
			t.Fatalf("Qwen child %s = %q, want a non-secret sentinel", key, got)
		}
	}
	for key, want := range map[string]string{
		"HTTP_PROXY": "http://127.0.0.1:43210", "HTTPS_PROXY": "http://127.0.0.1:43210", "ALL_PROXY": "http://127.0.0.1:43210",
		"http_proxy": "http://127.0.0.1:43210", "https_proxy": "http://127.0.0.1:43210", "all_proxy": "http://127.0.0.1:43210",
		"NO_PROXY": "*", "no_proxy": "*",
	} {
		if got, exists := testEnvEntry(qwenEnv, key); !exists || got != want {
			t.Fatalf("Qwen child %s = %q exists=%t, want %q", key, got, exists, want)
		}
	}
	settings := testEnvValue(qwenEnv, "QWEN_CODE_SYSTEM_SETTINGS_PATH")
	body, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var overlay struct {
		Proxy           *string           `json:"proxy"`
		Env             map[string]string `json:"env"`
		FastModel       string            `json:"fastModel"`
		AdvisorModel    string            `json:"advisorModel"`
		VisionModel     string            `json:"visionModel"`
		CompactionModel string            `json:"compactionModel"`
		ImageModel      string            `json:"imageModel"`
		VoiceModel      string            `json:"voiceModel"`
		ModelFallbacks  string            `json:"modelFallbacks"`
		SlashCommands   struct {
			Disabled []string `json:"disabled"`
		} `json:"slashCommands"`
		ModelProviders map[string][]struct {
			ID      string `json:"id"`
			BaseURL string `json:"baseUrl"`
		} `json:"modelProviders"`
	}
	if err := json.Unmarshal(body, &overlay); err != nil {
		t.Fatal(err)
	}
	providers := overlay.ModelProviders["openai"]
	if len(overlay.ModelProviders) != 5 || len(providers) != 1 || providers[0].ID != "qwen-test-model" || providers[0].BaseURL != qwenProviderURL {
		t.Fatalf("Qwen overlay = %+v", overlay)
	}
	for _, provider := range []string{"anthropic", "gemini", "vertex-ai", "qwen-oauth"} {
		if models, exists := overlay.ModelProviders[provider]; !exists || len(models) != 0 {
			t.Fatalf("Qwen overlay provider %q = %+v exists=%t, want an explicit empty catalog", provider, models, exists)
		}
	}
	if overlay.FastModel != "" || overlay.AdvisorModel != "" || overlay.VisionModel != "" ||
		overlay.CompactionModel != "" || overlay.ImageModel != "" || overlay.VoiceModel != "" || overlay.ModelFallbacks != "" {
		t.Fatalf("Qwen overlay retained an alternate model selector: %+v", overlay)
	}
	if overlay.Proxy == nil || *overlay.Proxy != " " {
		t.Fatalf("Qwen overlay proxy = %v, want truthy whitespace that Qwen normalizes to disabled", overlay.Proxy)
	}
	for _, key := range []string{"BAILIAN_CODING_PLAN_API_KEY", "BAILIAN_TOKEN_PLAN_API_KEY", "DASHSCOPE_API_KEY"} {
		if value, exists := overlay.Env[key]; !exists || value != "subrouter" {
			t.Fatalf("Qwen overlay env[%q] = %q exists=%t, want a non-secret sentinel", key, value, exists)
		}
	}
	for key, want := range map[string]string{
		"HTTP_PROXY": "http://127.0.0.1:43210", "HTTPS_PROXY": "http://127.0.0.1:43210", "ALL_PROXY": "http://127.0.0.1:43210",
		"http_proxy": "http://127.0.0.1:43210", "https_proxy": "http://127.0.0.1:43210", "all_proxy": "http://127.0.0.1:43210",
		"NO_PROXY": "*", "no_proxy": "*",
	} {
		if got, exists := overlay.Env[key]; !exists || got != want {
			t.Fatalf("Qwen overlay env[%q] = %q exists=%t, want %q", key, got, exists, want)
		}
	}
	if !reflect.DeepEqual(overlay.SlashCommands.Disabled, []string{"auth", "model"}) {
		t.Fatalf("Qwen disabled slash commands = %q, want auth and model only", overlay.SlashCommands.Disabled)
	}

	kimiEnv, kimiCleanup, err := nativeProxyEnvironment(kimiNativeProxy, "http://127.0.0.1:43211", original, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer kimiCleanup()
	if got, exists := testEnvEntry(kimiEnv, "KIMI_CODE_LEGACY_FLAG"); exists {
		t.Fatalf("Kimi child KIMI_CODE_LEGACY_FLAG = %q, want removed so Kimi 0.39 uses its resumable v2 engine", got)
	}
	kimiChildHome := testEnvValue(kimiEnv, "KIMI_CODE_HOME")
	if kimiChildHome == "" || kimiChildHome == kimiSourceHome || !strings.HasPrefix(filepath.Base(kimiChildHome), "subrouter-kimi-proxy-") {
		t.Fatalf("KIMI_CODE_HOME = %q, want a fresh routed child home distinct from %q", kimiChildHome, kimiSourceHome)
	}
	if got := testEnvValue(kimiEnv, "KIMI_MODEL_API_KEY"); got != "subrouter" {
		t.Fatalf("KIMI_MODEL_API_KEY = %q", got)
	}
	for key, want := range map[string]string{
		"KIMI_CODE_NO_AUTO_UPDATE":               "1",
		"KIMI_CODE_EXPERIMENTAL_SECONDARY_MODEL": "1",
		"KIMI_MODEL_NAME":                        "kimi-for-coding",
		"KIMI_MODEL_MAX_CONTEXT_SIZE":            "262144",
		"KIMI_SECONDARY_MODEL":                   "__kimi_env_model__",
	} {
		if got := testEnvValue(kimiEnv, key); got != want {
			t.Fatalf("Kimi child %s = %q, want %q", key, got, want)
		}
	}
	joinedKimi := strings.Join(kimiEnv, "\n")
	for _, secret := range []string{"real-kimi-secret", "custom-header-secret", "direct-tools", "direct/secondary", "oauth.invalid"} {
		if strings.Contains(joinedKimi, secret) {
			t.Fatalf("Kimi child environment retained direct credential %q", secret)
		}
	}
}

func TestKimiProxyHomeIsolatesCatalogAndCredentialsWhileLinkingOnlySessions(t *testing.T) {
	source := filepath.Join(t.TempDir(), "real-kimi-home")
	source = prepareKimiTestSessionHome(t, source)
	sourceConfig := []byte("default_model = \"direct/private-model\"\n[providers.direct]\napi_key = \"real-secret\"\n")
	if err := os.WriteFile(filepath.Join(source, "config.toml"), sourceConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "oauth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "oauth", "kimi-code"), []byte("real-oauth-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	indexBefore, err := os.ReadFile(filepath.Join(source, "session_index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(source, "sessions", "existing-session")
	if err := os.WriteFile(marker, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	overlay, cleanup, err := prepareKimiProxyHome([]string{"KIMI_CODE_HOME=" + source})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(overlay.home, "config.toml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	if string(config) != kimiProxyConfig || strings.Contains(string(config), "direct") || strings.Contains(string(config), "api_key") || strings.Contains(string(config), "oauth") {
		cleanup()
		t.Fatalf("routed Kimi config was not minimal and isolated:\n%s", config)
	}
	for path, wantPerm := range map[string]os.FileMode{overlay.home: 0o700, configPath: 0o400} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			cleanup()
			t.Fatal(statErr)
		}
		if got := info.Mode().Perm(); got != wantPerm {
			cleanup()
			t.Fatalf("%s permissions = %o, want %o", path, got, wantPerm)
		}
	}
	for name, want := range map[string]string{
		"sessions":            filepath.Join(source, "sessions"),
		"session_index.jsonl": filepath.Join(source, "session_index.jsonl"),
	} {
		got, readErr := os.Readlink(filepath.Join(overlay.home, name))
		if readErr != nil || got != want {
			cleanup()
			t.Fatalf("routed Kimi %s link = %q, %v; want %q", name, got, readErr, want)
		}
	}
	for _, forbidden := range []string{"oauth", "device_id", "tui.toml", "mcp.json"} {
		if _, statErr := os.Lstat(filepath.Join(overlay.home, forbidden)); !errors.Is(statErr, os.ErrNotExist) {
			cleanup()
			t.Fatalf("routed Kimi home exposed %s: %v", forbidden, statErr)
		}
	}
	// Kimi requires a writable root for its own atomic workspace catalog. Prove
	// that even a deliberate child-local credential write stays inside the
	// disposable home and cannot replace the user's real OAuth state.
	if err := os.Mkdir(filepath.Join(overlay.home, "oauth"), 0o700); err != nil {
		cleanup()
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlay.home, "oauth", "ephemeral"), []byte("child-only"), 0o600); err != nil {
		cleanup()
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Lstat(overlay.home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("routed Kimi home survived cleanup: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(source, "config.toml")); err != nil || !bytes.Equal(got, sourceConfig) {
		t.Fatalf("real Kimi config changed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(source, "session_index.jsonl")); err != nil || !bytes.Equal(got, indexBefore) {
		t.Fatalf("real Kimi session index changed during overlay setup/cleanup: %q, %v", got, err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "keep me" {
		t.Fatalf("real Kimi session content changed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(source, "oauth", "kimi-code")); err != nil || string(got) != "real-oauth-token" {
		t.Fatalf("real Kimi OAuth content changed: %q, %v", got, err)
	}
}

func TestKimiProxyCleanupNeverFollowsChildSymlinks(t *testing.T) {
	source := filepath.Join(t.TempDir(), "real-kimi-home")
	source = prepareKimiTestSessionHome(t, source)
	overlay, cleanup, err := prepareKimiProxyHome([]string{"KIMI_CODE_HOME=" + source})
	if err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	externalMarker := filepath.Join(external, "must-survive")
	if err := os.WriteFile(externalMarker, []byte("safe"), 0o600); err != nil {
		cleanup()
		t.Fatal(err)
	}
	if err := os.Chmod(overlay.home, 0o700); err != nil {
		cleanup()
		t.Fatal(err)
	}
	localDir := filepath.Join(overlay.home, "logs")
	if err := os.Symlink(external, filepath.Join(localDir, "outside")); err != nil {
		cleanup()
		t.Fatal(err)
	}
	nested := filepath.Join(localDir, "child-created", "sealed")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		cleanup()
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "private"), []byte("ephemeral"), 0o600); err != nil {
		cleanup()
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(nested, "outside")); err != nil {
		cleanup()
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0); err != nil {
		cleanup()
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(nested), 0); err != nil {
		cleanup()
		t.Fatal(err)
	}
	if err := os.Chmod(overlay.home, 0o500); err != nil {
		cleanup()
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("clean nested child-created directories: %v", err)
	}
	if got, err := os.ReadFile(externalMarker); err != nil || string(got) != "safe" {
		t.Fatalf("child cleanup followed an external symlink: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(source, "sessions")); err != nil {
		t.Fatalf("child cleanup followed the real sessions link: %v", err)
	}
	if _, err := os.Lstat(overlay.home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only routed Kimi home survived cleanup: %v", err)
	}
}

func TestKimiProxyCleanupDoesNotFollowReplacedRoot(t *testing.T) {
	source := filepath.Join(t.TempDir(), "real-kimi-home")
	source = prepareKimiTestSessionHome(t, source)
	overlay, cleanup, err := prepareKimiProxyHome([]string{"KIMI_CODE_HOME=" + source})
	if err != nil {
		t.Fatal(err)
	}
	detached := overlay.home + "-detached"
	if err := os.Rename(overlay.home, detached); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(detached) })
	external := t.TempDir()
	marker := filepath.Join(external, "must-survive")
	if err := os.WriteFile(marker, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, overlay.home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(overlay.home) })
	err = cleanup()
	if err == nil || !strings.Contains(err.Error(), "basename was replaced") {
		t.Fatalf("replacement race was not reported: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "safe" {
		t.Fatalf("cleanup followed a replaced root: %q, %v", got, err)
	}
	if info, err := os.Lstat(overlay.home); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("replacement link was changed: info=%v err=%v", info, err)
	}
	entries, err := os.ReadDir(detached)
	if err != nil || len(entries) != 0 {
		t.Fatalf("pinned original home was not emptied: entries=%v err=%v", entries, err)
	}
}

func TestNativeProxyLaunchSurfacesCleanupFailure(t *testing.T) {
	runErr := errors.New("child failed")
	cleanupErr := errors.New("cleanup failed")
	err := joinNativeProxyRunAndCleanupErrors("Kimi", runErr, func() error { return cleanupErr })
	if !errors.Is(err, runErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("joined error = %v, want child and cleanup failures", err)
	}
	if !strings.Contains(err.Error(), "clean up temporary Kimi proxy home") {
		t.Fatalf("cleanup error lacks context: %v", err)
	}
}

func TestQwenProxyEnvironmentReturnsCleanupFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory permissions do not provide the Unix cleanup failure used by this regression")
	}
	tempParent := t.TempDir()
	t.Setenv("TMPDIR", tempParent)
	env, cleanup, err := nativeProxyEnvironment(qwenNativeProxy, "http://127.0.0.1:43123", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	overlayRoot := filepath.Dir(testEnvValue(env, "QWEN_CODE_SYSTEM_SETTINGS_PATH"))
	if overlayRoot == "." || !strings.HasPrefix(filepath.Base(overlayRoot), "subrouter-qwen-proxy-") {
		t.Fatalf("Qwen overlay root = %q", overlayRoot)
	}
	if err := os.Chmod(tempParent, 0); err != nil {
		t.Fatal(err)
	}
	cleanupErr := cleanup()
	if err := os.Chmod(tempParent, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removePrivateProxyHome(overlayRoot) })
	if cleanupErr == nil {
		t.Fatal("Qwen environment swallowed its private-overlay cleanup failure")
	}
}

func TestKimiProxySessionLinksFailClosedOnWindows(t *testing.T) {
	err := kimiProxySessionLinksSupported("windows")
	if err == nil || !strings.Contains(err.Error(), "not supported on Windows") {
		t.Fatalf("Windows session-link gate = %v", err)
	}
	if err := kimiProxySessionLinksSupported("darwin"); err != nil {
		t.Fatalf("Darwin session-link gate = %v", err)
	}
	if err := kimiProxySessionLinksSupported("linux"); err != nil {
		t.Fatalf("Linux session-link gate = %v", err)
	}
}

func TestKimiProxyHomesAreUniqueAcrossConcurrentLaunches(t *testing.T) {
	source := filepath.Join(t.TempDir(), "real-kimi-home")
	source = prepareKimiTestSessionHome(t, source)
	type result struct {
		overlay kimiProxyOverlay
		cleanup func() error
		err     error
	}
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			start.Done()
			start.Wait()
			overlay, cleanup, err := prepareKimiProxyHome([]string{"KIMI_CODE_HOME=" + source})
			results <- result{overlay: overlay, cleanup: cleanup, err: err}
		}()
	}
	first := <-results
	second := <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent Kimi homes failed: %v / %v", first.err, second.err)
	}
	defer second.cleanup()
	if first.overlay.home == second.overlay.home {
		first.cleanup()
		t.Fatalf("concurrent Kimi launches shared home %q", first.overlay.home)
	}
	first.cleanup()
	if _, err := os.Stat(filepath.Join(second.overlay.home, "config.toml")); err != nil {
		t.Fatalf("cleaning one Kimi home removed the other: %v", err)
	}
	if got, err := os.Readlink(filepath.Join(second.overlay.home, "sessions")); err != nil || got != filepath.Join(source, "sessions") {
		t.Fatalf("second Kimi session link = %q, %v", got, err)
	}
}

func TestKimiSessionIndexFirstCreationIsConcurrentSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_index.jsonl")
	const launches = 8
	errorsSeen := make(chan error, launches)
	var start sync.WaitGroup
	start.Add(launches)
	for i := 0; i < launches; i++ {
		go func() {
			start.Done()
			start.Wait()
			errorsSeen <- ensureKimiSessionIndex(path)
		}()
	}
	for i := 0; i < launches; i++ {
		if err := <-errorsSeen; err != nil {
			t.Fatalf("concurrent first Kimi session-index creation failed: %v", err)
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("concurrent Kimi session index = %v, %v", info, err)
	}
}

func TestKimiProxySessionLinksRejectIndirectSources(t *testing.T) {
	for _, name := range []string{"sessions", "session_index.jsonl"} {
		t.Run(name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "real-kimi-home")
			if err := os.MkdirAll(source, 0o700); err != nil {
				t.Fatal(err)
			}
			external := t.TempDir()
			if name == "sessions" {
				if err := os.WriteFile(filepath.Join(source, "session_index.jsonl"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(filepath.Join(source, "sessions"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(external, "index"), []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				external = filepath.Join(external, "index")
			}
			if err := os.Symlink(external, filepath.Join(source, name)); err != nil {
				t.Fatal(err)
			}
			_, cleanup, err := prepareKimiProxyHome([]string{"KIMI_CODE_HOME=" + source})
			cleanup()
			if err == nil || !strings.Contains(err.Error(), "not a link") {
				t.Fatalf("indirect %s source error = %v", name, err)
			}
		})
	}
}

func TestQwenNativeProxyArgsForceRoutingAndPreserveChosenModel(t *testing.T) {
	baseURL := "http://127.0.0.1:43210/private-relay-capability/qwen-token/v1"
	for _, test := range []struct {
		input []string
		model string
	}{
		{input: nil, model: defaultQwenProxyModel},
		{input: []string{"--model", "qwen-custom"}, model: "qwen-custom"},
		{input: []string{"-p", "hello", "--model=qwen-equals"}, model: "qwen-equals"},
		{input: []string{"-m=qwen-short-equals"}, model: "qwen-short-equals"},
		{input: []string{"--fallback-model", "direct-model"}, model: defaultQwenProxyModel},
		{input: []string{"--no-bare"}, model: defaultQwenProxyModel},
	} {
		model, err := qwenProxyModel(test.input)
		if err != nil {
			t.Fatalf("qwenProxyModel(%q): %v", test.input, err)
		}
		if model != test.model {
			t.Fatalf("qwenProxyModel(%q) = %q, want %q", test.input, model, test.model)
		}
		got := qwenNativeProxyArgs(test.input, model)
		joined := strings.Join(got, " ")
		for _, want := range []string{"--bare", "--auth-type openai", "--openai-api-key subrouter", "--model " + model} {
			if !strings.Contains(joined, want) {
				t.Fatalf("qwen proxy args %q do not contain %q", got, want)
			}
		}
		if strings.Contains(joined, baseURL) {
			t.Fatalf("qwen proxy args exposed the private relay capability: %q", got)
		}
		if strings.Count(joined, "--model") != 1 || strings.Contains(joined, "-m=") {
			t.Fatalf("qwen proxy args retained a competing model: %q", got)
		}
		if strings.Contains(joined, "fallback-model") || strings.Contains(joined, "direct-model") {
			t.Fatalf("qwen proxy args retained a fallback route: %q", got)
		}
		if strings.Contains(joined, "no-bare") || strings.Count(joined, "--bare") != 1 {
			t.Fatalf("qwen proxy args did not force bare mode: %q", got)
		}
	}
}

func TestQwenProxyOverlayOverridesSavedProviderCatalogAndModelRoles(t *testing.T) {
	baseURL := "http://127.0.0.1:43210/private-relay-capability/qwen-token/v1"
	overlay, cleanup, err := prepareQwenProxyOverlay(baseURL, "qwen-routed", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	body, err := os.ReadFile(overlay.settings)
	if err != nil {
		t.Fatal(err)
	}
	var system map[string]any
	if err := json.Unmarshal(body, &system); err != nil {
		t.Fatal(err)
	}
	saved := map[string]any{
		"modelProviders": map[string]any{
			"openai":     []any{map[string]any{"id": "direct-openai", "baseUrl": "https://openai.invalid/v1", "envKey": "DIRECT_OPENAI_KEY"}},
			"anthropic":  []any{map[string]any{"id": "direct-anthropic", "baseUrl": "https://anthropic.invalid", "envKey": "DIRECT_ANTHROPIC_KEY"}},
			"gemini":     []any{map[string]any{"id": "direct-gemini", "baseUrl": "https://gemini.invalid", "envKey": "DIRECT_GEMINI_KEY"}},
			"vertex-ai":  []any{map[string]any{"id": "direct-vertex"}},
			"qwen-oauth": []any{map[string]any{"id": "direct-qwen"}},
			"private":    []any{map[string]any{"id": "direct-private", "baseUrl": "https://private.invalid/v1", "envKey": "DIRECT_PRIVATE_KEY"}},
		},
		"providerProtocol": map[string]any{"private": "openai"},
		"fastModel":        "gemini:direct-gemini",
		"advisorModel":     "anthropic:direct-anthropic",
		"visionModel":      "gemini:direct-gemini",
		"compactionModel":  "qwen-oauth:direct-qwen",
		"imageModel":       "vertex-ai:direct-vertex",
		"voiceModel":       "openai:direct-openai",
		"modelFallbacks":   "anthropic:direct-anthropic,gemini:direct-gemini",
	}
	merged := mergeQwenSettingsForTest(saved, system)
	mergedProviders := merged["modelProviders"].(map[string]any)
	if custom, ok := mergedProviders["private"].([]any); !ok || len(custom) != 1 {
		t.Fatalf("hostile custom provider did not survive Qwen's deep merge: %#v", mergedProviders["private"])
	}
	// Qwen 0.22's --bare startup calls createMinimalSettings instead of
	// loadSettings, so none of the merged persistent catalog is effective. The
	// only effective provider/model inputs are the forced CLI arguments.
	effective := loadQwenSettingsForTest(true, saved, system)
	if len(effective) != 0 {
		t.Fatalf("bare Qwen settings = %#v, want no persisted settings", effective)
	}
	forced := strings.Join(qwenNativeProxyArgs(nil, "qwen-routed"), " ")
	for _, want := range []string{"--bare", "--auth-type openai", "--model qwen-routed", "--openai-api-key subrouter"} {
		if !strings.Contains(forced, want) {
			t.Fatalf("forced Qwen route %q does not contain %q", forced, want)
		}
	}

	// The system overlay remains defense in depth for every built-in provider
	// and selector if Qwen ever consults settings despite the forced bare flag.
	effective = mergeQwenSettingsForTest(saved, system)
	providers := effective["modelProviders"].(map[string]any)
	for _, provider := range []string{"anthropic", "gemini", "vertex-ai", "qwen-oauth"} {
		models, ok := providers[provider].([]any)
		if !ok || len(models) != 0 {
			t.Fatalf("effective %s catalog = %#v, want explicit empty replacement", provider, providers[provider])
		}
	}
	openAI, ok := providers["openai"].([]any)
	if !ok || len(openAI) != 1 {
		t.Fatalf("effective openai catalog = %#v", providers["openai"])
	}
	routed := openAI[0].(map[string]any)
	if routed["id"] != "qwen-routed" || routed["baseUrl"] != baseURL {
		t.Fatalf("effective routed model = %#v", routed)
	}
	for _, selector := range []string{"fastModel", "advisorModel", "visionModel", "compactionModel", "imageModel", "voiceModel", "modelFallbacks"} {
		if got, exists := effective[selector]; !exists || got != "" {
			t.Fatalf("effective %s = %#v exists=%t, want explicit empty override", selector, got, exists)
		}
	}
}

func TestQwenProxyOverlayRefusesExistingSystemPolicy(t *testing.T) {
	for _, environ := range [][]string{
		{"QWEN_CODE_SYSTEM_SETTINGS_PATH=/managed/settings.json"},
		{"QWEN_CODE_SYSTEM_DEFAULTS_PATH=/managed/defaults.json"},
	} {
		_, cleanup, err := prepareQwenProxyOverlay("http://127.0.0.1/qwen-token/v1", "qwen-test", environ)
		cleanup()
		if err == nil || !strings.Contains(err.Error(), "refusing a proxy overlay") {
			t.Fatalf("policy environment error = %v", err)
		}
	}
	if got := qwenSystemPolicyConflict([]string{"qwen_code_system_settings_path=C:\\managed\\settings.json"}, "windows"); got != "QWEN_CODE_SYSTEM_SETTINGS_PATH" {
		t.Fatalf("case-insensitive Windows policy conflict = %q", got)
	}
	policyPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(policyPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := qwenSystemPolicyConflictAtPaths(nil, []string{policyPath}); got != policyPath {
		t.Fatalf("system policy conflict = %q, want %q", got, policyPath)
	}
}

func TestQwenProxyRejectsClientProxyOverrideBeforeLaunch(t *testing.T) {
	for _, args := range [][]string{{"--proxy", "http://proxy.invalid"}, {"--proxy=http://proxy.invalid"}, {"--", "--proxy", "http://proxy.invalid"}, {"--fallback-model", "direct-model"}, {"--fallback-model=direct-model"}, {"-s"}, {"-sy"}, {"-ys"}, {"--sandbox"}, {"--sandbox=true"}} {
		err := (srRunner{}).launchQwenProxy(t.Context(), args)
		if err == nil || !strings.Contains(err.Error(), "controls Qwen routing") {
			t.Fatalf("routing override %q error = %v", args, err)
		}
		if strings.Contains(err.Error(), "proxy.invalid") || strings.Contains(err.Error(), "direct-model") {
			t.Fatalf("routing override error exposed the supplied target: %v", err)
		}
	}
	for _, args := range [][]string{{"--model", "qwen-oauth:coder-model"}, {"--model=openai:direct-model"}, {"-m=qwen-oauth:coder-model"}} {
		err := (srRunner{}).launchQwenProxy(t.Context(), args)
		if err == nil || !strings.Contains(err.Error(), "provider-qualified Qwen models") {
			t.Fatalf("provider-qualified model %q error = %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"serve"},
		{"--safe-mode", "serve"},
		{"--approval-mode", "default", "serve"},
		{"--telemetry-target", "local", "serve"},
		{"--allowed-tools", "Shell(git status)", "--acp"},
		{"--acp"},
		{"--model", "--acp"},
		{"--experimental-acp=true"},
		{"review", "run"},
		{"channel", "start"},
		{"--approval-mode", "default", "channel", "start"},
		{"channel", "daemon-worker", "--channel", "work"},
		{"channel", "reload"},
		{"channel", "--telemetry-target", "local", "reload"},
		{"--telemetry-target", "local", "channel", "reload"},
		{"channel", "set", "work"},
	} {
		err := (srRunner{}).launchQwenProxy(t.Context(), args)
		if err == nil || !strings.Contains(err.Error(), "can reload saved credentials and proxies") {
			t.Fatalf("reload-capable mode %q error = %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"-p", "serve"},
		{"-p", "channel"},
		{"-p", "start"},
		{"-p", "channel start"},
		{"--model", "channel", "-p", "start"},
		{"--model", "serve", "--continue"},
		{"-p", "review"},
		{"--system-prompt", "serve"},
		{"channel", "stop"},
		{"channel", "status"},
		{"channel", "pairing", "list", "work"},
		{"channel", "configure-weixin", "status"},
		{"--", "serve"},
		{"--", "--acp"},
	} {
		if qwenProxyReloadCapableMode(args) {
			t.Fatalf("ordinary Qwen args %q classified as reload-capable", args)
		}
	}
}

func TestQwenProxyBundledShortOptionsMatchYargsGrammar(t *testing.T) {
	for _, test := range []struct {
		args       []string
		restricted string
	}{
		{args: []string{"-cy"}, restricted: "cr"},
		{args: []string{"-sc"}, restricted: "s"},
		{args: []string{"-ys"}, restricted: "s"},
		{args: []string{"-yrsession-id"}, restricted: "cr"},
		{args: []string{"-smstream"}, restricted: "s"},
		{args: []string{"-mc"}, restricted: "cr"},
		{args: []string{"-ms"}, restricted: "s"},
		{args: []string{"-mstream"}, restricted: "scr"},
		{args: []string{"-pfancy"}, restricted: "cr"},
		{args: []string{"-icontinue"}, restricted: "cr"},
		{args: []string{"-ostream"}, restricted: "scr"},
		{args: []string{"-ymstream"}, restricted: "scr"},
		{args: []string{"-m", "-cy"}, restricted: "cr"},
		{args: []string{"-p", "-sc"}, restricted: "s"},
	} {
		if !qwenProxyBundledShortOptionRequested(test.args, test.restricted) {
			t.Fatalf("bundled Qwen option %q did not activate one of %q", test.args, test.restricted)
		}
	}

	for _, args := range [][]string{
		{"-m=stream"},
		{"-p=-cy"},
		{"-i=-sc"},
		{"-o=stream"},
		{"--model=-sc"},
		{"--", "-cy"},
	} {
		if qwenProxyBundledShortOptionRequested(args, "scr") {
			t.Fatalf("Qwen attached value or post-delimiter data %q was treated as a bundled routing option", args)
		}
	}
	if err := (srRunner{}).launchQwenProxy(t.Context(), []string{"-p", "-sc"}); err == nil {
		t.Fatal("dash-prefixed Qwen prompt follower restored sandbox/session routing")
	}
}

func TestKimiProxyRejectsCredentialAndServerModesBeforeLaunch(t *testing.T) {
	for _, args := range [][]string{
		{"login"}, {"--yolo", "provider"}, {"--", "acp"}, {"--auto", "web"}, {"--plan", "server"},
		{"migrate"}, {"upgrade"}, {"update"},
	} {
		mode := kimiProxyReloadCapableMode(args)
		if mode == "" {
			t.Fatalf("Kimi control mode %q was not detected", args)
		}
		err := (srRunner{}).launchKimiProxy(t.Context(), args)
		if err == nil || !strings.Contains(err.Error(), "plain 'kimi "+mode+"'") {
			t.Fatalf("Kimi control mode %q error = %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"-p", "login"}, {"--model", "web", "--continue"}, {"--agent", "server", "--continue"}, {"export", "session-id"},
	} {
		if mode := kimiProxyReloadCapableMode(args); mode != "" {
			t.Fatalf("ordinary Kimi args %q classified as %q", args, mode)
		}
	}
}

func TestKimiProxyFailsClosedWithoutNonInteractivePromptMode(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--continue"},
		{"--session", "session-id"},
		{"--resume=session-id"},
		{"--model", "kimi-for-coding"},
		{"--prompt="},
		{"-p", ""},
		{"--"},
		{"--add-dir", "-p", "--yolo"},
		{"--model", "-p", "hello"},
		{"vis", "-p", "hello"},
		{"-p", "hello", "vis"},
		{"--future-option", "-p", "hello"},
	} {
		if kimiProxyPromptModeRequested(args) {
			t.Fatalf("interactive Kimi args %q classified as prompt mode", args)
		}
		err := (srRunner{}).launchKimiProxy(t.Context(), args)
		if err == nil || !strings.Contains(err.Error(), "interactive 'sr kimi' is disabled") {
			t.Fatalf("interactive Kimi args %q error = %v", args, err)
		}
	}

	for _, args := range [][]string{
		{"-p", "hello"},
		{"--prompt", "/web"},
		{"--prompt=hello"},
		{"-p=hello"},
		{"-phello"},
		{"--add-dir", "workspace", "-p", "hello"},
		{"--session", "session-id", "-p", "continue safely"},
		{"--resume=session-id", "--prompt=continue safely"},
		{"--continue", "-p", "continue safely"},
	} {
		if !kimiProxyPromptModeRequested(args) {
			t.Fatalf("non-interactive Kimi args %q were rejected", args)
		}
	}
	if kimiProxyPromptModeRequested([]string{"--", "-p", "prompt after vendor delimiter"}) {
		t.Fatal("prompt flag after Kimi's own option delimiter was accepted")
	}
	_, vendorArgs, err := parseNativeProxyLaunchArgs([]string{"--", "-p", "prompt after wrapper delimiter"})
	if err != nil || !kimiProxyPromptModeRequested(vendorArgs) {
		t.Fatalf("wrapper-delimited prompt args = %q, %v", vendorArgs, err)
	}
}

func TestNativeProxyRejectsResumePickerWithoutStickySessionID(t *testing.T) {
	for _, args := range [][]string{{"--resume"}, {"-r"}, {"--resume="}, {"--resume", ""}, {"-r", "   "}, {"--resume", "--model", "qwen-test"}} {
		if !nativeProxyResumePickerRequested(qwenNativeProxy, args) {
			t.Fatalf("picker resume %q was not detected", args)
		}
	}
	for _, args := range [][]string{{"--resume", "session-id"}, {"-r", "session-id"}, {"--resume=session-id"}, {"--", "--resume"}} {
		if nativeProxyResumePickerRequested(qwenNativeProxy, args) {
			t.Fatalf("explicit/non-option resume %q was rejected", args)
		}
	}
	for _, args := range [][]string{{"--resume"}, {"--resume", "session-id"}, {"--resume=session-id"}, {"-r=session-id"}, {"--continue"}, {"--continue=true"}, {"-c"}, {"-cy"}, {"-yc"}, {"-sc"}, {"-cs"}, {"-yr"}, {"-p", "--continue"}, {"-p", "-cy"}} {
		if !qwenProxyPersistentSessionRequested(args) {
			t.Fatalf("persistent session %q was not detected", args)
		}
		if err := (srRunner{}).launchQwenProxy(t.Context(), args); err == nil || !strings.Contains(err.Error(), "restore a saved direct provider route") {
			t.Fatalf("persistent session %q launch error = %v", args, err)
		}
	}
	for _, args := range [][]string{{"--prompt=-cy"}, {"--", "--resume", "session-id"}, {"--", "-cy"}} {
		if qwenProxyPersistentSessionRequested(args) {
			t.Fatalf("non-option session text %q was rejected", args)
		}
	}
	for _, args := range [][]string{{"-m"}, {"--model"}, {"-m", "-cy"}, {"--model", "--acp"}, {"-m="}, {"--model="}} {
		if _, err := qwenProxyModel(args); err == nil {
			t.Fatalf("invalid Qwen model args %q were accepted", args)
		}
		if err := (srRunner{}).launchQwenProxy(t.Context(), args); err == nil {
			t.Fatalf("invalid Qwen model launch %q was accepted", args)
		}
	}
	for _, args := range [][]string{{"--session"}, {"-S"}, {"--resume"}, {"-r"}, {"--session="}, {"--resume="}, {"--session", ""}, {"-r", "   "}, {"--session", "--model", "kimi-test"}, {"--session", "session-id", "--session", "-p", "hello"}} {
		if !nativeProxyResumePickerRequested(kimiNativeProxy, args) {
			t.Fatalf("Kimi picker resume %q was not detected", args)
		}
	}
	for _, args := range [][]string{{"--session", "session-id"}, {"-S", "session-id"}, {"--resume", "session-id"}, {"-r", "session-id"}, {"--resume=session-id"}} {
		if nativeProxyResumePickerRequested(kimiNativeProxy, args) {
			t.Fatalf("explicit Kimi resume %q was rejected", args)
		}
	}
	for _, args := range [][]string{
		{"-p", "--resume"},
		{"--prompt", "--session"},
		{"--agent", "--resume", "-p", "hello"},
		{"--add-dir", "--session", "-p", "hello"},
	} {
		if nativeProxyResumePickerRequested(kimiNativeProxy, args) {
			t.Fatalf("required Kimi option value %q was treated as a resume picker", args)
		}
		if !kimiProxyPromptModeRequested(args) {
			t.Fatalf("required Kimi option value %q did not preserve prompt mode", args)
		}
	}
	if err := (srRunner{}).launchKimiProxy(t.Context(), []string{"--session"}); err == nil || !strings.Contains(err.Error(), "explicit session ID") {
		t.Fatalf("Kimi picker launch error = %v", err)
	}
}

func TestNativeProxyDataPlanePreflightRejectsLeaseRequiredRouter(t *testing.T) {
	for _, test := range []struct {
		status  int
		wantErr bool
	}{{status: http.StatusNoContent}, {status: http.StatusUnauthorized, wantErr: true}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodHead || request.URL.Path != "/" || request.Header.Get("Authorization") != "Bearer relay-token" {
				http.Error(w, "bad preflight", http.StatusBadRequest)
				return
			}
			w.WriteHeader(test.status)
		}))
		runner := srRunner{client: server.Client()}
		err := runner.requireNativeProxyDataPlane(t.Context(), server.URL, "relay-token")
		server.Close()
		if (err != nil) != test.wantErr {
			t.Fatalf("status %d preflight error = %v, wantErr=%t", test.status, err, test.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "no vendor CLI was started") {
			t.Fatalf("preflight error did not fail before launch: %v", err)
		}
	}
}

func TestNativeProxyAccountInventoryIsRequiredOnlyForHardPins(t *testing.T) {
	if nativeProxyNeedsAccountInventory(nativeProxyLaunchOptions{}) {
		t.Fatal("pooled native launch unexpectedly requires admin account inventory")
	}
	if !nativeProxyNeedsAccountInventory(nativeProxyLaunchOptions{pickPinnedAccount: true}) {
		t.Fatal("pinned-account picker did not require account inventory")
	}
	if !nativeProxyNeedsAccountInventory(nativeProxyLaunchOptions{accountSelector: "work"}) {
		t.Fatal("named hard pin did not require account inventory")
	}
}

func TestQwenDefaultSystemPolicyPathsCoverSupportedPlatforms(t *testing.T) {
	for _, test := range []struct {
		goos string
		want string
	}{
		{goos: "darwin", want: "/Library/Application Support/QwenCode/settings.json"},
		{goos: "linux", want: "/etc/qwen-code/settings.json"},
		{goos: "windows", want: `C:\ProgramData`},
	} {
		paths := qwenDefaultSystemPolicyPaths(nil, test.goos)
		if len(paths) != 2 || !strings.Contains(paths[0], test.want) {
			t.Fatalf("%s Qwen policy paths = %q", test.goos, paths)
		}
	}
}

func TestKimiNativeProxyArgsForceEphemeralModelOnNewAndResumedSessions(t *testing.T) {
	for _, input := range [][]string{
		{"--continue"},
		{"--session", "session-id", "--model", "direct/model"},
		{"-p", "hello", "-m", "direct-model"},
		{"-m=direct-equals-model"},
		{"-p", "--model=prompt-text"},
		{"--agent", "--model=agent-name", "-p", "hello"},
	} {
		got := kimiNativeProxyArgs(input)
		joined := strings.Join(got, " ")
		if !strings.Contains(joined, "--model __kimi_env_model__") {
			t.Fatalf("kimi proxy args = %q", got)
		}
		if strings.Contains(joined, "direct-model") || strings.Contains(joined, "direct/model") || strings.Contains(joined, "direct-equals-model") {
			t.Fatalf("direct model survived proxy args: %q", got)
		}
		if len(input) > 1 && (input[0] == "-p" || input[0] == "--agent") && !slices.Contains(got, input[1]) {
			t.Fatalf("required Kimi option value %q was rewritten: %q", input[1], got)
		}
	}
}

func TestNativeProxyWorkspaceSessionIdentityCoversNewAndContinue(t *testing.T) {
	for _, spec := range []nativeProxySpec{kimiNativeProxy, qwenNativeProxy, antigravityNativeProxy} {
		first, err := nativeProxySessionID(spec, nil)
		if err != nil {
			t.Fatal(err)
		}
		continued, continueErr := nativeProxySessionID(spec, []string{"--continue"})
		if continueErr != nil {
			t.Fatal(continueErr)
		}
		if continued != first {
			t.Fatalf("%s initial session %q changed on continue to %q", spec.provider, first, continued)
		}
		if !strings.HasPrefix(first, "sr-native-") {
			t.Fatalf("workspace session identity = %q", first)
		}
	}
	qwenID, _ := nativeProxySessionID(qwenNativeProxy, nil)
	kimiID, _ := nativeProxySessionID(kimiNativeProxy, nil)
	if qwenID == kimiID {
		t.Fatal("provider namespaces share a native proxy session ID")
	}
	qwenWork := nativeProxyPinnedSessionID(qwenID, "qwen-token:work")
	qwenPersonal := nativeProxyPinnedSessionID(qwenID, "qwen-token:personal")
	if qwenWork == qwenID || qwenPersonal == qwenID || qwenWork == qwenPersonal {
		t.Fatalf("pinned session identities collide: pooled=%q work=%q personal=%q", qwenID, qwenWork, qwenPersonal)
	}
	if again := nativeProxyPinnedSessionID(qwenID, "qwen-token:work"); again != qwenWork {
		t.Fatalf("pinned session identity is not stable: %q != %q", again, qwenWork)
	}
}

func TestKimiExplicitSessionIdentityIsStableAcrossWorkspaces(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	t.Chdir(workspaceA)

	workspaceSessionA, err := nativeProxySessionID(kimiNativeProxy, nil)
	if err != nil {
		t.Fatal(err)
	}
	explicitForms := [][]string{
		{"--session", "kimi-session-id"},
		{"-S", "kimi-session-id"},
		{"--session=kimi-session-id"},
		{"-Skimi-session-id"},
		{"-S=kimi-session-id"},
		{"--resume", "kimi-session-id"},
		{"-r", "kimi-session-id"},
		{"--resume=kimi-session-id"},
		{"-rkimi-session-id"},
		{"-r=kimi-session-id"},
	}
	var explicitIdentity string
	for _, args := range explicitForms {
		got, sessionErr := nativeProxySessionID(kimiNativeProxy, args)
		if sessionErr != nil {
			t.Fatal(sessionErr)
		}
		if explicitIdentity == "" {
			explicitIdentity = got
		} else if got != explicitIdentity {
			t.Fatalf("explicit Kimi session form %q identity = %q, want %q", args, got, explicitIdentity)
		}
	}
	if explicitIdentity == workspaceSessionA {
		t.Fatal("explicit Kimi session reused the workspace-scoped identity")
	}

	t.Chdir(workspaceB)
	workspaceSessionB, err := nativeProxySessionID(kimiNativeProxy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if workspaceSessionB == workspaceSessionA {
		t.Fatal("distinct Kimi workspaces shared the new-session identity")
	}
	continued, err := nativeProxySessionID(kimiNativeProxy, []string{"--continue"})
	if err != nil {
		t.Fatal(err)
	}
	if continued != workspaceSessionB {
		t.Fatalf("Kimi continue identity = %q, want workspace identity %q", continued, workspaceSessionB)
	}
	resumed, err := nativeProxySessionID(kimiNativeProxy, []string{"--session", "kimi-session-id"})
	if err != nil {
		t.Fatal(err)
	}
	if resumed != explicitIdentity {
		t.Fatalf("cross-workspace Kimi resume identity = %q, want %q", resumed, explicitIdentity)
	}
	different, err := nativeProxySessionID(kimiNativeProxy, []string{"--session", "different-session-id"})
	if err != nil {
		t.Fatal(err)
	}
	if different == explicitIdentity {
		t.Fatal("distinct explicit Kimi session IDs shared one identity")
	}
	for _, args := range [][]string{
		{"-p", "--session"},
		{"--prompt", "--resume"},
		{"--agent", "--session", "-p", "hello"},
	} {
		got, identityErr := nativeProxySessionID(kimiNativeProxy, args)
		if identityErr != nil {
			t.Fatal(identityErr)
		}
		if got != workspaceSessionB {
			t.Fatalf("required option value %q changed Kimi workspace identity to %q", args, got)
		}
	}
}

func TestAntigravityRoutedLaunchUsesCloudCodeOverride(t *testing.T) {
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	environ := []string{"HOME=" + t.TempDir(), "GEMINI_API_KEY=direct-secret", "CLOUD_CODE_URL=https://old.invalid"}
	got, cleanup, err := nativeProxyEnvironment(antigravityNativeProxy, "http://127.0.0.1:43212", environ, nil)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if value := testEnvValue(got, "CLOUD_CODE_URL"); value != "http://127.0.0.1:43212/antigravity" {
		t.Fatalf("CLOUD_CODE_URL = %q", value)
	}
	if testEnvValue(got, "GEMINI_API_KEY") != "" {
		t.Fatal("direct Gemini API key leaked into routed AGY environment")
	}
}

func TestRequireNativeProxyAccountChecksProviderAndAuthMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/accounts" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		_, _ = io.WriteString(w, `[
			{"id":"antigravity","provider":"antigravity","auth_mode":"oauth"},
			{"id":"qwen-token:work","provider":"qwen-token","auth_mode":"apikey"},
			{"id":"kimi-code","provider":"kimi","auth_mode":"oauth","source":"kimi-code credentials file"}
		]`)
	}))
	defer server.Close()
	runner := srRunner{client: server.Client()}
	config := srServerConfig{Name: "test", URL: server.URL}
	if err := runner.requireNativeProxyAccount(context.Background(), config, antigravityNativeProxy); err != nil {
		t.Fatal(err)
	}
	if err := runner.requireNativeProxyAccount(context.Background(), config, qwenNativeProxy); err != nil {
		t.Fatal(err)
	}
	wrong := kimiNativeProxy
	wrong.authMode = accounts.AuthModeOAuth
	if err := runner.requireNativeProxyAccount(context.Background(), config, wrong); err == nil || !strings.Contains(err.Error(), "no routed Kimi oauth account") {
		t.Fatalf("wrong-mode error = %v", err)
	}
}

// mergeQwenSettingsForTest matches Qwen Code 0.22's effective merge behavior
// for the object/array/scalar settings used by the routed overlay: objects are
// merged recursively and arrays or scalars replace the lower-precedence value.
func mergeQwenSettingsForTest(sources ...map[string]any) map[string]any {
	merged := make(map[string]any)
	var merge func(map[string]any, map[string]any)
	merge = func(target, source map[string]any) {
		for key, sourceValue := range source {
			sourceMap, sourceIsMap := sourceValue.(map[string]any)
			targetMap, targetIsMap := target[key].(map[string]any)
			if sourceIsMap && targetIsMap {
				merge(targetMap, sourceMap)
				continue
			}
			if sourceIsMap {
				targetMap = make(map[string]any)
				merge(targetMap, sourceMap)
				target[key] = targetMap
				continue
			}
			target[key] = sourceValue
		}
	}
	for _, source := range sources {
		merge(merged, source)
	}
	return merged
}

func loadQwenSettingsForTest(bare bool, sources ...map[string]any) map[string]any {
	if bare {
		return map[string]any{}
	}
	return mergeQwenSettingsForTest(sources...)
}

func testEnvValue(environ []string, key string) string {
	value, _ := testEnvEntry(environ, key)
	return value
}

func testEnvEntry(environ []string, key string) (string, bool) {
	prefix := key + "="
	for _, item := range environ {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix), true
		}
	}
	return "", false
}

func prepareKimiTestSessionHome(t *testing.T, home string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
