//go:build !windows

// Package bakee2e runs the post-upgrade bake gate end to end with real
// binaries: a real `subrouter supervise` on a loopback port, real worker
// generations built from this checkout, real traffic through the public
// listener to a fake upstream, and the real subrouter-deploy.sh,
// subrouter-autoupdate.sh and subrouter-guard.sh with every path pointed into
// a temporary root. Nothing touches launchd, /Library, /usr/local, or a real
// team host; launchctl is a stub that fails the test if the guard calls it.
//
// It builds three workers and takes a few minutes, so it runs only when
// SUBROUTER_BAKE_E2E=1 (CI sets it in the deploy-scripts job):
//
//	SUBROUTER_BAKE_E2E=1 go test ./deploy/macos/tests/bakee2e -count=1 -v
package bakee2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

type harness struct {
	t        *testing.T
	repo     string
	root     string
	sockDir  string
	addr     string
	env      []string
	upstream *httptest.Server
	// upstreamFailures, while positive, makes the fake upstream answer 502
	// and counts down: real upstream noise, not a subrouter regression.
	upstreamFailures atomic.Int64
	supervisor       *exec.Cmd
}

func TestBakeGateEndToEnd(t *testing.T) {
	if os.Getenv("SUBROUTER_BAKE_E2E") != "1" {
		t.Skip("set SUBROUTER_BAKE_E2E=1 to run the real-binary bake gate test")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required by the deploy scripts")
	}
	h := newHarness(t)

	good1 := h.build("v0.0.1", "")
	good2 := h.build("v0.0.2", "")
	bad := h.build("v0.0.3", "subrouter_bakefault")

	h.install(good1, "v0.0.1")
	h.startSupervisor()

	// Baseline traffic on the promoted generation.
	gen := newTrafficGenerator(h, 40*time.Millisecond)
	gen.start()
	defer gen.stop()
	h.waitRequests(150)
	h.logf("baseline traffic: %s", h.trafficSummary())
	if w := gen.windowFailures(); w.status+w.connection > gen.windowTotal()/20 {
		t.Fatalf("the good build is not routing (%s); first failure: %s\n%s", gen.windowSummary(), gen.firstFailure(), h.supervisorTail())
	}

	// A. good -> bad: the bad worker starts, answers health, and fails a third
	// of proxied requests with its own 502. The bake rolls it back and pins.
	h.logf("=== A: upgrade v0.0.1 -> v0.0.3 (broken build) with a 60s bake")
	h.deploy(map[string]string{"SUBROUTER_BAKE_SECONDS": "60"}, "install", bad, "--label", "v0.0.3")
	h.expectHealthVersion("v0.0.3")
	h.expectRelease("baking", "v0.0.3")
	gen.resetWindow()
	state := h.guardUntil(func(s releaseState) bool { return s.State != "baking" }, 60*time.Second)
	if state.State != "rolled_back" || !strings.Contains(state.Reason, "proxy 5xx") {
		t.Fatalf("broken release: state %q reason %q, want rolled_back on proxy 5xx", state.State, state.Reason)
	}
	h.logf("rolled back: %s", state.Reason)
	if !sameFile(t, h.bin(), good1) {
		t.Fatal("broken release: the live worker binary is not the good build after rollback")
	}
	h.expectHealthVersion("v0.0.1")
	h.expectRelease("rolled_back", "v0.0.3")
	pin := h.read("transaction/upgrade-inhibited")
	if !strings.HasPrefix(pin, "pinned at v0.0.1 by subrouter-guard.sh bake gate: rolled back v0.0.3") {
		t.Fatalf("pin after rollback = %q", pin)
	}
	if got := strings.TrimSpace(h.read("etc/subrouter-version")); got != "v0.0.1" {
		t.Fatalf("version marker after rollback = %q, want v0.0.1", got)
	}
	h.expectNoLaunchctl()
	// Autoupdate must not reinstall the rejected release while pinned.
	out := h.autoupdate("v0.0.3", bad)
	if !strings.Contains(out, "worker update deferred: pinned at v0.0.1") {
		t.Fatalf("autoupdate while pinned:\n%s", out)
	}
	if !sameFile(t, h.bin(), good1) {
		t.Fatal("autoupdate replaced the pinned worker")
	}
	h.logf("traffic during bad bake: %s", gen.windowSummary())
	h.expectListenerUp(gen, "bad bake and rollback", 2)
	gen.resetWindow()
	h.waitWindowRequests(gen, 50)
	if w := gen.windowFailures(); w.status != 0 || w.connection != 0 {
		t.Fatalf("traffic after rollback still failing: %s", gen.windowSummary())
	}
	h.logf("traffic after rollback: %s", gen.windowSummary())
	h.deploy(nil, "unpin")

	// B. good -> good: a clean bake promotes the new worker and only then
	// advances last-good.
	h.logf("=== B: upgrade v0.0.1 -> v0.0.2 (good build) with a 20s bake")
	gen.resetWindow()
	h.deploy(map[string]string{"SUBROUTER_BAKE_SECONDS": "20"}, "install", good2, "--label", "v0.0.2")
	h.expectRelease("baking", "v0.0.2")
	h.guard()
	if !sameFile(t, h.lastGood(), good1) {
		t.Fatal("last-good advanced to a worker that is still baking")
	}
	state = h.guardUntil(func(s releaseState) bool { return s.State != "baking" }, 60*time.Second)
	if state.State != "promoted" {
		t.Fatalf("good release: state %q reason %q, want promoted", state.State, state.Reason)
	}
	h.logf("promoted: %s", state.Reason)
	if !sameFile(t, h.lastGood(), good2) || !sameFile(t, h.bin(), good2) {
		t.Fatal("good release: last-good or live worker is not the new build after promotion")
	}
	h.expectRelease("promoted", "v0.0.2")
	h.expectHealthVersion("v0.0.2")
	h.logf("traffic during clean bake: %s", gen.windowSummary())
	if w := gen.windowFailures(); w.status != 0 {
		t.Fatalf("requests failed during a clean upgrade: %s; first failure: %s", gen.windowSummary(), gen.firstFailure())
	}
	h.expectListenerUp(gen, "clean upgrade", 1)

	// C. Low traffic with real upstream noise: a few upstream 502s on a few
	// requests are under the request floor and must not roll back.
	h.logf("=== C: low traffic with upstream noise, upgrade v0.0.2 -> v0.0.1 with a 15s bake")
	gen.stop()
	slow := newTrafficGenerator(h, 700*time.Millisecond)
	slow.start()
	defer slow.stop()
	h.deploy(map[string]string{"SUBROUTER_BAKE_SECONDS": "15"}, "install", good1, "--label", "v0.0.1")
	h.upstreamFailures.Store(4)
	state = h.guardUntil(func(s releaseState) bool { return s.State != "baking" }, 60*time.Second)
	if state.State != "promoted" {
		t.Fatalf("low traffic: state %q reason %q, want promoted", state.State, state.Reason)
	}
	h.logf("low-traffic bake promoted: %s", state.Reason)
	h.logf("low-traffic window: %s", slow.windowSummary())
	if w := slow.windowFailures(); w.status == 0 {
		t.Fatalf("low traffic: the fake upstream noise never reached a client (%s); the scenario proved nothing", slow.windowSummary())
	}
	h.expectListenerUp(slow, "low-traffic upgrade", 1)
	h.expectNoLaunchctl()
	h.logf("guard log:\n%s", h.read("guard.log"))
}

// --- harness -----------------------------------------------------------------

func newHarness(t *testing.T) *harness {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	repo, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Unix socket paths are limited to about 104 bytes on macOS, and the
	// default per-user temp directory there is already most of that.
	base := os.TempDir()
	if len(base) > 40 {
		base = "/tmp"
	}
	root, err := os.MkdirTemp(base, "bakee2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("kept %s for inspection", root)
			return
		}
		_ = os.RemoveAll(root)
	})
	for _, dir := range []string{"bin", "builds", "verify", "etc", "transaction", "home", "state/codex/accounts", "tmp", "releases"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h := &harness{t: t, repo: repo, root: root, sockDir: filepath.Join(root, "tmp")}

	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if h.upstreamFailures.Load() > 0 && h.upstreamFailures.Add(-1) >= 0 {
			http.Error(w, "upstream flake", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_e2e","object":"response","output":[]}`)
	}))
	t.Cleanup(h.upstream.Close)

	// Where `subrouter serve` looks with SUBROUTER_STATE_DIR set, i.e.
	// accounts.DefaultCodexStore().
	if _, _, err := (accounts.CodexStore{Dir: filepath.Join(root, "state", "codex", "accounts")}).AddAPIKey("e2e", "sk-e2e-not-a-real-key"); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.addr = listener.Addr().String()
	_ = listener.Close()

	launchctl := filepath.Join(root, "bin", "launchctl")
	if err := os.WriteFile(launchctl, []byte("#!/bin/sh\necho \"$*\" >>\""+filepath.Join(root, "launchctl.calls")+"\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	health := "http://" + h.addr + "/_subrouter/health"
	h.env = append(os.Environ(),
		"HOME="+filepath.Join(root, "home"),
		"TMPDIR="+h.sockDir,
		"SUBROUTER_STATE_DIR="+filepath.Join(root, "state"),
		"SUBROUTER_BIN="+h.bin(),
		"SUBROUTER_SUPERVISOR_BIN="+filepath.Join(root, "bin", "subrouter-supervisor"),
		"SUBROUTER_LABEL=ai.manaflow.subrouter-e2e",
		"SUBROUTER_PLIST="+filepath.Join(root, "service.plist"),
		"SUBROUTER_CONTROL_SOCKET="+filepath.Join(h.sockDir, "ctl.sock"),
		"SUBROUTER_VERIFY_STATE="+filepath.Join(root, "verify"),
		"SUBROUTER_DEPLOY_STATE="+filepath.Join(root, "verify"),
		"SUBROUTER_LAST_GOOD="+filepath.Join(root, "verify", "subrouter.last-good"),
		"SUBROUTER_VERSION_FILE="+filepath.Join(root, "etc", "subrouter-version"),
		"SUBROUTER_HEALTH_URL="+health,
		"SUBROUTER_UPGRADE_INHIBIT_FILE="+filepath.Join(root, "transaction", "upgrade-inhibited"),
		"SUBROUTER_MUTATION_LOCK_FILE="+filepath.Join(root, "mutation.lock"),
		"SUBROUTER_DEPLOY_LOCK_DIR="+filepath.Join(root, "verify", "deploy.lock"),
		"SUBROUTER_GUARD_LOCK_DIR="+filepath.Join(root, "verify", "guard.lock"),
		"SUBROUTER_LAUNCHCTL="+launchctl,
		"SUBROUTER_RELEASE_STATE="+filepath.Join(root, "verify", "release-state.json"),
		"SUBROUTER_GUARD_HEALTH_WAIT_SECS=20",
		"SUBROUTER_DEPLOY_HEALTH_TIMEOUT_SECS=30",
	)
	return h
}

func (h *harness) logf(format string, args ...any) { h.t.Helper(); h.t.Logf(format, args...) }

func (h *harness) bin() string      { return filepath.Join(h.root, "bin", "subrouter") }
func (h *harness) lastGood() string { return filepath.Join(h.root, "verify", "subrouter.last-good") }

func (h *harness) read(rel string) string {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.root, rel))
	if err != nil {
		return ""
	}
	return string(data)
}

func (h *harness) build(version, tags string) string {
	h.t.Helper()
	out := filepath.Join(h.root, "builds", "subrouter-"+version)
	args := []string{"build", "-o", out, "-ldflags", "-X github.com/manaflow-ai/subrouter/internal/buildversion.version=" + version}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "./cmd/subrouter")
	command := exec.Command("go", args...)
	command.Dir = h.repo
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		h.t.Fatalf("go build %s: %v\n%s", version, err, output)
	}
	return out
}

// install puts the first worker in place the way a fresh host has it: a
// promoted release recorded as last-good.
func (h *harness) install(binary, version string) {
	h.t.Helper()
	copyFile(h.t, binary, h.bin())
	copyFile(h.t, binary, filepath.Join(h.root, "bin", "subrouter-supervisor"))
	copyFile(h.t, binary, h.lastGood())
	if err := os.WriteFile(filepath.Join(h.root, "etc", "subrouter-version"), []byte(version+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) startSupervisor() {
	h.t.Helper()
	command := exec.Command(filepath.Join(h.root, "bin", "subrouter-supervisor"), "supervise",
		"--addr", h.addr,
		"--control-socket", filepath.Join(h.sockDir, "ctl.sock"),
		"--worker-bin", h.bin(),
		"--ready-timeout", "30s",
		"--worker-stop-grace", "5s",
		"--",
		"--upstream", h.upstream.URL,
		"--sessions", filepath.Join(h.root, "state", "sessions.json"),
	)
	command.Env = h.env
	// A file, not a pipe: a worker that outlives the supervisor would hold a
	// pipe open and make Wait hang. Its own process group lets cleanup stop
	// every worker generation with one signal.
	logFile, err := os.Create(filepath.Join(h.root, "supervisor.log"))
	if err != nil {
		h.t.Fatal(err)
	}
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		h.t.Fatalf("start supervisor: %v", err)
	}
	h.supervisor = command
	h.t.Cleanup(func() {
		group := -command.Process.Pid
		_ = syscall.Kill(group, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = command.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}
		_ = syscall.Kill(group, syscall.SIGKILL)
		<-done
		_ = logFile.Close()
		if h.t.Failed() {
			h.t.Logf("supervisor and worker output (tail):\n%s", h.supervisorTail())
		}
	})
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := h.health(); ok {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.t.Fatalf("supervisor never answered health:\n%s", h.supervisorTail())
}

func (h *harness) supervisorTail() string {
	text := h.read("supervisor.log")
	if len(text) > 6000 {
		text = text[len(text)-6000:]
	}
	return text
}

type releaseState struct {
	Version         string `json:"version"`
	PreviousVersion string `json:"previous_version"`
	State           string `json:"state"`
	Reason          string `json:"reason"`
}

type healthBody struct {
	Version string        `json:"version"`
	Release *releaseState `json:"release"`
}

func (h *harness) health() (healthBody, bool) {
	var body healthBody
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + h.addr + "/_subrouter/health")
	if err != nil {
		return body, false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return body, false
	}
	return body, json.NewDecoder(response.Body).Decode(&body) == nil
}

func (h *harness) expectHealthVersion(version string) {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last healthBody
	for time.Now().Before(deadline) {
		if body, ok := h.health(); ok {
			last = body
			if body.Version == version {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.t.Fatalf("health version = %q, want %q", last.Version, version)
}

// expectRelease checks the release object the real worker reads from the
// state file and serves on the public /_subrouter/health.
func (h *harness) expectRelease(state, version string) {
	h.t.Helper()
	body, ok := h.health()
	if !ok || body.Release == nil {
		h.t.Fatalf("health has no release object (ok=%v)", ok)
	}
	if body.Release.State != state || body.Release.Version != version {
		h.t.Fatalf("health release = %+v, want %s %s", *body.Release, state, version)
	}
}

func (h *harness) traffic() map[string]any {
	response, err := http.Get("http://" + h.addr + "/_subrouter/traffic")
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(response.Body).Decode(&body)
	return body
}

func (h *harness) trafficSummary() string {
	body := h.traffic()
	return fmt.Sprintf("version=%v requests=%v responses=%v proxy_5xx=%v upstream_5xx=%v",
		body["version"], body["requests"], body["responses"], body["proxy_5xx"], body["upstream_5xx"])
}

func (h *harness) waitRequests(n float64) {
	h.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := h.traffic()["requests"].(float64); got >= n {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.t.Fatalf("traffic never reached %v requests: %s", n, h.trafficSummary())
}

func (h *harness) waitWindowRequests(g *trafficGenerator, n int64) {
	h.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if g.windowTotal() >= n {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("traffic stalled: %s", g.windowSummary())
}

func (h *harness) script(name string, extra map[string]string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", append([]string{filepath.Join(h.repo, "deploy", "macos", name)}, args...)...)
	command.Env = append([]string{}, h.env...)
	for key, value := range extra {
		command.Env = append(command.Env, key+"="+value)
	}
	output, err := command.CombinedOutput()
	return string(output), err
}

func (h *harness) deploy(extra map[string]string, args ...string) {
	h.t.Helper()
	output, err := h.script("subrouter-deploy.sh", extra, args...)
	h.logf("$ subrouter-deploy.sh %s\n%s", strings.Join(args, " "), strings.TrimSpace(output))
	if err != nil {
		h.t.Fatalf("subrouter-deploy.sh %s: %v", strings.Join(args, " "), err)
	}
}

func (h *harness) guard() {
	h.t.Helper()
	output, err := h.script("subrouter-guard.sh", nil)
	f, openErr := os.OpenFile(filepath.Join(h.root, "guard.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if openErr == nil {
		_, _ = f.WriteString(output)
		_ = f.Close()
	}
	if err != nil {
		h.t.Fatalf("subrouter-guard.sh: %v\n%s", err, output)
	}
}

func (h *harness) state() releaseState {
	var state releaseState
	_ = json.Unmarshal([]byte(h.read("verify/release-state.json")), &state)
	return state
}

// guardUntil runs the guard on a short cadence (launchd runs it every 60s)
// until done is true for the release state.
func (h *harness) guardUntil(done func(releaseState) bool, limit time.Duration) releaseState {
	h.t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		h.guard()
		if state := h.state(); done(state) {
			return state
		}
	}
	h.t.Fatalf("release state still %+v after %s\nguard log:\n%s", h.state(), limit, h.read("guard.log"))
	return releaseState{}
}

// autoupdate runs the real updater against a file:// release that names the
// given worker.
func (h *harness) autoupdate(tag, binary string) string {
	h.t.Helper()
	arch := "amd64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	dir := filepath.Join(h.root, "releases", tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	asset := fmt.Sprintf("subrouter_%s_darwin_%s", strings.TrimPrefix(tag, "v"), arch)
	copyFile(h.t, binary, filepath.Join(dir, asset))
	sum, err := exec.Command("shasum", "-a", "256", filepath.Join(dir, asset)).Output()
	if err != nil {
		h.t.Fatalf("shasum: %v", err)
	}
	digest := strings.Fields(string(sum))[0]
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(digest+"  "+asset+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	latest := filepath.Join(h.root, "releases", "latest.json")
	if err := os.WriteFile(latest, []byte(`{"tag_name":"`+tag+`"}`), 0o644); err != nil {
		h.t.Fatal(err)
	}
	output, err := h.script("subrouter-autoupdate.sh", map[string]string{
		"SUBROUTER_RELEASE_API_URL":      "file://" + latest,
		"SUBROUTER_RELEASE_DOWNLOAD_URL": "file://" + filepath.Join(h.root, "releases"),
	})
	h.logf("$ subrouter-autoupdate.sh (latest %s)\n%s", tag, strings.TrimSpace(output))
	if err != nil {
		h.t.Fatalf("subrouter-autoupdate.sh: %v", err)
	}
	return output
}

func (h *harness) expectNoLaunchctl() {
	h.t.Helper()
	if calls := h.read("launchctl.calls"); calls != "" {
		h.t.Fatalf("the guard fell back to launchctl, which would restart the listener:\n%s", calls)
	}
}

// --- traffic -----------------------------------------------------------------

type failureCounts struct{ status, connection, refused int64 }

// expectListenerUp fails when any request was refused (the public listener
// was closed) and when more requests failed at the connection level than
// generation switches in the window. A keep-alive POST that races the old
// generation closing its idle connection fails once per switch with EOF or a
// reset: that is HTTP/1.1 behaviour of the existing supervisor drain, not a
// closed listener, and every such error is logged verbatim.
func (h *harness) expectListenerUp(g *trafficGenerator, window string, switches int64) {
	h.t.Helper()
	w := g.windowFailures()
	for _, text := range g.connectionErrors() {
		h.logf("%s: connection-level failure: %s", window, text)
	}
	if w.refused != 0 {
		h.t.Fatalf("%s: the public listener refused %d connections", window, w.refused)
	}
	if w.connection > switches {
		h.t.Fatalf("%s: %d connection-level failures across %d generation switch(es)", window, w.connection, switches)
	}
}

type trafficGenerator struct {
	h        *harness
	interval time.Duration
	stopOnce sync.Once
	done     chan struct{}
	wg       sync.WaitGroup

	ok, status, connection, refused atomic.Int64

	mu         sync.Mutex
	first      string
	connErrors []string
}

func newTrafficGenerator(h *harness, interval time.Duration) *trafficGenerator {
	return &trafficGenerator{h: h, interval: interval, done: make(chan struct{})}
}

func (g *trafficGenerator) start() {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 4}}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		ticker := time.NewTicker(g.interval)
		defer ticker.Stop()
		for {
			select {
			case <-g.done:
				return
			case <-ticker.C:
			}
			body := strings.NewReader(`{"model":"gpt-5","input":"bake e2e"}`)
			request, _ := http.NewRequest(http.MethodPost, "http://"+g.h.addr+"/v1/responses", body)
			request.Header.Set("Content-Type", "application/json")
			response, err := client.Do(request)
			if err != nil {
				g.connection.Add(1)
				if strings.Contains(err.Error(), "connection refused") {
					g.refused.Add(1)
				}
				g.noteFailure("connection: " + err.Error())
				g.mu.Lock()
				g.connErrors = append(g.connErrors, time.Now().Format("15:04:05.000")+" "+err.Error())
				g.mu.Unlock()
				continue
			}
			text, _ := io.ReadAll(io.LimitReader(response.Body, 512))
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				g.ok.Add(1)
			} else {
				g.status.Add(1)
				g.noteFailure(fmt.Sprintf("%d %s", response.StatusCode, strings.TrimSpace(string(text))))
			}
		}
	}()
}

func (g *trafficGenerator) noteFailure(text string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.first == "" {
		g.first = text
	}
}

func (g *trafficGenerator) firstFailure() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.first
}

func (g *trafficGenerator) stop() {
	g.stopOnce.Do(func() { close(g.done) })
	g.wg.Wait()
}

func (g *trafficGenerator) resetWindow() {
	g.ok.Store(0)
	g.status.Store(0)
	g.connection.Store(0)
	g.refused.Store(0)
	g.mu.Lock()
	g.connErrors = nil
	g.first = ""
	g.mu.Unlock()
}

func (g *trafficGenerator) windowFailures() failureCounts {
	return failureCounts{status: g.status.Load(), connection: g.connection.Load(), refused: g.refused.Load()}
}

func (g *trafficGenerator) connectionErrors() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.connErrors...)
}

func (g *trafficGenerator) windowTotal() int64 {
	return g.ok.Load() + g.status.Load() + g.connection.Load()
}

func (g *trafficGenerator) windowSummary() string {
	return fmt.Sprintf("%d ok, %d non-200, %d connection errors", g.ok.Load(), g.status.Load(), g.connection.Load())
}

// --- helpers -----------------------------------------------------------------

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to+".tmp", data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(to+".tmp", to); err != nil {
		t.Fatal(err)
	}
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	left, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	right, err := os.ReadFile(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}
