package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestCodexSharedDaemonEligibility(t *testing.T) {
	t.Setenv(codexSharedDaemonDisable, "")
	t.Setenv("SUBROUTER_CODEX_BASE_URL", "")
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
		{"attached profile", []string{"-pmuse-high"}, false},
		{"attached model", []string{"-mgpt-5"}, false},
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
	t.Setenv("SUBROUTER_CODEX_BASE_URL", "http://x/v1")
	if codexSharedDaemonEligible(nil, false, "", "", false, "") {
		t.Error("an explicit base URL has no stable shared home")
	}
	t.Setenv("SUBROUTER_CODEX_BASE_URL", "")
	t.Setenv(codexSharedDaemonDisable, "0")
	if codexSharedDaemonEligible(nil, false, "", "", false, "") {
		t.Error("SUBROUTER_CODEX_SHARED_DAEMON=0 must opt out")
	}
}

type sharedHomeFixture struct {
	t      *testing.T
	source string
	target string
}

func newSharedHomeFixture(t *testing.T) sharedHomeFixture {
	root := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("SUBROUTER_SERVER", "")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	f := sharedHomeFixture{t: t, source: filepath.Join(root, "codex"), target: filepath.Join(root, "state", codexSharedHomeDirName)}
	if err := os.MkdirAll(f.source, 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f sharedHomeFixture) writeUser(body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.source, "config.toml"), []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f sharedHomeFixture) prepare(baseURL string) map[string]any {
	f.t.Helper()
	if err := prepareCodexSharedHome(f.source, f.target, baseURL, []string{"/bin/sr", "__session-notify", "--shared"}); err != nil {
		f.t.Fatal(err)
	}
	return f.shared()
}

func (f sharedHomeFixture) shared() map[string]any {
	f.t.Helper()
	body, err := os.ReadFile(filepath.Join(f.target, "config.toml"))
	if err != nil {
		f.t.Fatal(err)
	}
	table, err := decodeTomlTable(body)
	if err != nil {
		f.t.Fatalf("generated config is not valid TOML: %v\n%s", err, body)
	}
	return table
}

// codexSaves edits the shared config the way Codex does from inside sr codex.
func (f sharedHomeFixture) codexSaves(edit func(map[string]any)) {
	f.t.Helper()
	table := f.shared()
	edit(table)
	var out strings.Builder
	if err := toml.NewEncoder(&out).Encode(table); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.target, "config.toml"), []byte(out.String()), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func TestCodexSharedHomeConfigRoutesThroughSubrouter(t *testing.T) {
	f := newSharedHomeFixture(t)
	f.writeUser(`model = "gpt-6"
model_provider = "openai"
chatgpt_base_url = "http://127.0.0.1:31415/backend-api"
developer_instructions = """
[not a table](https://example.com)
"""

[model_providers]
subrouter = { name = "stale", base_url = "http://stale/v1" }
other = { name = "Other", base_url = "http://other/v1" }

[mcp_servers.docs]
url = "https://example.com/mcp"

  [projects."/Users/u/src"] # indented, with a comment
  trust_level = "trusted"
`)
	got := f.prepare("http://127.0.0.1:31415/v1")
	provider := getTomlPath(got, []string{"model_providers", "subrouter"}).(map[string]any)
	if got["model_provider"] != "subrouter" || provider["base_url"] != "http://127.0.0.1:31415/v1" ||
		provider["experimental_bearer_token"] != "subrouter" || provider["supports_websockets"] != true {
		t.Fatalf("provider not routed through Subrouter: %v", got)
	}
	for path, want := range map[string]any{
		"model":            "gpt-6",
		"chatgpt_base_url": "http://127.0.0.1:31415/backend-api",
	} {
		if got[path] != want {
			t.Errorf("%s = %v, want the user's %v", path, got[path], want)
		}
	}
	if getTomlPath(got, []string{"model_providers", "other", "name"}) != "Other" ||
		getTomlPath(got, []string{"mcp_servers", "docs", "url"}) == nil ||
		getTomlPath(got, []string{"projects", "/Users/u/src", "trust_level"}) != "trusted" ||
		!strings.Contains(got["developer_instructions"].(string), "[not a table]") {
		t.Fatalf("user settings lost: %v", got)
	}
	if got["allow_symlinked_codex_home"] != true {
		t.Fatal("symlinked writable roots must be allowed for the linked home")
	}
	if notify, _ := got["notify"].([]any); len(notify) != 3 || notify[1] != "__session-notify" {
		t.Fatalf("notify = %v", got["notify"])
	}
}

func TestCodexSharedHomeKeepsWhatCodexSavesAndFollowsTheUser(t *testing.T) {
	f := newSharedHomeFixture(t)
	f.writeUser(`model = "gpt-6"
[projects."/repo"]
trust_level = "trusted"
[notice]
hide_full_access_warning = false
`)
	f.prepare("http://a/v1")
	// Inside sr codex: /model, a dismissed notice, trust for a new project,
	// and an attempt to change the provider.
	f.codexSaves(func(table map[string]any) {
		table["model"] = "gpt-6-mini"
		setTomlPath(table, []string{"notice", "hide_full_access_warning"}, true)
		setTomlPath(table, []string{"projects", "/new", "trust_level"}, "trusted")
		table["model_provider"] = "openai"
	})
	// Meanwhile the user revokes /repo in their own config and moves server.
	f.writeUser(`model = "gpt-6"
[notice]
hide_full_access_warning = false
`)
	got := f.prepare("http://b/v1")
	if got["model"] != "gpt-6-mini" || getTomlPath(got, []string{"notice", "hide_full_access_warning"}) != true ||
		getTomlPath(got, []string{"projects", "/new", "trust_level"}) != "trusted" {
		t.Fatalf("settings Codex saved inside sr codex were lost: %v", got)
	}
	if getTomlPath(got, []string{"projects", "/repo"}) != nil {
		t.Fatal("trust the user revoked stayed in the shared home")
	}
	if got["model_provider"] != "subrouter" || getTomlPath(got, []string{"model_providers", "subrouter", "base_url"}) != "http://b/v1" {
		t.Fatalf("routing must always come from the generation: %v", got)
	}

	// A later change in the user's own config wins over Codex's saved value.
	f.writeUser(`model = "gpt-7"
[notice]
hide_full_access_warning = false
`)
	if got := f.prepare("http://b/v1"); got["model"] != "gpt-7" {
		t.Fatalf("model = %v, want the user's new choice", got["model"])
	}
}

func TestCodexSharedHomeRekeysHookTrust(t *testing.T) {
	f := newSharedHomeFixture(t)
	f.writeUser(fmt.Sprintf(`[hooks.state.%q]
trusted_hash = "sha256:aa"
[hooks.state."/elsewhere/hooks.json:stop:0:0"]
trusted_hash = "sha256:bb"
`, filepath.Join(f.source, "hooks.json")+":pre_tool_use:0:0"))
	got := f.prepare("http://a/v1")
	state := getTomlPath(got, []string{"hooks", "state"}).(map[string]any)
	want := filepath.Join(canonicalPath(f.target), "hooks.json") + ":pre_tool_use:0:0"
	if state[want] == nil || state["/elsewhere/hooks.json:stop:0:0"] == nil || len(state) != 2 {
		t.Fatalf("hook trust keys = %v, want %q and the unrelated key", state, want)
	}
}

func TestCodexSharedHomeKeepsUserNotify(t *testing.T) {
	f := newSharedHomeFixture(t)
	f.writeUser("notify = [\"mine\"]\n")
	if got := f.prepare("http://a/v1"); !reflect.DeepEqual(got["notify"], []any{"mine"}) {
		t.Fatalf("the user's notify program must win: %v", got["notify"])
	}
}

func TestCodexSharedHomeRejectsInvalidUserConfig(t *testing.T) {
	f := newSharedHomeFixture(t)
	f.writeUser("model = \n")
	if err := prepareCodexSharedHome(f.source, f.target, "http://a/v1", nil); err == nil {
		t.Fatal("an unparseable config must fall back to the per-launch path")
	}
}

func TestCodexSharedHomeRefusesToMirrorItself(t *testing.T) {
	f := newSharedHomeFixture(t)
	if err := prepareCodexSharedHome(f.target, f.target, "http://a/v1", nil); err == nil {
		t.Fatal("mirroring the shared home into itself must fail")
	}
	// A nested sr codex sees CODEX_HOME set to the shared home.
	f.prepare("http://a/v1")
	t.Setenv("CODEX_HOME", f.target)
	if got, err := codexSourceHome(); err != nil || canonicalPath(got) != canonicalPath(f.source) {
		t.Fatalf("nested source = %q %v, want %q", got, err, f.source)
	}
}

func TestCodexSharedHomeDirIsKeyedBySourceAndServer(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("SUBROUTER_SERVER", "")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	def := codexSharedHomeDir(filepath.Join(root, ".codex"))
	if filepath.Base(def) != codexSharedHomeDirName {
		t.Fatalf("default home dir = %s", def)
	}
	other := codexSharedHomeDir(filepath.Join(root, "other-codex"))
	t.Setenv("SUBROUTER_CODEX_SERVER", "staging")
	staging := codexSharedHomeDir(filepath.Join(root, ".codex"))
	if other == def || staging == def || staging == other {
		t.Fatalf("homes collide: %s %s %s", def, other, staging)
	}
}

func TestConcurrentSharedHomePreparesStayValid(t *testing.T) {
	f := newSharedHomeFixture(t)
	f.writeUser("model = \"m\"\n")
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			errs <- prepareCodexSharedHome(f.source, f.target, fmt.Sprintf("http://s%d/v1", i), nil)
		}(i)
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := f.shared(); got["model_provider"] != "subrouter" {
		t.Fatalf("config after concurrent prepares: %v", got)
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
	if err := os.MkdirAll(filepath.Join(target, "worktrees", "w1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, ".tmpAB12"), []byte("tmp"), 0o600); err != nil {
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
	// A directory Codex created only here moves into the source; files
	// (databases, atomic-write temporaries) stay put.
	if dest, err := os.Readlink(filepath.Join(target, "worktrees")); err != nil || dest != filepath.Join(source, "worktrees") {
		t.Errorf("new directory not adopted into the source: %q %v", dest, err)
	}
	if _, err := os.Stat(filepath.Join(source, "worktrees", "w1")); err != nil {
		t.Error("adopted directory lost its content")
	}
	for _, name := range []string{"state_5.sqlite", ".tmpAB12"} {
		if info, err := os.Lstat(filepath.Join(target, name)); err != nil || !info.Mode().IsRegular() {
			t.Errorf("%s must stay a private file: %v %v", name, info, err)
		}
		if _, err := os.Lstat(filepath.Join(source, name)); !os.IsNotExist(err) {
			t.Errorf("%s was moved into the source", name)
		}
	}
	// A newer release in the user's home is followed.
	newer := filepath.Join(source, "packages", "app-server-daemon", "releases", "2.0")
	if err := os.MkdirAll(filepath.Join(newer, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newer, "bin", "codex"), []byte("bin2"), 0o700); err != nil {
		t.Fatal(err)
	}
	userCurrent := filepath.Join(source, "packages", "app-server-daemon", "current")
	if err := os.Remove(userCurrent); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newer, userCurrent); err != nil {
		t.Fatal(err)
	}
	if err := prepareCodexSharedHome(source, target, "http://moved/v1", nil); err != nil {
		t.Fatal(err)
	}
	resolvedNewer, _ := filepath.EvalSymlinks(newer)
	if dest, _ := filepath.EvalSymlinks(filepath.Join(target, "packages", "app-server-daemon", "current")); dest != resolvedNewer {
		t.Fatalf("daemon package = %q, want the user's upgraded release", dest)
	}
	if body, _ := os.ReadFile(config); !strings.Contains(string(body), `base_url = "http://moved/v1"`) {
		t.Errorf("base URL not refreshed:\n%s", body)
	}
}

func TestCodexBareLaunchUsesSharedHomeWithRecoveryOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(home, ".subrouter"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "user-codex"))
	t.Setenv(codexSharedDaemonDisable, "")
	t.Setenv("SUBROUTER_CODEX_BASE_URL", "")
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
	shared := codexSharedHomeDir(filepath.Join(home, "user-codex"))
	if got := string(body); got != "args:fix it -c features.goals=true -c model_providers.subrouter.http_headers.X-Subrouter-Capacity-Retry=\"persist\" -c model_providers.subrouter.http_headers.X-Subrouter-Capacity-Retryable=\"1\" -c model_providers.subrouter.request_max_retries=100 -c model_providers.subrouter.stream_max_retries=100\nhome:"+shared+"\n" {
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
	got, ok := ledger.findSharedLaunch("codex", "thread-new", dir)
	if !ok || got.ID != newer.ID {
		t.Fatalf("found %v %v, want newest running shared launch %s (not %s)", got.ID, ok, newer.ID, older.ID)
	}
	// A thread already linked to the older window stays with it.
	if err := ledger.linkLaunchSession(older.ID, "thread-old"); err != nil {
		t.Fatal(err)
	}
	if got, ok := ledger.findSharedLaunch("codex", "thread-old", dir); !ok || got.ID != older.ID {
		t.Fatalf("linked thread went to %v, want %s", got.ID, older.ID)
	}
	if _, ok := ledger.findSharedLaunch("codex", "thread-x", t.TempDir()); ok {
		t.Fatal("matched a launch from another directory")
	}
	if _, ok := ledger.findSharedLaunch("codex", "thread-x", ""); ok {
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
