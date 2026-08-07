package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
)

func TestSRServerAddStoresGCPServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out}
	err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
		"--gcp-project", "example-project",
	})
	if err != nil {
		t.Fatal(err)
	}

	server, ok, err := defaultSRServerStore(store).find("community")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing configured server")
	}
	if server.URL != "http://100.64.0.1:31415" || server.GCPInstance != "subrouter-community" || server.GCPZone != "us-central1-a" || server.GCPProject != "example-project" {
		t.Fatalf("unexpected server config: %+v", server)
	}
}

func TestSRServerStoreUpdateSerializesConcurrentMutations(t *testing.T) {
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	const writers = 24
	start := make(chan struct{})
	errors := make(chan error, writers)
	var workers sync.WaitGroup
	for index := 0; index < writers; index++ {
		index := index
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			errors <- store.update(func(file *srServerFile) error {
				file.Servers = append(file.Servers, srServerConfig{
					Name: fmt.Sprintf("server-%02d", index),
					URL:  "https://subrouter.example.com",
				})
				return nil
			})
		}()
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}

	file, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Servers) != writers {
		t.Fatalf("servers = %d, want %d", len(file.Servers), writers)
	}
}

func TestSRServerStoreUpdateSerializesAcrossProcesses(t *testing.T) {
	if os.Getenv("SUBROUTER_SERVER_STORE_UPDATE_HELPER") == "1" {
		path := os.Getenv("SUBROUTER_SERVER_STORE_UPDATE_PATH")
		name := os.Getenv("SUBROUTER_SERVER_STORE_UPDATE_NAME")
		ready := os.Getenv("SUBROUTER_SERVER_STORE_UPDATE_READY")
		gate := os.Getenv("SUBROUTER_SERVER_STORE_UPDATE_GATE")
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for server store update gate")
			}
			time.Sleep(time.Millisecond)
		}
		if err := (srServerStore{Path: path}).update(func(file *srServerFile) error {
			file.Servers = append(file.Servers, srServerConfig{
				Name: name,
				URL:  "https://subrouter.example.com",
			})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "servers.json")
	gate := filepath.Join(dir, "start-updates")
	const processCount = 24
	commands := make([]*exec.Cmd, 0, processCount)
	for index := 0; index < processCount; index++ {
		name := fmt.Sprintf("server-%02d", index)
		ready := filepath.Join(dir, fmt.Sprintf("ready-%02d", index))
		command := exec.Command(os.Args[0], "-test.run=^TestSRServerStoreUpdateSerializesAcrossProcesses$")
		command.Env = append(os.Environ(),
			"SUBROUTER_SERVER_STORE_UPDATE_HELPER=1",
			"SUBROUTER_SERVER_STORE_UPDATE_PATH="+path,
			"SUBROUTER_SERVER_STORE_UPDATE_NAME="+name,
			"SUBROUTER_SERVER_STORE_UPDATE_READY="+ready,
			"SUBROUTER_SERVER_STORE_UPDATE_GATE="+gate,
		)
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		readyCount := 0
		for index := 0; index < processCount; index++ {
			if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("ready-%02d", index))); err == nil {
				readyCount++
			}
		}
		if readyCount == processCount {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d server store helpers became ready", readyCount, processCount)
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Errorf("server store helper failed: %v", err)
		}
	}
	if t.Failed() {
		return
	}
	file, err := (srServerStore{Path: path}).load()
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Servers) != processCount {
		t.Fatalf("servers = %d, want %d", len(file.Servers), processCount)
	}
}

func TestSRServerAddStoresAdminTokenForRemoteAdminEndpoints(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out}
	err := runner.run(context.Background(), []string{
		"server", "add", "team",
		"--url", "http://100.64.0.1:31415",
		"--admin-token", "secret-token",
	})
	if err != nil {
		t.Fatal(err)
	}

	server, ok, err := defaultSRServerStore(store).find("team")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing configured server")
	}
	if server.AdminToken != "secret-token" {
		t.Fatalf("admin token = %q", server.AdminToken)
	}
}

func TestSRServerAddStoresScopedAccountImportToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out}
	err := runner.run(context.Background(), []string{
		"server", "add", "team",
		"--url", "http://100.64.0.1:31415",
		"--account-import-token", "import-secret",
	})
	if err != nil {
		t.Fatal(err)
	}

	server, ok, err := defaultSRServerStore(store).find("team")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing configured server")
	}
	if server.AccountImportToken != "import-secret" {
		t.Fatalf("account import token = %q", server.AccountImportToken)
	}
}

func TestSRServerStatusSendsAdminToken(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	server := srServerConfig{
		Name:       "team",
		URL:        "http://100.64.0.1:31415",
		AdminToken: "secret-token",
	}
	if err := defaultSRServerStore(store).save(srServerFile{Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Authorization"); got != "Bearer secret-token" {
				t.Fatalf("Authorization = %q", got)
			}
			if req.URL.Path == "/_subrouter/bedrock-cost" {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(bytes.NewReader([]byte(`{"requests":0,"throttled":0}`))),
				}, nil
			}
			if req.URL.Path != "/_subrouter/usage-status" {
				t.Fatalf("path = %s, want /_subrouter/usage-status", req.URL.Path)
			}
			body, _ := json.Marshal([]remoteServerUsageStatus{{
				ID:                 "acct@example.com",
				Provider:           accounts.ProviderCodex,
				AuthMode:           accounts.AuthModeOAuth,
				Email:              "acct@example.com",
				AuthValid:          true,
				PlanType:           "pro",
				Windows:            []accounts.UsageWindow{{UsedPercent: 20, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)}},
				ComplimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Available: true},
			}})
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		})},
	}
	if err := runner.run(context.Background(), []string{"server", "status", "team"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "acct@example.com") {
		t.Fatalf("status did not render usage table:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "avail") {
		t.Fatalf("status did not render complimentary reset status:\n%s", out.String())
	}
}

func TestSRServerAddPreservesExistingAdminTokenWhenUpdatingMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{
		"server", "add", "team",
		"--url", "http://100.64.0.1:31415",
		"--admin-token", "secret-token",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.run(context.Background(), []string{
		"server", "add", "team",
		"--url", "http://100.64.0.2:31415",
		"--gcp-instance", "subrouter-team",
		"--gcp-zone", "us-south1-a",
	}); err != nil {
		t.Fatal(err)
	}

	server, ok, err := defaultSRServerStore(store).find("team")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing configured server")
	}
	if server.URL != "http://100.64.0.2:31415" {
		t.Fatalf("url = %q", server.URL)
	}
	if server.AdminToken != "secret-token" {
		t.Fatalf("admin token = %q", server.AdminToken)
	}
}

func TestSRServerAddAllowsURLOnlyServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{
		"server", "add", "team",
		"--url", "http://100.64.0.1:31415",
		"--default",
	}); err != nil {
		t.Fatal(err)
	}

	file, err := defaultSRServerStore(store).load()
	if err != nil {
		t.Fatal(err)
	}
	if file.Default != "team" {
		t.Fatalf("default = %q, want team", file.Default)
	}
	server, ok := file.find("team")
	if !ok {
		t.Fatal("missing team server")
	}
	if server.GCPInstance != "" || server.GCPZone != "" {
		t.Fatalf("unexpected GCP metadata: %+v", server)
	}
}

func TestSRServerUseSetsExplicitDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.run(context.Background(), []string{"server", "use", "community"}); err != nil {
		t.Fatal(err)
	}

	file, err := defaultSRServerStore(store).load()
	if err != nil {
		t.Fatal(err)
	}
	if file.Default != "community" {
		t.Fatalf("default = %q, want community", file.Default)
	}
	configBody, err := os.ReadFile(filepath.Join(home, "codex-home", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`openai_base_url = "http://100.64.0.1:31415/v1"`,
		`chatgpt_base_url = "http://100.64.0.1:31415/backend-api"`,
		`experimental_realtime_ws_base_url = "http://100.64.0.1:31415/v1"`,
	} {
		if !strings.Contains(string(configBody), want) {
			t.Fatalf("missing %q in config:\n%s", want, string(configBody))
		}
	}
	out.Reset()
	if err := runner.run(context.Background(), []string{"server", "list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "(default)") {
		t.Fatalf("list did not mark default:\n%s", out.String())
	}
}

func TestSRServerUseLocalClearsDefaultAndWritesLocalCodexConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))
	store := accounts.DefaultCodexStore()
	serverStore := defaultSRServerStore(store)
	cloudConfigPath, err := broker.DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.SaveConfig(cloudConfigPath, broker.Config{
		BaseURL: "https://cmux.com", AccessToken: "access", RefreshToken: "refresh",
		TeamID: "team", CredentialSource: broker.CredentialSourceHosted,
		HostedURL: "https://sr.cmux.dev",
		TenantKey: "srt_0123456789abcdef0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if err := serverStore.save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{
			Name: "team",
			URL:  "http://100.64.0.1:31415",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{program: "sr", store: store, out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"server", "use", "local"}); err != nil {
		t.Fatal(err)
	}
	file, err := serverStore.load()
	if err != nil {
		t.Fatal(err)
	}
	if file.Default != "" {
		t.Fatalf("default = %q, want local", file.Default)
	}
	configBody, err := os.ReadFile(filepath.Join(home, "codex-home", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configBody), `openai_base_url = "http://127.0.0.1:31415/v1"`) {
		t.Fatalf("local config not written:\n%s", string(configBody))
	}
	cloudConfig, err := broker.LoadConfig(cloudConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if cloudConfig.EffectiveCredentialSource() != broker.CredentialSourceLocal {
		t.Fatalf("credential source = %q, want local", cloudConfig.EffectiveCredentialSource())
	}
	if got := strings.Count(out.String(), "Credential storage: local"); got != 1 {
		t.Fatalf("local storage selected %d times:\n%s", got, out.String())
	}
}

func TestSRRemoteUseCMUXLocalSelectsSharedCredentialsWithLocalEgress(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))
	store := accounts.DefaultCodexStore()
	serverStore := defaultSRServerStore(store)
	cloudConfigPath, err := broker.DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	tenantKey := "srt_0123456789abcdef0123456789abcdef"
	if err := broker.SaveConfig(cloudConfigPath, broker.Config{
		BaseURL: "https://cmux.com", AccessToken: "access", RefreshToken: "refresh",
		TeamID: "team", TeamName: "Acme",
		CredentialSource: broker.CredentialSourceHosted,
		HostedURL:        "https://sr.cmux.dev",
		TenantKey:        tenantKey,
	}); err != nil {
		t.Fatal(err)
	}
	if err := serverStore.save(srServerFile{
		Default: "cmux",
		Servers: []srServerConfig{{
			Name: "cmux", URL: "https://sr.cmux.dev", TenantKey: tenantKey,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	runner := srRunner{
		program: "sr", store: store, out: &output, errOut: &output,
	}
	if err := runner.run(
		context.Background(),
		[]string{"remote", "use", "cmux-local"},
	); err != nil {
		t.Fatal(err)
	}
	config, err := broker.LoadConfig(cloudConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !config.TeamModeReady() {
		t.Fatalf("local-egress config is not ready: %#v", config)
	}
	file, err := serverStore.load()
	if err != nil {
		t.Fatal(err)
	}
	if file.Default != "" {
		t.Fatalf("server default = %q, want local daemon", file.Default)
	}
	codexConfig, err := os.ReadFile(
		filepath.Join(home, "codex-home", "config.toml"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		string(codexConfig),
		`openai_base_url = "http://127.0.0.1:31415/v1"`,
	) {
		t.Fatalf("Codex did not route through the local daemon:\n%s", codexConfig)
	}
	output.Reset()
	if err := runner.run(
		context.Background(),
		[]string{"remote", "current"},
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "cmux-local") {
		t.Fatalf("current remote = %q", output.String())
	}
	output.Reset()
	if err := runner.run(
		context.Background(),
		[]string{"remote", "list"},
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "cmux-local") ||
		!strings.Contains(output.String(), "cmux-local\thttp://127.0.0.1:31415\t(default)") {
		t.Fatalf("remote list = %q", output.String())
	}
}

func TestBuiltInCMUXRemoteUsesCanonicalProductionHostname(t *testing.T) {
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	var output bytes.Buffer
	runner := srRunner{
		program: "sr", store: accounts.CodexStore{Dir: t.TempDir()},
		out: &output, errOut: &output,
	}
	if err := runner.remoteList(defaultSRServerStore(runner.store)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "cmux\thttps://sr.cmux.com\t(login required)") {
		t.Fatalf("remote list = %q", output.String())
	}
	if strings.Contains(output.String(), "https://sr.cmux.dev") {
		t.Fatalf("deprecated hostname is still canonical: %q", output.String())
	}
}

func TestSRDefaultOutputUsesDefaultRemoteServerStatus(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{
			Name:       "team",
			URL:        "http://100.64.0.1:31415",
			AdminToken: "secret-token",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		program: "sr",
		store:   store,
		out:     &out,
		errOut:  &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			// status also queries bedrock-cost for the spend/rate-limit block;
			// return an empty summary so it stays silent here.
			if req.URL.Path == "/_subrouter/bedrock-cost" {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(bytes.NewReader([]byte(`{"requests":0,"throttled":0}`))),
				}, nil
			}
			if req.URL.Path != "/_subrouter/usage-status" {
				t.Fatalf("path = %s, want /_subrouter/usage-status", req.URL.Path)
			}
			body, _ := json.Marshal([]remoteServerUsageStatus{{
				ID:        "remote@example.com",
				Provider:  accounts.ProviderCodex,
				AuthMode:  accounts.AuthModeOAuth,
				Email:     "remote@example.com",
				AuthValid: true,
				PlanType:  "pro",
				Windows:   []accounts.UsageWindow{{UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)}},
			}})
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		})},
	}
	if err := runner.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Server: team") || !strings.Contains(out.String(), "remote@example.com") {
		t.Fatalf("default output did not render remote server status:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Switch to (#)") {
		t.Fatalf("remote status should not show local switch prompt:\n%s", out.String())
	}
}

func TestSRAddUsesDefaultRemoteServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != serverAccountImportPath {
			t.Errorf("unexpected path: %s", req.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if got := req.Header.Get("Authorization"); got != "Bearer import-secret" {
			t.Error("Authorization header did not match the expected protected import credential")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer remote.Close()
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{
			Name:       "team",
			URL:        remote.URL,
			AdminToken: "import-secret",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fake := &recordingSRCommandRunner{loginAuth: testCodexAuth("fresh@example.com", "acct_fresh")}
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake, client: remote.Client()}
	if err := runner.run(context.Background(), []string{"add", "--device-auth"}); err != nil {
		t.Fatal(err)
	}

	if !fake.hasCommand("codex", "login", "--device-auth") {
		t.Fatalf("missing remote login command: %#v", fake.commands)
	}
	for _, forbidden := range []string{"ssh", "scp", "gcloud"} {
		if fake.hasCommandPrefix(forbidden) {
			t.Fatalf("remote add must not execute %s: %#v", forbidden, fake.commands)
		}
	}
	if strings.Contains(out.String(), "Added account:") {
		t.Fatalf("top-level add should not add to local account store when a server is selected:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Uploaded fresh@example.com to server team.") {
		t.Fatalf("missing server add confirmation:\n%s", out.String())
	}
}

func TestSRAddUsesExplicitRemoteServerWhileTeamStorageIsActive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_CODEX_SERVER", "gcp-staging")
	cloudConfigPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudConfigPath)
	if err := os.WriteFile(cloudConfigPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var methods []string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		methods = append(methods, req.Method)
		if req.URL.Path != serverAccountImportPath {
			http.NotFound(w, req)
			return
		}
		if got := req.Header.Get("Authorization"); got != "Bearer import-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer remote.Close()

	store := accounts.DefaultCodexStore()
	if err := defaultSRServerStore(store).save(srServerFile{
		Servers: []srServerConfig{{
			Name:               "gcp-staging",
			URL:                remote.URL,
			AccountImportToken: "import-secret",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fake := &recordingSRCommandRunner{loginAuth: testCodexAuth("fresh@example.com", "acct_fresh")}
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake, client: remote.Client()}
	if err := runner.run(context.Background(), []string{"add", "--device-auth"}); err != nil {
		t.Fatal(err)
	}

	if !fake.hasCommand("codex", "login", "--device-auth") {
		t.Fatalf("missing remote login command: %#v", fake.commands)
	}
	if got, want := strings.Join(methods, ","), "GET,POST"; got != want {
		t.Fatalf("account-import methods = %q, want %q", got, want)
	}
	for _, forbidden := range []string{"ssh", "scp", "gcloud"} {
		if fake.hasCommandPrefix(forbidden) {
			t.Fatalf("remote add must not execute %s: %#v", forbidden, fake.commands)
		}
	}
	if !strings.Contains(out.String(), "Uploaded fresh@example.com to server gcp-staging.") {
		t.Fatalf("missing server add confirmation:\n%s", out.String())
	}
}

func TestSRListUsesDefaultRemoteServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "local@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    testCodexAuth("local@example.com", "acct_local"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{
			Name:       "team",
			URL:        "http://100.64.0.1:31415",
			AdminToken: "secret-token",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		program: "sr",
		store:   store,
		out:     &out,
		errOut:  &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/_subrouter/accounts" {
				t.Fatalf("path = %s, want /_subrouter/accounts", req.URL.Path)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer secret-token" {
				t.Fatalf("Authorization = %q", got)
			}
			body, _ := json.Marshal([]remoteServerAccount{{
				ID:       "remote@example.com",
				Provider: accounts.ProviderCodex,
				AuthMode: accounts.AuthModeOAuth,
				Email:    "remote@example.com",
			}})
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		})},
	}
	if err := runner.run(context.Background(), []string{"list"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "Server: team") || !strings.Contains(got, "remote@example.com") {
		t.Fatalf("list did not render remote server accounts:\n%s", got)
	}
	if strings.Contains(got, "local@example.com") {
		t.Fatalf("list read local accounts despite selected remote server:\n%s", got)
	}
}

func TestSRPickFailsWhenDefaultRemoteServerLacksUsageStatus(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{
			Name: "team",
			URL:  "http://100.64.0.1:31415",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	runner := srRunner{
		program: "sr",
		store:   store,
		out:     io.Discard,
		errOut:  io.Discard,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/_subrouter/usage-status" {
				t.Fatalf("unexpected remote request to %s", req.URL)
			}
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Status:     "404 Not Found",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("not found")),
			}, nil
		})},
	}

	err := runner.run(context.Background(), []string{"pick"})
	if err == nil || !strings.Contains(err.Error(), "does not support remote pick") {
		t.Fatalf("err = %v, want unsupported remote pick error", err)
	}
}

func TestSRServerEnvLocalKeepsCommandsLocal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_CODEX_SERVER", "local")
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "local@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    testCodexAuth("local@example.com", "acct_local"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{
			Name: "team",
			URL:  "http://100.64.0.1:31415",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		program: "sr",
		store:   store,
		out:     &out,
		errOut:  &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected remote request to %s", req.URL)
			return nil, nil
		})},
	}
	if err := runner.run(context.Background(), []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "local@example.com") {
		t.Fatalf("local override did not use local account store:\n%s", out.String())
	}
}

func TestSRSwitchDoesNotMutateLocalWhenDefaultRemoteServerSelected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	localAuth := testCodexAuth("local@example.com", "acct_local")
	if err := accounts.WriteActiveCodexAuth(localAuth); err != nil {
		t.Fatal(err)
	}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{
			Name: "team",
			URL:  "http://100.64.0.1:31415",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{program: "sr", store: store, out: &out, errOut: &out}
	err := runner.run(context.Background(), []string{"switch", "remote@example.com"})
	if err == nil || !strings.Contains(err.Error(), "will not edit local Codex state") {
		t.Fatalf("err = %v, want remote guard", err)
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || active.Tokens.RefreshToken != localAuth.Tokens.RefreshToken {
		t.Fatalf("active local auth was mutated")
	}
}

func TestUsageRowsFromServerUsageStatusesKeepsServerErrorWithoutEmail(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		Provider:    accounts.ProviderCodex,
		AuthChecked: true,
		AuthValid:   false,
		Error:       "read account store: permission denied",
	}})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one server error row", rows)
	}
	if rows[0].email != "server" || rows[0].err == nil || rows[0].err.Error() != "read account store: permission denied" {
		t.Fatalf("row = %+v", rows[0])
	}
}

func TestUsageRowsFromServerUsageStatusesPreservesComplimentaryReset(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		ID:                 "acct@example.com",
		Email:              "acct@example.com",
		Provider:           accounts.ProviderCodex,
		AuthMode:           accounts.AuthModeOAuth,
		PlanType:           "pro",
		Windows:            []accounts.UsageWindow{{UsedPercent: 20, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)}},
		ComplimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Consumed: true},
	}})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one row", rows)
	}
	if rows[0].complimentaryReset == nil || !rows[0].complimentaryReset.Consumed {
		t.Fatalf("complimentary reset not preserved: %+v", rows[0].complimentaryReset)
	}
}

func TestSRServerRenameUpdatesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := srRunner{program: "sr", store: store, out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--default",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.run(context.Background(), []string{"server", "rename", "community", "team"}); err != nil {
		t.Fatal(err)
	}

	file, err := defaultSRServerStore(store).load()
	if err != nil {
		t.Fatal(err)
	}
	if file.Default != "team" {
		t.Fatalf("default = %q, want team", file.Default)
	}
	if _, ok := file.find("community"); ok {
		t.Fatal("old server name still exists")
	}
	if _, ok := file.find("team"); !ok {
		t.Fatal("new server name missing")
	}
}

func TestSRServerLoginUploadsFreshAuthAndRestoresLocalChain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	oldLocal := testCodexAuth("bob@example.com", "acct_old_local")
	freshServer := testCodexAuth("bob@example.com", "acct_fresh_server")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "bob@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    oldLocal,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(oldLocal); err != nil {
		t.Fatal(err)
	}

	var stateMu sync.Mutex
	var imported accounts.StoredCodexAccount
	var preflightRequests, importRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/_subrouter/account-import" {
			t.Errorf("path = %q, want account import endpoint", req.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if got := req.Header.Get("Authorization"); got != "Bearer scoped-import-secret" {
			t.Error("Authorization header did not match the expected protected import credential")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch req.Method {
		case http.MethodGet:
			stateMu.Lock()
			preflightRequests++
			stateMu.Unlock()
		case http.MethodPost:
			var payload struct {
				Provider string                       `json:"provider"`
				Codex    *accounts.StoredCodexAccount `json:"codex"`
			}
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Error(err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if payload.Provider != "codex" || payload.Codex == nil {
				t.Errorf("unexpected import payload: provider=%q codex=%v", payload.Provider, payload.Codex != nil)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			stateMu.Lock()
			importRequests++
			imported = *payload.Codex
			stateMu.Unlock()
		default:
			t.Errorf("method = %s, want GET preflight or POST import", req.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	var out bytes.Buffer
	fake := &recordingSRCommandRunner{loginAuth: freshServer}
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake, client: server.Client()}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", server.URL,
		"--admin-token", "admin-secret",
		"--account-import-token", "scoped-import-secret",
	}); err != nil {
		t.Fatal(err)
	}

	if err := runner.run(context.Background(), []string{"server", "login", "community", "--device-auth"}); err != nil {
		t.Fatal(err)
	}

	stored, ok, err := store.FindStored("bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("local account should have been restored")
	}
	if stored.Auth.Tokens.RefreshToken != oldLocal.Tokens.RefreshToken {
		t.Fatalf("local refresh token = %q, want old chain", stored.Auth.Tokens.RefreshToken)
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || active.Tokens.RefreshToken != oldLocal.Tokens.RefreshToken {
		t.Fatalf("active auth was not restored")
	}
	if !fake.hasCommand("codex", "login", "--device-auth") {
		t.Fatalf("missing device-auth login command: %#v", fake.commands)
	}
	stateMu.Lock()
	gotPreflightRequests := preflightRequests
	gotImportRequests := importRequests
	gotImported := imported
	stateMu.Unlock()
	if gotPreflightRequests != 1 || gotImportRequests != 1 {
		t.Fatalf("account import requests = preflight:%d post:%d, want 1 each", gotPreflightRequests, gotImportRequests)
	}
	if gotImported.Email != "bob@example.com" || gotImported.Auth.Tokens == nil || gotImported.Auth.Tokens.RefreshToken != freshServer.Tokens.RefreshToken {
		t.Fatalf("server did not receive fresh OAuth account for bob@example.com")
	}
	for _, forbidden := range []string{"ssh", "scp", "gcloud"} {
		if fake.hasCommandPrefix(forbidden) {
			t.Fatalf("server login must never execute %s: %#v", forbidden, fake.commands)
		}
	}
	if !strings.Contains(out.String(), "Local Codex auth was left unchanged.") {
		t.Fatalf("missing ownership message:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "The new bob@example.com refresh token is stored on community, not kept as your local active login.") {
		t.Fatalf("missing server token ownership message:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Uploading bob@example.com to server community...") {
		t.Fatalf("missing upload progress message:\n%s", out.String())
	}
	if !fake.hasEnvPrefix("CODEX_HOME=") {
		t.Fatalf("server login should isolate Codex auth via CODEX_HOME: %#v", fake.envs)
	}
}

func TestSRServerLoginRejectsUnexpectedEmailWithoutUpload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	oldLocal := testCodexAuth("alice@example.com", "acct_old_local")
	wrongLogin := testCodexAuth("wrong@example.com", "acct_wrong")
	if err := accounts.WriteActiveCodexAuth(oldLocal); err != nil {
		t.Fatal(err)
	}
	var postCountMu sync.Mutex
	postCount := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			postCountMu.Lock()
			postCount++
			postCountMu.Unlock()
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer remote.Close()

	var out bytes.Buffer
	fake := &recordingSRCommandRunner{loginAuth: wrongLogin}
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake, client: remote.Client()}
	server := srServerConfig{
		Name:       "community",
		URL:        remote.URL,
		AdminToken: "import-secret",
	}

	err := runner.serverLoginOne(context.Background(), server, true, "alice@example.com", false)
	if err == nil || !strings.Contains(err.Error(), "expected alice@example.com") {
		t.Fatalf("error = %v", err)
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || active.Tokens.RefreshToken != oldLocal.Tokens.RefreshToken {
		t.Fatalf("active auth was not restored")
	}
	postCountMu.Lock()
	gotPostCount := postCount
	postCountMu.Unlock()
	if gotPostCount != 0 {
		t.Fatalf("wrong OAuth identity triggered %d account import POST(s)", gotPostCount)
	}
	for _, forbidden := range []string{"ssh", "scp", "gcloud"} {
		if fake.hasCommandPrefix(forbidden) {
			t.Fatalf("wrong login must not execute %s: %#v", forbidden, fake.commands)
		}
	}
}

func TestSRServerSyncUploadsMissingLocalOAuthOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	active := testCodexAuth("active@example.com", "acct_active")
	alice := testCodexAuth("alice@example.com", "acct_alice")
	bob := testCodexAuth("bob@example.com", "acct_bob")
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		email string
		auth  accounts.CodexAuthFile
	}{
		{"alice@example.com", alice},
		{"bob@example.com", bob},
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   item.email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    item.auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.AddAPIKey("paid", "sk-test-paid"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fake := &recordingSRCommandRunner{loginAuths: []accounts.CodexAuthFile{alice}}
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		cmd:    fake,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == serverAccountImportPath {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				}, nil
			}
			if req.URL.Path != "/_subrouter/account-status" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			if req.Method != http.MethodGet {
				t.Fatalf("method = %s, want GET", req.Method)
			}
			body, _ := json.Marshal([]remoteServerAccountStatus{
				{ID: "bob@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Email: "bob@example.com", AuthChecked: true, AuthValid: true},
				{ID: "old@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Email: "old@example.com", AuthChecked: true, AuthValid: true},
				{ID: "apikey:paid", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeAPIKey, Email: "apikey:paid"},
			})
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		})},
	}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
		"--gcp-project", "example-project",
		"--admin-token", "import-secret",
	}); err != nil {
		t.Fatal(err)
	}

	if err := runner.run(context.Background(), []string{"server", "sync", "community", "--device-auth", "--yes"}); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	for _, want := range []string{
		"Missing on server:\n  alice@example.com",
		"Already on server:\n  bob@example.com",
		"Invalid on server: none",
		"Server-only OAuth accounts:\n  old@example.com",
		"Synced 1 server-owned OAuth account(s) to community",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	if fake.countCommand("codex", "login", "--device-auth") != 1 {
		t.Fatalf("login command count mismatch: %#v", fake.commands)
	}
	for _, forbidden := range []string{"ssh", "scp", "gcloud"} {
		if fake.hasCommandPrefix(forbidden) {
			t.Fatalf("server sync must not execute %s: %#v", forbidden, fake.commands)
		}
	}
	restored, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || restored.Tokens.RefreshToken != active.Tokens.RefreshToken {
		t.Fatalf("active auth was not restored")
	}
}

func TestSRServerSyncURLOnlyServerFailsBeforeOAuthWithoutProtectedImportCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	active := testCodexAuth("active@example.com", "acct_active")
	fresh := testCodexAuth("alice@example.com", "acct_alice_fresh")
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    testCodexAuth("alice@example.com", "acct_alice"),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fake := &recordingSRCommandRunner{loginAuth: fresh}
	runner := srRunner{
		store:  store,
		out:    &out,
		errOut: &out,
		cmd:    fake,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			// A token-protected server answers the preflight with 401; the
			// client must stop there rather than starting an OAuth login it
			// would have to discard.
			if strings.HasSuffix(req.URL.Path, serverAccountImportPath) {
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("protected account import credential required")),
				}, nil
			}
			body, _ := json.Marshal([]remoteServerAccountStatus{})
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		})},
	}
	if err := runner.run(context.Background(), []string{
		"server", "add", "team",
		"--url", "http://100.64.0.1:31415",
	}); err != nil {
		t.Fatal(err)
	}

	err := runner.run(context.Background(), []string{"server", "sync", "team", "--yes"})
	if err == nil || !strings.Contains(err.Error(), "no protected HTTP account-import credential") {
		t.Fatalf("error = %v, want protected import credential failure", err)
	}
	if fake.hasCommandPrefix("codex", "login") {
		t.Fatalf("OAuth started before account-import preflight: %#v", fake.commands)
	}
	for _, forbidden := range []string{"ssh", "scp", "gcloud"} {
		if fake.hasCommandPrefix(forbidden) {
			t.Fatalf("URL-only server must not execute %s: %#v", forbidden, fake.commands)
		}
	}
	restored, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || restored.Tokens.RefreshToken != active.Tokens.RefreshToken {
		t.Fatalf("active auth was not restored")
	}
}

func TestSRServerSyncDryRunDoesNotLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    testCodexAuth("alice@example.com", "acct_alice"),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fake := &recordingSRCommandRunner{}
	runner := srRunner{
		store:  store,
		out:    &out,
		errOut: &out,
		cmd:    fake,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == serverAccountImportPath {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				}, nil
			}
			if req.URL.Path != "/_subrouter/account-status" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`[]`)),
			}, nil
		})},
	}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
		"--admin-token", "import-secret",
	}); err != nil {
		t.Fatal(err)
	}

	if err := runner.run(context.Background(), []string{"server", "sync", "community", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if fake.hasCommandPrefix("codex") || fake.hasCommandPrefix("gcloud") {
		t.Fatalf("dry-run ran commands: %#v", fake.commands)
	}
	if !strings.Contains(out.String(), "Would reauth on server:\n  alice@example.com") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
}

func TestSRServerSyncPromptsForInvalidServerAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FORCE_COLOR", "1")
	store := accounts.DefaultCodexStore()
	active := testCodexAuth("active@example.com", "acct_active")
	invalidFresh := testCodexAuth("old@example.com", "acct_old_fresh")
	if err := accounts.WriteActiveCodexAuth(active); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fake := &recordingSRCommandRunner{loginAuths: []accounts.CodexAuthFile{invalidFresh}}
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("yes\n"),
		out:    &out,
		errOut: &out,
		cmd:    fake,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == serverAccountImportPath {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				}, nil
			}
			if req.URL.Path != "/_subrouter/account-status" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			body, _ := json.Marshal([]remoteServerAccountStatus{
				{ID: "old@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Email: "old@example.com", AuthChecked: true, AuthValid: false, Error: "token refresh failed (401): refresh_token_reused"},
			})
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		})},
	}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
		"--admin-token", "import-secret",
	}); err != nil {
		t.Fatal(err)
	}

	if err := runner.run(context.Background(), []string{"server", "sync", "community", "--device-auth"}); err != nil {
		t.Fatal(err)
	}
	output := out.String()
	for _, want := range []string{
		"Invalid on server:\n  old@example.com: token refresh failed",
		"Reauth 1 account(s) on server community?",
		"Sign in as " + ansiBold + ansiMagenta + "old@example.com" + ansiReset + " for server community.",
		"Synced 1 server-owned OAuth account(s) to community",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	if fake.countCommand("codex", "login", "--device-auth") != 1 {
		t.Fatalf("login command count mismatch: %#v", fake.commands)
	}
}

func TestSRServerInstallUsesPublicInstallerAndSystemdCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TAILSCALE_AUTH_KEY", "tailscale-auth-test-secret")
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	fake := &recordingSRCommandRunner{}
	runner := srRunner{store: store, out: &out, errOut: &out, cmd: fake}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
		"--gcp-project", "example-project",
	}); err != nil {
		t.Fatal(err)
	}

	if err := runner.run(context.Background(), []string{"server", "install", "community", "--version", "0.1.2"}); err != nil {
		t.Fatal(err)
	}

	if !fake.hasCommandPrefix("gcloud", "compute", "ssh", "subrouter-community") {
		t.Fatalf("missing gcloud ssh install command: %#v", fake.commands)
	}
	if !strings.Contains(strings.Join(fake.commands[len(fake.commands)-1], " "), "--tunnel-through-iap") {
		t.Fatalf("gcloud install did not require IAP: %#v", fake.commands)
	}
	joined := strings.Join(fake.commands[0], " ")
	if strings.Contains(joined, "tailscale-auth-test-secret") {
		t.Fatalf("tailscale auth key leaked into command: %s", joined)
	}
	installCommand := strings.Join(fake.commands[len(fake.commands)-1], " ")
	for _, want := range []string{
		publicInstallScriptURL,
		"SUBROUTER_VERSION='0.1.2'",
		"/usr/local/bin/sr install-systemd",
		"until curl -fsS http://127.0.0.1:31415/_subrouter/health",
		">/dev/null 2>&1",
		"--admin-token-stdin",
		"--account-import-token-stdin",
	} {
		if !strings.Contains(installCommand, want) {
			t.Fatalf("install command missing %q:\n%s", want, installCommand)
		}
	}
	for _, forbidden := range []string{
		"tailscale", "tailscale_auth_key", "--accept-routes", "--accept-dns",
	} {
		if strings.Contains(installCommand, forbidden) {
			t.Fatalf("install command still depends on %q:\n%s", forbidden, installCommand)
		}
	}
	if !strings.Contains(out.String(), "Installed Subrouter server: community") {
		t.Fatalf("missing install message:\n%s", out.String())
	}
	server, ok, err := defaultSRServerStore(store).find("community")
	if err != nil || !ok {
		t.Fatalf("installed server config = found:%v err:%v", ok, err)
	}
	if len(server.AdminToken) < 40 {
		t.Fatalf("server install did not provision a strong remote control token")
	}
	if len(server.AccountImportToken) < 40 || server.AccountImportToken == server.AdminToken {
		t.Fatal("server install did not provision a distinct strong account import token")
	}
	if strings.Contains(out.String(), server.AdminToken) {
		t.Fatal("server install printed its remote control token")
	}
	if strings.Contains(installCommand, server.AdminToken) {
		t.Fatal("server install exposed its remote control token in process arguments")
	}
	if strings.Contains(out.String(), server.AccountImportToken) || strings.Contains(installCommand, server.AccountImportToken) {
		t.Fatal("server install exposed its account import token")
	}
}

func TestSRServerLoginPreflightFailureDoesNotStartOAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	serverStore := defaultSRServerStore(store)
	if err := serverStore.save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{
			Name:       "team",
			URL:        "http://100.64.0.20:31415",
			AdminToken: "import-secret",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	fake := &recordingSRCommandRunner{loginAuth: testCodexAuth("fresh@example.com", "fresh-refresh")}
	var out bytes.Buffer
	runner := srRunner{
		program: "sr",
		store:   store,
		in:      strings.NewReader(""),
		out:     &out,
		errOut:  &out,
		cmd:     fake,
		client: &http.Client{Transport: srRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Status:     "503 Service Unavailable",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("unavailable")),
			}, nil
		})},
	}

	err := runner.run(context.Background(), []string{"add"})
	if err == nil || !strings.Contains(err.Error(), "account-import preflight failed") {
		t.Fatalf("error = %v, want preflight failure", err)
	}
	if fake.hasCommandPrefix("codex", "login") {
		t.Fatalf("OAuth started despite failed account-import preflight: %#v", fake.commands)
	}
	for _, forbidden := range []string{"ssh", "scp", "gcloud"} {
		if fake.hasCommandPrefix(forbidden) {
			t.Fatalf("login preflight must not execute %s: %#v", forbidden, fake.commands)
		}
	}
}

type recordingSRCommandRunner struct {
	mu                  sync.Mutex
	loginAuth           accounts.CodexAuthFile
	loginAuths          []accounts.CodexAuthFile
	loginDelay          time.Duration
	onLogin             func(env []string)
	commands            [][]string
	envs                [][]string
	failCommandPrefixes []failCommandPrefix
}

func (r *recordingSRCommandRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return r.RunWithEnv(ctx, name, args, nil, stdin, stdout, stderr)
}

func (r *recordingSRCommandRunner) RunWithEnv(_ context.Context, name string, args []string, env []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	r.mu.Lock()
	command := append([]string{name}, args...)
	r.commands = append(r.commands, command)
	r.envs = append(r.envs, append([]string(nil), env...))
	for i := range r.failCommandPrefixes {
		failure := &r.failCommandPrefixes[i]
		if failure.times > 0 && commandHasPrefix(command, failure.prefix) {
			failure.times--
			err := failure.err
			r.mu.Unlock()
			return err
		}
	}
	loginAuth := r.loginAuth
	if name == "codex" && len(args) > 0 && args[0] == "login" {
		if len(r.loginAuths) > 0 {
			loginAuth = r.loginAuths[0]
			r.loginAuths = r.loginAuths[1:]
		}
		onLogin := r.onLogin
		delay := r.loginDelay
		r.mu.Unlock()
		if onLogin != nil {
			onLogin(env)
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		body, err := jsonMarshalIndent(loginAuth)
		if err != nil {
			return err
		}
		path := authPathFromEnv(env)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return os.WriteFile(path, body, 0o600)
	}
	r.mu.Unlock()
	return nil
}

func authPathFromEnv(env []string) string {
	for _, item := range env {
		if strings.HasPrefix(item, "CODEX_HOME=") {
			return filepath.Join(strings.TrimPrefix(item, "CODEX_HOME="), "auth.json")
		}
	}
	return accounts.DefaultCodexAuthPath()
}

type failCommandPrefix struct {
	prefix []string
	times  int
	err    error
}

func (r *recordingSRCommandRunner) Output(context.Context, string, []string) ([]byte, error) {
	return nil, nil
}

func (r *recordingSRCommandRunner) hasEnvPrefix(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, env := range r.envs {
		for _, item := range env {
			if strings.HasPrefix(item, prefix) {
				return true
			}
		}
	}
	return false
}

func (r *recordingSRCommandRunner) hasCommand(parts ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, command := range r.commands {
		if len(command) != len(parts) {
			continue
		}
		matches := true
		for i := range parts {
			if command[i] != parts[i] {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func (r *recordingSRCommandRunner) countCommand(parts ...string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, command := range r.commands {
		if len(command) != len(parts) {
			continue
		}
		matches := true
		for i := range parts {
			if command[i] != parts[i] {
				matches = false
				break
			}
		}
		if matches {
			count++
		}
	}
	return count
}

func (r *recordingSRCommandRunner) hasCommandPrefix(parts ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, command := range r.commands {
		if commandHasPrefix(command, parts) {
			return true
		}
	}
	return false
}

func (r *recordingSRCommandRunner) countCommandPrefix(parts ...string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, command := range r.commands {
		if commandHasPrefix(command, parts) {
			count++
		}
	}
	return count
}

func commandHasPrefix(command []string, parts []string) bool {
	if len(command) < len(parts) {
		return false
	}
	for i := range parts {
		if command[i] != parts[i] {
			return false
		}
	}
	return true
}

func TestParallelServerLoginSerializesAndPreservesLocalAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	local := testCodexAuth("local@example.com", "acct_local")
	if err := accounts.WriteActiveCodexAuth(local); err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer remote.Close()
	server := srServerConfig{Name: "team", URL: remote.URL, AdminToken: "import-secret"}

	started := make(chan struct{}, 2)
	releaseFirst := make(chan struct{})
	var firstStarted sync.Once
	fakeA := &recordingSRCommandRunner{
		loginAuth:  testCodexAuth("lc+4@cmux.com", "acct_lc4"),
		loginDelay: 80 * time.Millisecond,
		onLogin: func([]string) {
			firstStarted.Do(func() { close(releaseFirst) })
			started <- struct{}{}
		},
	}
	fakeB := &recordingSRCommandRunner{
		loginAuth: testCodexAuth("lc+5@cmux.com", "acct_lc5"),
		onLogin: func([]string) {
			<-releaseFirst
			started <- struct{}{}
		},
	}

	var outA, outB bytes.Buffer
	errA := make(chan error, 1)
	errB := make(chan error, 1)
	go func() {
		runner := srRunner{store: store, out: &outA, errOut: &outA, cmd: fakeA, client: remote.Client()}
		errA <- runner.serverLoginOne(context.Background(), server, true, "", false)
	}()
	go func() {
		// Ensure B contends for the lock while A holds it during login.
		time.Sleep(20 * time.Millisecond)
		runner := srRunner{store: store, out: &outB, errOut: &outB, cmd: fakeB, client: remote.Client()}
		errB <- runner.serverLoginOne(context.Background(), server, true, "", false)
	}()

	if err := <-errA; err != nil {
		t.Fatalf("login A: %v\n%s", err, outA.String())
	}
	if err := <-errB; err != nil {
		t.Fatalf("login B: %v\n%s", err, outB.String())
	}
	close(started)

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || active.Tokens.RefreshToken != local.Tokens.RefreshToken {
		t.Fatalf("local active auth was mutated by parallel server logins")
	}
	combined := outA.String() + outB.String()
	if !strings.Contains(combined, "Another sr add/login is in progress; waiting...") {
		t.Fatalf("expected waiter message for concurrent login:\n%s", combined)
	}
	if !strings.Contains(combined, "Uploaded lc+4@cmux.com to server team.") {
		t.Fatalf("missing lc+4 upload:\n%s", combined)
	}
	if !strings.Contains(combined, "Uploaded lc+5@cmux.com to server team.") {
		t.Fatalf("missing lc+5 upload:\n%s", combined)
	}
	if !strings.Contains(combined, "Uploading lc+4@cmux.com to server team...") ||
		!strings.Contains(combined, "Uploading lc+5@cmux.com to server team...") {
		t.Fatalf("missing upload progress indicators:\n%s", combined)
	}
}

func TestParseAPIKeyProviderClaude(t *testing.T) {
	provider, err := parseAPIKeyProvider("claude")
	if err != nil {
		t.Fatal(err)
	}
	if provider != accounts.ProviderClaude {
		t.Fatalf("provider = %q, want claude", provider)
	}
}

func jsonMarshalIndent(value any) ([]byte, error) {
	return json.MarshalIndent(value, "", "  ")
}
