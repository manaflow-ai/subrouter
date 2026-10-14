package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// fakeAutoResumeCmux writes a cmux stand-in that types `send` text into fifo.
func fakeAutoResumeCmux(t *testing.T, fifo string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cmux")
	script := `#!/bin/sh
case "$1" in
  read-screen) echo '$ sr auto-resume test' ;;
  sessions) echo '{"sessions":[]}' ;;
  send) printf '%s' "$4" > "` + fifo + `" ;;
  send-key) [ "$4" = enter ] && printf '\n' > "` + fifo + `" ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func autoResumeTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	server := httptest.NewServer(proxy.Server{
		AutoResumeSettingPath: filepath.Join(dir, "auto-resume.json"),
		Recovery:              proxy.NewRecoveryTracker(),
	}.Handler())
	t.Cleanup(server.Close)
	return server
}

// `sr auto-resume test` proves the whole path on a Mac: the proxy reports a
// recovered test session and the resumer types into the tab running the test.
func TestAutoResumeTestTypesIntoThisTab(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	server := autoResumeTestServer(t)
	fifo := filepath.Join(t.TempDir(), "tab")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// O_RDWR keeps the open from blocking until the fake cmux writes.
	tab, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tab.Close()
	cmux := fakeAutoResumeCmux(t, fifo)
	var out bytes.Buffer
	runner := srRunner{program: "sr", in: tab, out: &out, errOut: &out}

	if err := runner.runAutoResumeTest(server.URL, cmux, "surface-1", "claude"); err == nil || !strings.Contains(err.Error(), "is off") {
		t.Fatalf("test with auto-resume off: err = %v, want it to say the switch is off", err)
	}
	if err := postAutoResume(server.URL, "/_subrouter/auto-resume", map[string]any{"agent": "claude", "enabled": true}, nil); err != nil {
		t.Fatal(err)
	}
	if err := runner.runAutoResumeTest(server.URL, cmux, "surface-1", "claude"); err != nil {
		t.Fatalf("test: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), `PASS: this tab received "continue"`) {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestAutoResumeSettingIsPoolWide(t *testing.T) {
	server := autoResumeTestServer(t)
	if err := postAutoResume(server.URL, "/_subrouter/auto-resume", map[string]any{"agent": "codex", "enabled": true}, nil); err != nil {
		t.Fatal(err)
	}
	scope := fleetWakeScope(server.URL)
	for agent, want := range map[string]bool{"codex": true, "claude": false} {
		if got, err := scope.enabled(agent); err != nil || got != want {
			t.Fatalf("%s enabled = %v, %v; want %v", agent, got, err, want)
		}
	}
	// A pool without auto-resume support says so instead of reading as off.
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	if _, err := fleetWakeScope(old.URL).enabled("claude"); err == nil || !strings.Contains(err.Error(), "update it") {
		t.Fatalf("old server: err = %v", err)
	}
}

// One resumer acts per Mac; a newer one that is still running takes over,
// and a recorded process that has exited is ignored.
func TestResumerYieldsOnlyToALiveNewerVersion(t *testing.T) {
	want := filepath.Join(t.TempDir(), "resumer.want")
	write := func(version, pid int) {
		if err := os.WriteFile(want, []byte(strconv.Itoa(version)+" "+strconv.Itoa(pid)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(resumerProtocolVersion+1, os.Getppid())
	if !newerResumerWanted(want) {
		t.Fatal("a running newer resumer should take over")
	}
	claimResumerWant(want)
	if v, _ := readResumerWant(want); v != resumerProtocolVersion+1 {
		t.Fatal("an older resumer must not replace a running newer one's claim")
	}
	write(resumerProtocolVersion+1, 999999)
	if newerResumerWanted(want) {
		t.Fatal("a newer resumer that has exited must not block this one")
	}
	claimResumerWant(want)
	if v, pid := readResumerWant(want); v != resumerProtocolVersion || pid != os.Getpid() {
		t.Fatalf("claim = %d %d, want this process", v, pid)
	}
}

// "Why did that happen?" has an answer: rules list their reasons, and why
// names the rule behind each answered prompt and flags prompts still waiting.
func TestAutoResumeRulesAndWhyExplainActions(t *testing.T) {
	server := autoResumeTestServer(t)
	t.Setenv("SUBROUTER_SERVER", "")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	var out bytes.Buffer
	runner := srRunner{program: "sr", out: &out, errOut: &out}
	if err := postAutoResume(server.URL, "/_subrouter/auto-resume/prompts", proxy.PromptReport{Host: "m1", Agent: "claude", SurfaceID: "s1", Question: "Do you want to overwrite notes.md?", AnsweredBy: "Claude saving its own memory notes"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := postAutoResume(server.URL, "/_subrouter/auto-resume/prompts", proxy.PromptReport{Host: "m5", Agent: "claude", SurfaceID: "s2", Question: "Do you want to proceed?"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := runner.autoResumeWhyAt(server.URL); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`answered "Do you want to overwrite notes.md?" in m1`, "because: Claude saving its own memory notes", `WAITING ON YOU: "Do you want to proceed?" in m5`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("why output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := runner.autoResumeRulesAt(server.URL, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "why: Claude saving its own memory notes") {
		t.Fatalf("rules output:\n%s", out.String())
	}
}
