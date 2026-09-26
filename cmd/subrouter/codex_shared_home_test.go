package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestCodexSharedDaemonEligibility(t *testing.T) {
	t.Setenv(codexSharedDaemonDisable, "")
	for _, tc := range []struct {
		name string
		args []string
		ok   bool
	}{
		{"bare", nil, true},
		{"prompt", []string{"fix the bug"}, true},
		{"resume", []string{"resume", "--last"}, true},
		{"fork", []string{"fork"}, true},
		{"cd flag", []string{"-C", "/tmp/x"}, true},
		{"exec", []string{"exec", "hi"}, false},
		{"app-server", []string{"app-server"}, false},
		{"model", []string{"-m", "gpt-5"}, false},
		{"config", []string{"-c", "x=1"}, false},
		{"attached config", []string{"-cx=1"}, false},
		{"profile", []string{"--profile=fast"}, false},
		{"enable", []string{"--enable", "foo"}, false},
		{"no-daemon", []string{"--no-daemon"}, false},
		{"flag after terminator", []string{"--", "-c", "x"}, true},
	} {
		if got := codexSharedDaemonEligible(tc.args, false, "", "", false, ""); got != tc.ok {
			t.Errorf("%s: eligible=%v, want %v", tc.name, got, tc.ok)
		}
	}
	if codexSharedDaemonEligible(nil, true, "", "", false, "") {
		t.Error("the built-in local relay needs a per-launch credential")
	}
	if codexSharedDaemonEligible(nil, false, "a@b.co", "", false, "") ||
		codexSharedDaemonEligible(nil, false, "", "team-codex-1", false, "") {
		t.Error("user and account pins are per-launch headers")
	}
	if codexSharedDaemonEligible(nil, false, "", "", true, "") ||
		codexSharedDaemonEligible(nil, false, "", "", false, "persist") {
		t.Error("capacity retry settings are per-launch")
	}
	t.Setenv(codexSharedDaemonDisable, "0")
	if codexSharedDaemonEligible(nil, false, "", "", false, "") {
		t.Error("SUBROUTER_CODEX_SHARED_DAEMON=0 must opt out")
	}
}

func TestCodexSharedHomeConfig(t *testing.T) {
	source := "/Users/u/.codex"
	target := "/Users/u/.subrouter/codex-home"
	sourceBody := `model = "gpt-6"
model_provider = "openai"
openai_base_url = "http://old/v1"

[model_providers.subrouter]
base_url = "http://stale/v1"

[model_providers.subrouter.http_headers]
X = "y"

[mcp_servers.docs]
url = "https://example.com/mcp"

[hooks.state."/Users/u/.codex/hooks.json:pre_tool_use:0:0"]
trusted_hash = "sha256:aa"

[projects."/Users/u/src"]
trust_level = "trusted"
`
	previous := `model_provider = "subrouter"

[projects."/Users/u/new"]
trust_level = "trusted"

[projects."/Users/u/src"]
trust_level = "untrusted"

[hooks.state."/Users/u/.subrouter/codex-home/hooks.json:stop:0:0"]
trusted_hash = "sha256:bb"

[mcp_servers.removed]
url = "https://gone"
`
	notify := []string{"/usr/local/bin/sr", "__session-notify", "--shared", "--store-dir", "/s"}
	got := codexSharedHomeConfig(sourceBody, previous, source, target, "http://127.0.0.1:31415/v1", notify)

	mustContain := []string{
		"model_provider = \"subrouter\"\n",
		`notify = ["/usr/local/bin/sr", "__session-notify", "--shared", "--store-dir", "/s"]`,
		`model = "gpt-6"`,
		"[mcp_servers.docs]",
		`[hooks.state."/Users/u/.subrouter/codex-home/hooks.json:pre_tool_use:0:0"]`,
		`[projects."/Users/u/src"]` + "\ntrust_level = \"trusted\"",
		`[projects."/Users/u/new"]`,
		`[hooks.state."/Users/u/.subrouter/codex-home/hooks.json:stop:0:0"]`,
		"[model_providers.subrouter]\nname = \"Subrouter\"\nbase_url = \"http://127.0.0.1:31415/v1\"\nexperimental_bearer_token = \"subrouter\"\n",
		`http_headers = {"X-Subrouter-Agent"="codex"}`,
	}
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{
		`model_provider = "openai"`, "openai_base_url", "http://stale/v1",
		"[model_providers.subrouter.http_headers]", `"/Users/u/.codex/hooks.json`,
		"[mcp_servers.removed]", `trust_level = "untrusted"`,
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("config kept %q:\n%s", unwanted, got)
		}
	}
	if strings.Count(got, `[projects."/Users/u/src"]`) != 1 || strings.Count(got, "[model_providers.subrouter]") != 1 {
		t.Errorf("duplicated table:\n%s", got)
	}
	if i, j := strings.Index(got, "model_provider = "), strings.Index(got, "["); i < 0 || i > j {
		t.Errorf("model_provider must be a top-level key before any table:\n%s", got)
	}
}

func TestDropTopLevelKeysSkipsMultiLineArrays(t *testing.T) {
	lines := splitTomlSections("a = 1\nnotify = [\n  \"x\",\n  \"y\",\n]\nb = 2\n")[0].lines
	got := strings.Join(dropTopLevelKeys(lines, func(key string) bool { return key == "notify" }), "")
	if got != "a = 1\nb = 2\n" {
		t.Fatalf("got %q", got)
	}
}

func TestCodexSharedHomeConfigKeepsUserNotify(t *testing.T) {
	got := codexSharedHomeConfig("notify = [\"mine\"]\n", "", "/a", "/b", "http://h/v1", []string{"sr", "__session-notify"})
	if !strings.Contains(got, `notify = ["mine"]`) || strings.Contains(got, "__session-notify") {
		t.Fatalf("the user's notify program must win:\n%s", got)
	}
}

func TestPrepareCodexSharedHomeLinksAndRefreshes(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "codex")
	target := filepath.Join(root, "shared")
	release := filepath.Join(source, "packages", "app-server-daemon", "releases", "1.0")
	if err := os.MkdirAll(filepath.Join(release, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "bin", "codex"), []byte("bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release, filepath.Join(source, "packages", "app-server-daemon", "current")); err != nil {
		t.Fatal(err)
	}
	// An older build linked packages; the owned entry must become this home's own.
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(source, "packages"), filepath.Join(target, "packages")); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"sessions", "skills", "app-server-daemon", "app-server-control"} {
		if err := os.MkdirAll(filepath.Join(source, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"auth.json": "{}", "config.toml": "model = \"m\"\n", "config.toml.bak": "old",
		"models_cache.json": "{}", "hooks.json": "{}",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareCodexSharedHome(source, target, "http://h/v1", nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sessions", "skills", "auth.json", "hooks.json"} {
		if dest, err := os.Readlink(filepath.Join(target, name)); err != nil || dest != filepath.Join(source, name) {
			t.Errorf("%s: link %q, %v", name, dest, err)
		}
	}
	for _, name := range []string{"app-server-daemon", "app-server-control", "models_cache.json", "config.toml.bak"} {
		if _, err := os.Lstat(filepath.Join(target, name)); !os.IsNotExist(err) {
			t.Errorf("%s must not be linked from the source home", name)
		}
	}
	if info, err := os.Lstat(filepath.Join(target, "packages")); err != nil || !info.IsDir() {
		t.Fatalf("packages must be a real directory of the shared home: %v %v", info, err)
	}
	resolvedRelease, _ := filepath.EvalSymlinks(release)
	if dest, err := filepath.EvalSymlinks(filepath.Join(target, "packages", "app-server-daemon", "current")); err != nil || dest != resolvedRelease {
		t.Fatalf("daemon package = %q %v, want the user's release %q", dest, err, resolvedRelease)
	}
	if dest, _ := os.Readlink(filepath.Join(source, "packages", "app-server-daemon", "current")); dest != release {
		t.Fatalf("the user's installation was modified: current -> %q", dest)
	}
	config := filepath.Join(target, "config.toml")
	info, err := os.Lstat(config)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("generated config: %v %v", info, err)
	}

	// A new source entry is linked, a vanished one unlinked, Codex's own
	// file kept, and a moved server rewrites the base URL.
	if err := os.WriteFile(filepath.Join(source, "rules"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(source, "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "state_5.sqlite"), []byte("own"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareCodexSharedHome(source, target, "http://moved/v1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(filepath.Join(target, "rules")); err != nil {
		t.Error("new source entry was not linked")
	}
	if _, err := os.Lstat(filepath.Join(target, "skills")); !os.IsNotExist(err) {
		t.Error("dangling link into the source was kept")
	}
	if body, _ := os.ReadFile(filepath.Join(target, "state_5.sqlite")); string(body) != "own" {
		t.Error("the shared home's own file was replaced")
	}
	if body, _ := os.ReadFile(config); !strings.Contains(string(body), `base_url = "http://moved/v1"`) {
		t.Errorf("base URL not refreshed:\n%s", body)
	}
}

func TestCodexBareLaunchUsesSharedHomeWithoutConfigOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(home, ".subrouter"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "user-codex"))
	t.Setenv(codexSharedDaemonDisable, "")
	if err := os.MkdirAll(filepath.Join(home, "user-codex", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := accounts.DefaultCodexStore()
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			http.NotFound(response, request)
			return
		}
		_, _ = fmt.Fprint(response, `{"status":"ok"}`)
	}))
	defer upstream.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", upstream.URL+"/v1")
	t.Setenv("SUBROUTER_CODEX_SERVER", "shadow")
	if err := defaultSRServerStore(store).save(srServerFile{
		Servers: []srServerConfig{{Name: "shadow", URL: upstream.URL}},
	}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "codex-fake")
	record := filepath.Join(home, "record")
	script := "#!/bin/sh\nprintf 'args:%s\\n' \"$*\" > " + shellQuote(record) + "\nprintf 'home:%s\\n' \"$CODEX_HOME\" >> " + shellQuote(record) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_CODEX_BIN", bin)

	if err := codex([]string{"fix", "it"}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(record)
	shared := filepath.Join(home, ".subrouter", codexSharedHomeDirName)
	if got := string(body); got != "args:fix it\nhome:"+shared+"\n" {
		t.Fatalf("shared launch = %q", got)
	}
	config, _ := os.ReadFile(filepath.Join(shared, "config.toml"))
	if !strings.Contains(string(config), `base_url = "`+upstream.URL+`/v1"`) {
		t.Fatalf("shared config lacks the resolved server:\n%s", config)
	}

	if err := codex([]string{"-m", "gpt-5", "fix"}); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(record)
	if got := string(body); !strings.Contains(got, `model_provider="subrouter"`) || !strings.Contains(got, "home:"+filepath.Join(home, "user-codex")+"\n") {
		t.Fatalf("a -m launch must keep the per-launch provider block in the user's home: %q", got)
	}
}

func TestFindSharedLaunchMatchesRunningLaunchByDirectory(t *testing.T) {
	ledger := newSessionLedger(t.TempDir())
	dir := t.TempDir()
	other := t.TempDir()
	base := time.Now()
	start := func(cwd string, shared bool, pid int, at time.Duration) sessionLaunchRecord {
		launch, err := ledger.startLaunch(sessionLaunchRecord{
			Agent: "codex", WorkingDir: cwd, Shared: shared, PID: pid, StartedAt: base.Add(at),
		})
		if err != nil {
			t.Fatal(err)
		}
		return launch
	}
	older := start(dir, true, os.Getpid(), 0)
	newer := start(dir, true, os.Getpid(), time.Second)
	start(dir, false, os.Getpid(), 2*time.Second)  // not shared
	start(other, true, os.Getpid(), 3*time.Second) // other directory
	start(dir, true, 999999, 4*time.Second)        // not running
	got, ok := ledger.findSharedLaunch("codex", dir)
	if !ok || got.ID != newer.ID {
		t.Fatalf("found %v %v, want newest running shared launch %s (not %s)", got.ID, ok, newer.ID, older.ID)
	}
	if _, ok := ledger.findSharedLaunch("codex", t.TempDir()); ok {
		t.Fatal("matched a launch from another directory")
	}
	if _, ok := ledger.findSharedLaunch("codex", ""); ok {
		t.Fatal("matched without a directory")
	}
}

func TestSessionNotifySharedFlagParses(t *testing.T) {
	hook, err := parseSessionHookArgs([]string{"--shared", "--store-dir", "/s", `{"type":"agent-turn-complete"}`}, sessionNotifyCommand)
	if err != nil || !hook.shared || hook.launchID != "" || hook.storeDir != "/s" || len(hook.rest) != 1 {
		t.Fatalf("hook = %+v, %v", hook, err)
	}
	var payload struct {
		Cwd string `json:"cwd"`
	}
	if json.Unmarshal([]byte(`{"cwd":"/w"}`), &payload) != nil || payload.Cwd != "/w" {
		t.Fatal("notify payload cwd")
	}
}
