package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
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
	fake := &recordingSRCommandRunner{loginAuth: testCodexAuth("fresh@example.com", "acct_fresh")}
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	if err := runner.run(context.Background(), []string{"add", "--device-auth"}); err != nil {
		t.Fatal(err)
	}

	if !fake.hasCommand("codex", "login", "--device-auth") {
		t.Fatalf("missing remote login command: %#v", fake.commands)
	}
	if !fake.hasCommandPrefix("ssh", "-o", "BatchMode=yes") {
		t.Fatalf("missing direct server upload command: %#v", fake.commands)
	}
	if strings.Contains(out.String(), "Added account:") {
		t.Fatalf("top-level add should not add to local account store when a server is selected:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Uploaded fresh@example.com to server team.") {
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

	var out bytes.Buffer
	fake := &recordingSRCommandRunner{loginAuth: freshServer}
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
		"--gcp-project", "example-project",
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
	if !fake.hasCommandPrefix("ssh", "-o", "BatchMode=yes") {
		t.Fatalf("missing direct ssh upload/install command: %#v", fake.commands)
	}
	if fake.hasCommandPrefix("gcloud", "compute", "scp") {
		t.Fatalf("unexpected gcloud scp for tailnet server: %#v", fake.commands)
	}
	uploadCommand := strings.Join(fake.commands[len(fake.commands)-1], " ")
	if strings.Contains(uploadCommand, "systemctl restart subrouter") {
		t.Fatalf("upload should hot-reload instead of restarting:\n%s", uploadCommand)
	}
	if !strings.Contains(uploadCommand, "reload_status=$(curl") {
		t.Fatalf("upload should preflight hot-reload support before writing files:\n%s", uploadCommand)
	}
	if !strings.Contains(uploadCommand, "POST http://127.0.0.1:31415/_subrouter/reload-accounts") {
		t.Fatalf("upload should hot-reload accounts:\n%s", uploadCommand)
	}
	if !strings.Contains(uploadCommand, "/var/lib/subrouter/codex/accounts") {
		t.Fatalf("upload should install accounts into subrouter state dir:\n%s", uploadCommand)
	}
	if !strings.Contains(uploadCommand, `sr_owner=$(stat -f '%Su' /var/lib/subrouter`) {
		t.Fatalf("upload should detect state-dir owner for macOS _subrouter installs:\n%s", uploadCommand)
	}
	if !strings.Contains(uploadCommand, `sudo install -d -o "$sr_owner" -g "$sr_group"`) {
		t.Fatalf("upload should chown via detected owner/group, not hardcode subrouter:\n%s", uploadCommand)
	}
	if strings.Contains(uploadCommand, "install -d -o subrouter -g subrouter") {
		t.Fatalf("upload should not hardcode Linux subrouter group on macOS servers:\n%s", uploadCommand)
	}
	if strings.Contains(uploadCommand, "/var/lib/subrouter/.codex-accounts") {
		t.Fatalf("upload should not use legacy account path:\n%s", uploadCommand)
	}
	if !strings.Contains(out.String(), "Your local Codex login is back to the account you were using before this command.") {
		t.Fatalf("missing ownership message:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "The new bob@example.com refresh token is stored on community, not kept as your local active login.") {
		t.Fatalf("missing server token ownership message:\n%s", out.String())
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

	var out bytes.Buffer
	fake := &recordingSRCommandRunner{loginAuth: wrongLogin}
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	server := srServerConfig{
		Name:        "community",
		URL:         "http://100.64.0.1:31415",
		GCPInstance: "subrouter-community",
		GCPZone:     "us-central1-a",
	}

	err := runner.serverLoginOne(context.Background(), server, true, "alice@example.com")
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
	if fake.hasCommandPrefix("gcloud") {
		t.Fatalf("unexpected upload command after wrong login: %#v", fake.commands)
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
	if !fake.hasCommandPrefix("ssh", "-o", "BatchMode=yes") {
		t.Fatalf("missing direct ssh upload/install command: %#v", fake.commands)
	}
	if fake.hasCommandPrefix("gcloud", "compute", "scp") {
		t.Fatalf("unexpected gcloud scp for tailnet server: %#v", fake.commands)
	}
	restored, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || restored.Tokens.RefreshToken != active.Tokens.RefreshToken {
		t.Fatalf("active auth was not restored")
	}
}

func TestSRServerSyncURLOnlyServerUsesDirectSSHUpload(t *testing.T) {
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

	if err := runner.run(context.Background(), []string{"server", "sync", "team", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if !fake.hasCommandPrefix("ssh", "-o", "BatchMode=yes") {
		t.Fatalf("missing direct ssh upload/install command: %#v", fake.commands)
	}
	if fake.hasCommandPrefix("gcloud") {
		t.Fatalf("URL-only server used gcloud: %#v", fake.commands)
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
		"tailscale up",
	} {
		if !strings.Contains(installCommand, want) {
			t.Fatalf("install command missing %q:\n%s", want, installCommand)
		}
	}
	if !strings.Contains(out.String(), "Installed Subrouter server: community") {
		t.Fatalf("missing install message:\n%s", out.String())
	}
}

func TestSRServerLoginRetriesTransientSSHUploadFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	serverStore := defaultSRServerStore(store)
	if err := serverStore.save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{
			Name: "team",
			URL:  "http://subrouter-team:31415",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	fake := &recordingSRCommandRunner{
		loginAuth: testCodexAuth("fresh@example.com", "fresh-refresh"),
		failCommandPrefixes: []failCommandPrefix{{
			prefix: []string{"ssh", "-o", "BatchMode=yes"},
			times:  1,
			err:    errors.New("ssh: connect to host subrouter-team port 22: Connection refused"),
		}},
	}
	var out bytes.Buffer
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}

	if err := runner.run(context.Background(), []string{"add"}); err != nil {
		t.Fatal(err)
	}

	if count := fake.countCommandPrefix("ssh", "-o", "BatchMode=yes"); count != 2 {
		t.Fatalf("ssh upload attempts = %d, want 2; commands: %#v", count, fake.commands)
	}
	if !strings.Contains(out.String(), "server ssh upload failed, retrying (1/3)") {
		t.Fatalf("missing retry message:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Uploaded fresh@example.com to server team.") {
		t.Fatalf("missing success message after retry:\n%s", out.String())
	}
}

type recordingSRCommandRunner struct {
	loginAuth           accounts.CodexAuthFile
	loginAuths          []accounts.CodexAuthFile
	commands            [][]string
	failCommandPrefixes []failCommandPrefix
}

func (r *recordingSRCommandRunner) Run(_ context.Context, name string, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	command := append([]string{name}, args...)
	r.commands = append(r.commands, command)
	for i := range r.failCommandPrefixes {
		failure := &r.failCommandPrefixes[i]
		if failure.times > 0 && commandHasPrefix(command, failure.prefix) {
			failure.times--
			return failure.err
		}
	}
	if name == "codex" && len(args) > 0 && args[0] == "login" {
		loginAuth := r.loginAuth
		if len(r.loginAuths) > 0 {
			loginAuth = r.loginAuths[0]
			r.loginAuths = r.loginAuths[1:]
		}
		body, err := jsonMarshalIndent(loginAuth)
		if err != nil {
			return err
		}
		path := accounts.DefaultCodexAuthPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return os.WriteFile(path, body, 0o600)
	}
	return nil
}

type failCommandPrefix struct {
	prefix []string
	times  int
	err    error
}

func (r *recordingSRCommandRunner) Output(context.Context, string, []string) ([]byte, error) {
	return nil, nil
}

func (r *recordingSRCommandRunner) hasCommand(parts ...string) bool {
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
	for _, command := range r.commands {
		if commandHasPrefix(command, parts) {
			return true
		}
	}
	return false
}

func (r *recordingSRCommandRunner) countCommandPrefix(parts ...string) int {
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
