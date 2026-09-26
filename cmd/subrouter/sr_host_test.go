package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/buildversion"
)

// hostFakeHost plays the SSH host and launchctl for sr host tests.
type hostFakeHost struct {
	mu        sync.Mutex
	calls     [][]string
	stdins    []string
	reachable map[string]bool // pool roots the host reaches directly
	portBusy  bool            // something already answers on the host port
	loaded    bool            // the tunnel LaunchAgent is loaded
}

func (h *hostFakeHost) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return h.RunWithEnv(ctx, name, args, nil, stdin, stdout, stderr)
}

func (h *hostFakeHost) RunWithEnv(_ context.Context, name string, args []string, _ []string, stdin io.Reader, stdout, _ io.Writer) error {
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, append([]string{name}, args...))
	h.stdins = append(h.stdins, string(in))
	switch name {
	case "launchctl":
		switch args[0] {
		case "bootstrap":
			h.loaded = true
		case "bootout":
			h.loaded = false
		}
		return nil
	case "ssh":
		script := args[len(args)-1]
		if strings.Contains(script, "/_subrouter/health") {
			if h.probeOK(script) {
				io.WriteString(stdout, "health=ok\n")
			} else {
				io.WriteString(stdout, "health=down\n")
			}
		}
		return nil
	}
	return errors.New("unexpected command " + name)
}

func (h *hostFakeHost) probeOK(script string) bool {
	if strings.Contains(script, "http://127.0.0.1:") {
		return h.loaded || h.portBusy
	}
	for root, ok := range h.reachable {
		if ok && strings.Contains(script, root+"/_subrouter/health") {
			return true
		}
	}
	return false
}

func (h *hostFakeHost) Output(_ context.Context, name string, args []string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, append([]string{name}, args...))
	h.stdins = append(h.stdins, "")
	if name == "launchctl" && args[0] == "print" && h.loaded {
		return []byte("state = running"), nil
	}
	return nil, errors.New("not found")
}

func (h *hostFakeHost) commands(name, sub string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if c[0] == name && (sub == "" || (len(c) > 1 && c[1] == sub)) {
			n++
		}
	}
	return n
}

func (h *hostFakeHost) sshScripts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, c := range h.calls {
		if c[0] == "ssh" {
			out = append(out, c[len(c)-1])
		}
	}
	return out
}

func setupHostTest(t *testing.T, serverURL string) (srRunner, *hostFakeHost, *bytes.Buffer, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_SERVER", "")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(home, "cloud.json"))
	store := accounts.DefaultCodexStore()
	if err := os.MkdirAll(store.StoreDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	servers := `{"servers":[{"name":"lawrence","url":"` + serverURL + `"}],"default":"lawrence"}`
	if err := os.WriteFile(filepath.Join(store.StoreDir(), "servers.json"), []byte(servers), 0o600); err != nil {
		t.Fatal(err)
	}
	agents := filepath.Join(home, "Library", "LaunchAgents")
	oldAgents, oldLogs, oldGOOS, oldInfo := hostLaunchAgentsDir, hostLogsDir, hostGOOS, hostBuildInfo
	hostLaunchAgentsDir = func() (string, error) { return agents, nil }
	hostLogsDir = func() (string, error) { return filepath.Join(home, "Library", "Logs"), nil }
	hostGOOS = "darwin"
	hostBuildInfo = func() buildversion.Info { return buildversion.Info{Version: "v0.1.133", Commit: "abc1234"} }
	t.Cleanup(func() {
		hostLaunchAgentsDir, hostLogsDir, hostGOOS, hostBuildInfo = oldAgents, oldLogs, oldGOOS, oldInfo
	})
	fake := &hostFakeHost{reachable: map[string]bool{}}
	var out bytes.Buffer
	return srRunner{store: store, out: &out, errOut: &out, cmd: fake}, fake, &out, agents
}

func TestHostAttachFallsBackToTunnelForLoopbackPool(t *testing.T) {
	runner, fake, out, agents := setupHostTest(t, "http://127.0.0.1:31415")
	if err := runner.run(context.Background(), []string{"host", "attach", "big-red"}); err != nil {
		t.Fatalf("attach: %v\n%s", err, out)
	}
	plist, err := os.ReadFile(filepath.Join(agents, "ai.manaflow.subrouter.host.big-red.plist"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>127.0.0.1:31415:127.0.0.1:31415</string>", "<string>big-red</string>", "<string>ControlPath=none</string>"} {
		if !strings.Contains(string(plist), want) {
			t.Fatalf("plist missing %s:\n%s", want, plist)
		}
	}
	if fake.commands("launchctl", "bootstrap") != 1 {
		t.Fatalf("expected one bootstrap, calls: %v", fake.calls)
	}
	scripts := strings.Join(fake.sshScripts(), "\n---\n")
	if !strings.Contains(scripts, "kind='\"'\"'release'\"'\"'") || !strings.Contains(scripts, "v0.1.133") {
		t.Fatalf("install did not match this machine's release:\n%s", scripts)
	}
	if !strings.Contains(scripts, "server") || !strings.Contains(scripts, "http://127.0.0.1:31415") {
		t.Fatalf("host server entry not written:\n%s", scripts)
	}
	state, err := runner.hostStore().load()
	if err != nil {
		t.Fatal(err)
	}
	h, ok := state.find("big-red")
	if !ok || h.Route != hostRouteTunnel || h.Server != "lawrence" || h.URL != "http://127.0.0.1:31415" {
		t.Fatalf("state = %+v", state)
	}
	if !strings.Contains(out.String(), "reverse tunnel") {
		t.Fatalf("output does not say it is a tunnel:\n%s", out)
	}

	// A second attach keeps the running tunnel.
	if err := runner.run(context.Background(), []string{"host", "attach", "big-red"}); err != nil {
		t.Fatalf("re-attach: %v\n%s", err, out)
	}
	if fake.commands("launchctl", "bootstrap") != 1 || fake.commands("launchctl", "bootout") != 0 {
		t.Fatalf("re-attach reloaded the tunnel: %v", fake.calls)
	}
}

func TestHostAttachPrefersDirectRoute(t *testing.T) {
	runner, fake, out, agents := setupHostTest(t, "http://127.0.0.1:31415")
	fake.reachable["http://100.89.225.106:31415"] = true
	err := runner.run(context.Background(), []string{"host", "attach", "big-red", "--url", "http://10.0.0.9:31415", "--url", "http://100.89.225.106:31415/v1"})
	if err != nil {
		t.Fatalf("attach: %v\n%s", err, out)
	}
	if fake.commands("launchctl", "") != 0 {
		t.Fatalf("direct route touched launchd: %v", fake.calls)
	}
	if _, err := os.Stat(agents); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("direct route wrote a LaunchAgent")
	}
	h, _ := mustLoadHosts(t, runner).find("big-red")
	if h.Route != hostRouteDirect || h.URL != "http://100.89.225.106:31415" {
		t.Fatalf("host = %+v", h)
	}
}

func TestHostAttachDirectOnlyFailsWithoutRoute(t *testing.T) {
	runner, fake, _, _ := setupHostTest(t, "http://127.0.0.1:31415")
	err := runner.run(context.Background(), []string{"host", "attach", "big-red", "--route", "direct"})
	if err == nil || !strings.Contains(err.Error(), "--url") {
		t.Fatalf("err = %v", err)
	}
	if fake.commands("launchctl", "") != 0 {
		t.Fatalf("direct-only attach fell back to a tunnel")
	}
}

func TestHostAttachRefusesBusyHostPort(t *testing.T) {
	runner, fake, _, _ := setupHostTest(t, "http://127.0.0.1:31415")
	fake.portBusy = true
	err := runner.run(context.Background(), []string{"host", "attach", "big-red"})
	if err == nil || !strings.Contains(err.Error(), "already answers") {
		t.Fatalf("err = %v", err)
	}
	if fake.commands("launchctl", "bootstrap") != 0 {
		t.Fatalf("started a tunnel over a busy port")
	}
}

func TestHostAttachTunnelRefusesHTTPS(t *testing.T) {
	runner, _, _, _ := setupHostTest(t, "https://pool.example.com")
	err := runner.run(context.Background(), []string{"host", "attach", "big-red"})
	if err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Fatalf("err = %v", err)
	}
}

func TestHostDetachStopsTunnelAndForgetsHost(t *testing.T) {
	runner, fake, out, agents := setupHostTest(t, "http://127.0.0.1:31415")
	if err := runner.run(context.Background(), []string{"host", "attach", "big-red"}); err != nil {
		t.Fatalf("attach: %v\n%s", err, out)
	}
	if err := runner.run(context.Background(), []string{"host", "detach", "big-red"}); err != nil {
		t.Fatalf("detach: %v\n%s", err, out)
	}
	if fake.loaded {
		t.Fatalf("tunnel still loaded")
	}
	if _, err := os.Stat(filepath.Join(agents, "ai.manaflow.subrouter.host.big-red.plist")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist left behind")
	}
	scripts := fake.sshScripts()
	if last := scripts[len(scripts)-1]; !strings.Contains(last, "remove") || !strings.Contains(last, "lawrence") {
		t.Fatalf("detach did not remove the host entry: %s", last)
	}
	if hosts, _ := mustLoadHosts(t, runner).find("big-red"); hosts.SSHHost != "" {
		t.Fatalf("host still recorded")
	}
	// Detaching again is a no-op.
	if err := runner.run(context.Background(), []string{"host", "detach", "big-red"}); err != nil {
		t.Fatal(err)
	}
}

func TestHostStatusReportsRouteAndHealth(t *testing.T) {
	runner, _, out, _ := setupHostTest(t, "http://127.0.0.1:31415")
	if err := runner.run(context.Background(), []string{"host", "attach", "big-red"}); err != nil {
		t.Fatalf("attach: %v\n%s", err, out)
	}
	out.Reset()
	if err := runner.run(context.Background(), []string{"host", "status"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"big-red: lawrence through this machine's tunnel", "tunnel:  running", "pool:    ok"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status missing %q:\n%s", want, out)
		}
	}
}

func TestHostAttachRejectsLocalStorage(t *testing.T) {
	runner, _, _, _ := setupHostTest(t, "http://127.0.0.1:31415")
	if err := os.WriteFile(os.Getenv("SUBROUTER_CLOUD_CONFIG"), []byte(`{"version":1,"credentialSource":"local"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runner.run(context.Background(), []string{"host", "attach", "big-red"})
	if err == nil || !strings.Contains(err.Error(), "local") {
		t.Fatalf("err = %v", err)
	}
}

func TestHostAttachRejectsOptionLikeHost(t *testing.T) {
	runner, _, _, _ := setupHostTest(t, "http://127.0.0.1:31415")
	for _, host := range []string{"-oProxyCommand=x", "a b", "x;y"} {
		if err := runner.run(context.Background(), []string{"host", "attach", "--", host}); err == nil {
			t.Fatalf("accepted host %q", host)
		}
	}
}

func mustLoadHosts(t *testing.T, runner srRunner) attachedHostsFile {
	t.Helper()
	file, err := runner.hostStore().load()
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestHostInstallSpecFor(t *testing.T) {
	release := buildversion.Info{Version: "v0.1.133", Commit: "0123abcd"}
	mainBuild := buildversion.Info{Version: "main-a6cf5e87a63f", Commit: "a6cf5e87a63f"}
	dirty := buildversion.Info{Version: "main-a6cf5e87a63f", Commit: "a6cf5e87a63f", Modified: true}
	cases := []struct {
		requested string
		local     buildversion.Info
		want      hostInstallSpec
	}{
		{"auto", release, hostInstallSpec{"release", "v0.1.133"}},
		{"auto", mainBuild, hostInstallSpec{"auto-source", "a6cf5e87a63f"}},
		{"auto", dirty, hostInstallSpec{"release", "latest"}},
		{"latest", mainBuild, hostInstallSpec{"release", "latest"}},
		{"0.1.130", mainBuild, hostInstallSpec{"release", "v0.1.130"}},
		{"main", release, hostInstallSpec{"source", "main"}},
	}
	for _, tc := range cases {
		got, err := hostInstallSpecFor(tc.requested, tc.local)
		if err != nil || got != tc.want {
			t.Fatalf("%s/%+v = %+v, %v; want %+v", tc.requested, tc.local, got, err, tc.want)
		}
	}
	for _, bad := range []string{"$(id)", "a..b", "-x"} {
		if _, err := hostInstallSpecFor(bad, release); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestApprovalURLWatcherOpensOnceAcrossChunks(t *testing.T) {
	var opened []string
	var out bytes.Buffer
	w := &approvalURLWatcher{out: &out, open: func(u string) { opened = append(opened, u) }}
	for _, chunk := range []string{"Approve Subrouter", " at:\n  https://cmux.com/han", "dler/cli?code=x\n", "  https://other\n"} {
		if _, err := io.WriteString(w, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if len(opened) != 1 || opened[0] != "https://cmux.com/handler/cli?code=x" {
		t.Fatalf("opened = %v", opened)
	}
	if !strings.Contains(out.String(), "Approve Subrouter at:") {
		t.Fatalf("output not passed through")
	}
}

// The install script runs on the host; exercise it with sh against stub curl
// and go so a re-run is proven to be a no-op.
func TestHostInstallScriptIsIdempotent(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	stubs := t.TempDir()
	log := filepath.Join(stubs, "calls.log")
	curl := `#!/bin/sh
echo "curl $*" >> ` + shellQuote(log) + `
case "$*" in
  *url_effective*) printf 'https://github.com/manaflow-ai/subrouter/releases/tag/v0.1.133' ;;
  *install.sh*) printf '%s\n' 'mkdir -p "$SUBROUTER_INSTALL_DIR"; printf x > "$SUBROUTER_INSTALL_DIR/subrouter"; chmod +x "$SUBROUTER_INSTALL_DIR/subrouter"; echo "installed $SUBROUTER_VERSION"' ;;
esac
`
	goStub := `#!/bin/sh
echo "go $*" >> ` + shellQuote(log) + `
mkdir -p "$GOBIN"; printf x > "$GOBIN/subrouter"; chmod +x "$GOBIN/subrouter"
`
	for name, body := range map[string]string{"curl": curl, "go": goStub} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(spec hostInstallSpec) string {
		t.Helper()
		cmd := exec.Command("sh", "-c", hostInstallScript(spec))
		cmd.Env = []string{"HOME=" + home, "PATH=" + stubs + ":/usr/bin:/bin"}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("install %v: %v\n%s", spec, err, output)
		}
		return string(output)
	}
	calls := func() string { b, _ := os.ReadFile(log); return string(b) }

	if got := run(hostInstallSpec{"release", "latest"}); !strings.Contains(got, "installed 0.1.133") {
		t.Fatalf("first install: %s", got)
	}
	if got := run(hostInstallSpec{"release", "latest"}); !strings.Contains(got, "already installed") {
		t.Fatalf("second install: %s", got)
	}
	if n := strings.Count(calls(), "install.sh"); n != 1 {
		t.Fatalf("install.sh fetched %d times:\n%s", n, calls())
	}

	run(hostInstallSpec{"auto-source", "a6cf5e87a63f"})
	if got := run(hostInstallSpec{"auto-source", "a6cf5e87a63f"}); !strings.Contains(got, "already installed") {
		t.Fatalf("second source install: %s", got)
	}
	if n := strings.Count(calls(), "go install github.com/manaflow-ai/subrouter/cmd/subrouter@a6cf5e87a63f"); n != 1 {
		t.Fatalf("go install ran %d times:\n%s", n, calls())
	}
	if target, err := os.Readlink(filepath.Join(home, ".local", "bin", "sr")); err != nil || target != "subrouter" {
		t.Fatalf("sr alias = %q, %v", target, err)
	}
}

func TestHostAttachRecordsRouteOnHost(t *testing.T) {
	runner, fake, out, _ := setupHostTest(t, "http://127.0.0.1:31415")
	if err := runner.run(context.Background(), []string{"host", "attach", "big-red"}); err != nil {
		t.Fatalf("attach: %v\n%s", err, out)
	}
	var marker string
	for i, c := range fake.calls {
		if c[0] == "ssh" && strings.Contains(c[len(c)-1], hostAttachMarkerFile) {
			marker = fake.stdins[i]
		}
	}
	for _, want := range []string{`"route":"tunnel"`, `"pool":"lawrence"`, `"url":"http://127.0.0.1:31415"`, `"via":"`} {
		if !strings.Contains(marker, want) {
			t.Fatalf("marker missing %s: %q", want, marker)
		}
	}
}

func TestWithHostRouteNamesTheTunnel(t *testing.T) {
	marker := hostAttachMarker{Pool: "lawrence", Route: hostRouteTunnel, Via: "Air-Blue"}
	live := sessionStatusView{AccountID: "a1", Label: "bob@example.com"}
	line := renderSessionStatus(live, time.Now())
	if got := withHostRoute(line, live, marker, true); got != line+" · via Air-Blue tunnel" {
		t.Fatalf("live = %q", got)
	}
	down := sessionStatusView{Stale: true}
	if got := withHostRoute(renderSessionStatus(down, time.Now()), down, marker, true); got != "sr: pool unreachable · tunnel from Air-Blue is down (asleep or offline?)" {
		t.Fatalf("down = %q", got)
	}
	stale := sessionStatusView{AccountID: "a1", Label: "bob@example.com", Stale: true}
	if got := withHostRoute("sr: bob@example.com · (stale)", stale, marker, true); !strings.HasSuffix(got, "tunnel from Air-Blue is down (asleep or offline?)") {
		t.Fatalf("stale = %q", got)
	}
	direct := hostAttachMarker{Route: hostRouteDirect, Via: "Air-Blue"}
	if got := withHostRoute(line, live, direct, true); got != line {
		t.Fatalf("direct route changed the line: %q", got)
	}
	if got := withHostRoute(line, live, hostAttachMarker{}, false); got != line {
		t.Fatalf("unattached machine changed the line: %q", got)
	}
}

func TestServerHeadingShowsAttachedRoute(t *testing.T) {
	runner, _, _, _ := setupHostTest(t, "http://127.0.0.1:31415")
	server := srServerConfig{Name: "lawrence", URL: "http://127.0.0.1:31415"}
	if got := runner.serverHeading(server); got != "Server: lawrence (http://127.0.0.1:31415)" {
		t.Fatalf("unattached heading = %q", got)
	}
	marker := `{"pool":"lawrence","route":"tunnel","via":"Air-Blue","url":"http://127.0.0.1:31415"}`
	if err := os.WriteFile(filepath.Join(runner.store.StoreDir(), hostAttachMarkerFile), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "Server: lawrence (http://127.0.0.1:31415) · via a reverse tunnel from Air-Blue (down while Air-Blue sleeps)"
	if got := runner.serverHeading(server); got != want {
		t.Fatalf("heading = %q", got)
	}
	other := srServerConfig{Name: "other", URL: "http://127.0.0.1:31415"}
	if got := runner.serverHeading(other); strings.Contains(got, "tunnel") {
		t.Fatalf("marker leaked onto another server: %q", got)
	}
}

func TestExplainHostRouteErrorNamesTheTunnel(t *testing.T) {
	runner, _, _, _ := setupHostTest(t, "http://127.0.0.1:31415")
	refused := errors.New(`Get "http://127.0.0.1:31415/_subrouter/usage-status": dial tcp 127.0.0.1:31415: connect: connection refused`)
	if got := runner.explainHostRouteError(refused); got != refused {
		t.Fatalf("unattached machine changed the error: %v", got)
	}
	marker := `{"pool":"lawrence","route":"tunnel","via":"Air-Blue","url":"http://127.0.0.1:31415"}`
	if err := os.WriteFile(filepath.Join(runner.store.StoreDir(), hostAttachMarkerFile), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runner.explainHostRouteError(refused)
	if !errors.Is(got, refused) || !strings.Contains(got.Error(), "reverse tunnel from Air-Blue, which is down") {
		t.Fatalf("got %v", got)
	}
	other := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	if got := runner.explainHostRouteError(other); got != other {
		t.Fatalf("unrelated error changed: %v", got)
	}
}
