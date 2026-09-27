package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClaudeProxyChildEnvironmentMarksSubrouterResumeCommand(t *testing.T) {
	got := claudeProxyChildEnvironment([]string{
		"PATH=/usr/bin",
		"ANTHROPIC_BASE_URL=https://remote.example",
		"ANTHROPIC_AUTH_TOKEN=old-token",
		"ANTHROPIC_CUSTOM_HEADERS=X-Subrouter-Agent: stale",
		"SUBROUTER_ADMIN_TOKEN=admin-secret",
		"SUBROUTER_STATE_DIR=/private/state",
		subrouterClaudeResumeCommandEnv + "=stale resume marker",
	}, "http://127.0.0.1:31415/v1", "/isolated/profile", "subrouter", "")
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		subrouterClaudeResumeCommandEnv + "=subrouter claude proxy --resume",
		"CLAUDE_CONFIG_DIR=/isolated/profile",
		"PATH=/usr/bin",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("pooled Claude child env missing %q:\n%s", want, joined)
		}
	}
	if strings.Count(joined, subrouterClaudeResumeCommandEnv+"=") != 1 {
		t.Fatalf("stale resume marker was not replaced:\n%s", joined)
	}
	for _, banned := range []string{
		"stale resume marker",
		"https://remote.example",
		"old-token",
		"X-Subrouter-Agent",
		"admin-secret",
		"/private/state",
		"ANTHROPIC_BASE_URL=",
		"ANTHROPIC_AUTH_TOKEN=",
	} {
		if strings.Contains(joined, banned) {
			t.Fatalf("pooled Claude child env retained %q:\n%s", banned, joined)
		}
	}
}

func TestClaudeProxyChildEnvironmentOnlyTrustsKnownLauncherAliases(t *testing.T) {
	for _, test := range []struct {
		launcher string
		want     string
	}{
		{launcher: "sr", want: "sr"},
		{launcher: "subrouter", want: "subrouter"},
		{launcher: " sr ", want: "sr"},
		{launcher: "cx", want: "sr"},
		{launcher: "/tmp/malicious launcher", want: "sr"},
		{launcher: "SR", want: "sr"},
		{launcher: "", want: "sr"},
	} {
		got := strings.Join(claudeProxyChildEnvironment(nil, "http://127.0.0.1:31415/v1", "", test.launcher, ""), "\n")
		if !strings.Contains(got, subrouterClaudeResumeCommandEnv+"="+test.want+" claude proxy --resume") {
			t.Fatalf("launcher %q env = %q", test.launcher, got)
		}
	}
}

func TestClaudeProxyResumeMarkerIsBoundedCommandTextWithoutRouting(t *testing.T) {
	env := claudeProxyChildEnvironment(nil, "https://tenant.example/v1", "/isolated/profile", "sr", "")
	var marker string
	for _, item := range env {
		if value, ok := strings.CutPrefix(item, subrouterClaudeResumeCommandEnv+"="); ok {
			marker = value
		}
	}
	tokens := strings.Fields(marker)
	want := []string{"sr", "claude", "proxy", "--resume"}
	if strings.Join(tokens, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("resume marker tokens = %q, want %q", tokens, want)
	}
	for _, forbidden := range []string{"tenant.example", "/isolated/profile", "http"} {
		if strings.Contains(marker, forbidden) {
			t.Fatalf("resume marker leaked routing detail %q: %q", forbidden, marker)
		}
	}
}

func TestLocalProfileClaudeEnvironmentHasNoProxyResumeMarker(t *testing.T) {
	// `sr claude run <name>` resumes through its local profile, not the pool,
	// so the settings-routed environment alone must not advertise the pooled
	// resume command.
	env := strings.Join(claudeSettingsChildEnvironment([]string{
		"PATH=/usr/bin",
		subrouterClaudeResumeCommandEnv + "=sr claude proxy --resume",
	}, "http://127.0.0.1:31415/v1", "/isolated/profile"), "\n")
	if strings.Contains(env, subrouterClaudeResumeCommandEnv) {
		t.Fatalf("local profile launch environment advertised the pooled resume command:\n%s", env)
	}
}

func TestLaunchProxyClaudeExportsResumeMarkerToChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a Unix shell; Windows coverage is platform-specific")
	}
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)
	binDir := filepath.Join(tempRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(tempRoot, "child-args")
	envPath := filepath.Join(tempRoot, "child-env")
	fakeClaude := "#!/bin/sh\n" +
		`printf '%s\n' "$@" > "` + argsPath + `"` + "\n" +
		`env > "` + envPath + `"` + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-token-must-not-leak")
	t.Setenv("SUBROUTER_ADMIN_TOKEN", "ambient-admin-secret")

	secret := "srt_pooled_launch_secret"
	configDir := filepath.Join(tempRoot, "config")
	var out, errOut bytes.Buffer
	runner := srRunner{program: "sr", out: &out, errOut: &errOut}
	sessionID := "0198f073-0a5b-7000-8000-000000000059"
	if err := runner.launchProxyClaude(t.Context(), []string{"--resume", sessionID}, "https://proxy.example", secret, configDir, "", ""); err != nil {
		t.Fatalf("launchProxyClaude: %v\nstderr: %s", err, errOut.String())
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("child argv was not recorded: %v", err)
	}
	argv := strings.Split(strings.TrimRight(string(args), "\n"), "\n")
	if len(argv) != 4 || argv[0] != "--settings" || argv[2] != "--resume" || argv[3] != sessionID {
		t.Fatalf("child argv = %q, want [--settings <private path> --resume %s]", argv, sessionID)
	}
	if !strings.HasPrefix(filepath.Base(filepath.Dir(argv[1])), claudeSettingsDirPrefix) {
		t.Fatalf("private settings path %q is not under a %s* directory", argv[1], claudeSettingsDirPrefix)
	}

	envBytes, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("child environment was not recorded: %v", err)
	}
	env := string(envBytes)
	if !strings.Contains(env, subrouterClaudeResumeCommandEnv+"=sr claude proxy --resume\n") {
		t.Fatalf("child environment lacks the resume marker:\n%s", redactedEnvNames(env))
	}
	if !strings.Contains(env, "CLAUDE_CONFIG_DIR="+configDir+"\n") {
		t.Fatalf("child environment lacks the pooled config dir:\n%s", redactedEnvNames(env))
	}
	for _, banned := range []string{secret, "ambient-token-must-not-leak", "ambient-admin-secret", "proxy.example", "SUBROUTER_ADMIN_TOKEN="} {
		if strings.Contains(env, banned) {
			t.Fatalf("child environment leaked %q", banned)
		}
	}
}

// redactedEnvNames keeps failure output to variable names so a diagnostic can
// never print a credential value.
func redactedEnvNames(env string) string {
	names := make([]string, 0)
	for _, line := range strings.Split(env, "\n") {
		if name, _, ok := strings.Cut(line, "="); ok {
			names = append(names, name)
		}
	}
	return strings.Join(names, "\n")
}

// A launch pinned with --account must not advertise a resume. The marker is a
// bare pooled `claude proxy --resume`, so replaying it for a pinned session
// could fail over to another account and break the pin's no-failover contract.
func TestClaudeProxyPinnedAccountExportsNoResumeMarker(t *testing.T) {
	pinned := claudeProxyChildEnvironment(nil, "http://127.0.0.1:31415/v1", "/isolated/profile", "sr", "work@example.com")
	for _, item := range pinned {
		if strings.HasPrefix(item, subrouterClaudeResumeCommandEnv+"=") {
			t.Fatalf("pinned launch advertised an unpinned resume: %q", item)
		}
	}
	// The pooled launch on the same inputs still advertises one, so the guard
	// is the pin and not an unrelated regression.
	pooled := claudeProxyChildEnvironment(nil, "http://127.0.0.1:31415/v1", "/isolated/profile", "sr", "")
	found := false
	for _, item := range pooled {
		if strings.HasPrefix(item, subrouterClaudeResumeCommandEnv+"=") {
			found = true
		}
	}
	if !found {
		t.Fatal("pooled launch lost its resume marker")
	}
}

// An inherited marker must never survive into a pinned launch and be mistaken
// for this launch's own advertisement.
func TestClaudeProxyPinnedAccountDropsInheritedResumeMarker(t *testing.T) {
	inherited := []string{subrouterClaudeResumeCommandEnv + "=sr claude proxy --resume"}
	env := claudeProxyChildEnvironment(inherited, "http://127.0.0.1:31415/v1", "", "sr", "work@example.com")
	for _, item := range env {
		if strings.HasPrefix(item, subrouterClaudeResumeCommandEnv+"=") {
			t.Fatalf("inherited marker survived a pinned launch: %q", item)
		}
	}
}
